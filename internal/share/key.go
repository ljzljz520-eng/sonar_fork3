package share

import (
	"fmt"
	"strings"

	"github.com/raskrebs/sonar/internal/daemon/rpc"
	"github.com/raskrebs/sonar/internal/state"
)

// What a share is keyed on.
//
// sonar-relay/docs/SHARE.md, as amended on 2026-09-15:
//
//	(account_id, repo, worktree, service_name)
//
// and, for a project with no `sonar.yaml`, the fallback
//
//	(account_id, install_id, project_root, port)
//
// The account half is the relay's — it comes from the session on the request —
// so this file resolves the other three or four from what the daemon already
// knows about a listening port.
//
// The committed key travels: clone the project onto a second machine, or open a
// second checkout of it, and the same account publishing the same service gets
// the same URL. The fallback key does not: it names this machine, this
// directory and this port, so moving the project gets a new URL and so does a
// dev server that picks 3001 because 3000 was busy. Both clients say so out
// loud, and the honest nudge is `sonar init`.

// target is one listening port resolved into a share key.
type target struct {
	Port int
	// Group is the daemon's group name, for the sentence a person reads. It is
	// not part of the key: group names became `<repo>@<worktree>` when
	// checkouts shipped, so keying on one would store a string with two fields
	// already inside it.
	Group string
	// Image is the container image behind the port, when there is one. Like
	// Group it is for a sentence a person reads — the probe decides whether a
	// port speaks HTTP, and this only says what to name in the refusal.
	Image string

	// The committed key. Empty Repo means the fallback key is in force.
	Repo     string
	Worktree string
	Service  string

	// The fallback key.
	ProjectRoot string
}

// committed reports whether this target names a service from a `sonar.yaml`.
func (t target) committed() bool { return t.Repo != "" }

// worktreeColumn is what goes in the relay's `worktree` column.
//
// It is the group name, not state.Group.Worktree, and the reason is that the
// relay's committed key requires all three columns to be set: a unique index
// treats two empty strings as one row but two NULLs as distinct, so the
// migration predicates its partial index on `repo <> ”` and the daemon must
// never send a key with a hole in it. A main checkout's Group.Worktree is
// exactly such a hole — it is the `<worktree>` half of a `<repo>@<worktree>`
// name, and a main checkout has none.
//
// Sending Group.Worktree-or-the-repo would close the hole and open a worse one:
// a linked worktree whose directory is called `acme`, in a repository called
// `acme`, would key identically to that repository's main checkout and the two
// would share one URL. The group name has no such case. A main checkout's is
// `acme` and a linked worktree's is `acme@acme`, so every checkout of every
// repository is its own row, which is what "two worktrees are two previews"
// means. The column reads `repo=acme, worktree=acme@feature`, which is
// redundant to look at and exactly right to index on.
//
// It still travels, which is the property the committed key exists for: a
// clone of the same repository on another machine produces the same group name
// for the same checkout, because neither half of the name is a path.
func worktreeColumn(g state.Group) string {
	if name := strings.TrimSpace(g.Name); name != "" {
		return name
	}
	return strings.TrimSpace(g.Repo)
}

// resolveTarget turns a selector into the key for a share.
//
// Two failures are the caller's to report and both are ordinary: nothing is
// listening on the port (1100), and a selector that names more than one thing
// (1002). Everything else resolves — a port with no group and no service still
// has a fallback key, because refusing to share a plain `python -m http.server`
// would be refusing the case that sells the feature.
func resolveTarget(snap state.Snapshot, sel rpc.Selector) (target, error) {
	port, err := selectPort(snap, sel)
	if err != nil {
		return target{}, err
	}

	out := target{Port: port.Port}
	if port.Docker != nil {
		out.Image = strings.TrimSpace(port.Docker.Image)
	}
	if port.ProjectRoot != nil {
		out.ProjectRoot = *port.ProjectRoot
	}
	if out.ProjectRoot == "" {
		out.ProjectRoot = port.Cwd
	}
	if port.Group == nil || *port.Group == "" {
		return out, nil
	}
	out.Group = *port.Group

	group, ok := groupNamed(snap, out.Group)
	if !ok {
		return out, nil
	}
	if group.RootDir != nil && *group.RootDir != "" {
		out.ProjectRoot = *group.RootDir
	}

	// No committed config, no committed key. `GroupSource` alone is not the
	// test: a group can be `file` from a compose file, which is not a
	// `sonar.yaml` and names no sonar service.
	service, ok := serviceOn(group, port.Port)
	if !ok || strings.TrimSpace(group.Repo) == "" {
		return out, nil
	}
	out.Repo = strings.TrimSpace(group.Repo)
	out.Worktree = worktreeColumn(group)
	out.Service = service
	return out, nil
}

// selectPort finds the one listening port a selector names.
func selectPort(snap state.Snapshot, sel rpc.Selector) (state.Port, error) {
	if sel.Port == nil && sel.PID == nil {
		return state.Port{}, rpc.NewError(rpc.CodeInvalidSelector,
			"a port or a pid is required", `send {"port": 3000}`)
	}
	if sel.Port != nil && sel.PID != nil {
		return state.Port{}, rpc.NewError(rpc.CodeInvalidSelector,
			"port and pid are mutually exclusive", "send one of them, not both")
	}

	var matches []state.Port
	for _, p := range snap.Ports {
		if !state.IsLocalhost(p.Host) {
			// A share is a tunnel from this machine to this machine's port.
			// Sharing a remote host's port would mean asking that host's
			// daemon, which is a different feature.
			continue
		}
		switch {
		case sel.PID != nil:
			if p.PID == *sel.PID {
				matches = append(matches, p)
			}
		case p.Port == *sel.Port:
			if sel.BindAddress == nil || *sel.BindAddress == "" || p.BindAddress == *sel.BindAddress {
				matches = append(matches, p)
			}
		}
	}

	switch len(matches) {
	case 0:
		return state.Port{}, notListening(sel)
	case 1:
		return matches[0], nil
	}
	// One service that bound both 127.0.0.1 and ::1 is two rows and one
	// target. The tunnel dials "localhost", which reaches either.
	first := matches[0]
	for _, m := range matches[1:] {
		if m.Port != first.Port || m.PID != first.PID {
			return state.Port{}, rpc.Errorf(rpc.CodeAmbiguous,
				"port %d is bound by more than one process; name it with a pid", first.Port)
		}
	}
	return first, nil
}

func notListening(sel rpc.Selector) *rpc.Error {
	what := "that selector"
	switch {
	case sel.Port != nil:
		what = fmt.Sprintf("port %d", *sel.Port)
	case sel.PID != nil:
		what = fmt.Sprintf("pid %d", *sel.PID)
	}
	return rpc.NewError(rpc.CodeTargetNotListening,
		"nothing is listening on "+what,
		"start the service first; a share is a tunnel to something that is already running")
}

func groupNamed(snap state.Snapshot, name string) (state.Group, bool) {
	for _, g := range snap.Groups {
		if state.IsLocalhost(g.Host) && g.Name == name {
			return g, true
		}
	}
	return state.Group{}, false
}

// serviceOn is the `services[].name:` of the `sonar.yaml` entry listening on a
// port. PortActual is checked first because it is where the service actually
// is: a `port: auto` service has no declared port, and one that found 3000 busy
// is on 3001 and is still the same service.
func serviceOn(g state.Group, port int) (string, bool) {
	for _, s := range g.Services {
		if s.PortActual != nil && *s.PortActual == port {
			return s.Name, s.Name != ""
		}
	}
	for _, s := range g.Services {
		if s.Port != nil && *s.Port == port {
			return s.Name, s.Name != ""
		}
	}
	return "", false
}
