// Package runs implements the tagged-runs registry: a small on-disk JSON file
// recording processes spawned via `sonar run`, keyed by PID. Each entry carries
// a caller-supplied tag (and optional stable id) so that `sonar list` can
// attribute listening ports back to whoever started them.
//
// The registry lives next to sonar's config (e.g. ~/.config/sonar/runs.json).
// Writes are serialized with a sidecar lock file and an atomic rename so that
// multiple concurrent `sonar run` invocations don't clobber each other.
//
// A PID alone is not an identity: the OS reuses PIDs, so every entry also
// carries a random start token (in the child's environment) and the process's
// kernel-reported birth time. Pruning and port attribution match that
// identity; a PID that has been reused by a different process is reported as
// stale, never attributed or killed. A file that cannot be parsed or fails
// validation is quarantined (renamed aside) and reported, never silently
// replaced with an empty registry.
package runs

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Entry is a single tagged run, keyed in the registry by its PID.
//
// Group, Name, Cwd, PPID and PortHint arrived with `sonar start` (step 1A.5).
// They are all omitempty, so a file written by an older `sonar run` still
// loads: Tag then stands for both the group and the name, which is exactly the
// migration `sonar run --tag X` -> `sonar start --group X` promises.
//
// Token and Birth are the process identity: a random start token embedded in
// the child and the observed process creation time (RFC3339Nano UTC).
type Entry struct {
	PID       int    `json:"pid"`
	Tag       string `json:"tag"`
	ID        string `json:"id,omitempty"`
	Cmd       string `json:"cmd,omitempty"`
	StartedAt string `json:"startedAt,omitempty"` // RFC3339
	Group     string `json:"group,omitempty"`
	Name      string `json:"name,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	PPID      int    `json:"ppid,omitempty"`
	PortHint  int    `json:"portHint,omitempty"`
	Token     string `json:"token,omitempty"`
	Birth     string `json:"birth,omitempty"` // RFC3339Nano UTC
}

// GroupOf is the group this run attributes its ports to.
func (e Entry) GroupOf() string {
	if e.Group != "" {
		return e.Group
	}
	return e.Tag
}

// NameOf is the service name this run attributes its ports to.
func (e Entry) NameOf() string {
	if e.Name != "" {
		return e.Name
	}
	return e.Tag
}

// Registry is the in-memory view of the on-disk runs file: pid -> entry.
type Registry struct {
	Runs map[int]Entry `json:"runs"`
}

// Path returns the absolute path to the runs registry file. It mirrors the
// config package's layout (~/.config/sonar) without importing it, to avoid a
// dependency cycle and to keep this package self-contained.
func Path() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "sonar", "runs.json")
}

// lockPath returns the sidecar lock file path for the registry.
func lockPath() string {
	return Path() + ".lock"
}

// StaleEntry names an entry pruned because its PID had been reused by a
// different process.
type StaleEntry struct {
	PID   int
	ID    string
	Token string
}

// Report says what happened while loading, beyond a normal clean read.
type Report struct {
	// Quarantined says the file was unreadable, truncated or structurally
	// invalid: it was moved aside rather than treated as empty.
	Quarantined bool
	Path        string
	Backup      string
	Reason      string
	// Stale lists entries whose PID was reused by a different (alive) process.
	Stale []StaleEntry
}

// loadChecked reads path without pruning. A missing file yields an empty
// registry. A truncated, malformed or invalid file is quarantined (renamed
// aside) and reported, never silently treated as empty.
func loadChecked(path string) (*Registry, *Report) {
	rep := &Report{Path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Registry{Runs: map[int]Entry{}}, rep
		}
		// Unreadable (permissions, I/O): quarantine so a later save cannot
		// overwrite a file we could not inspect.
		quarantine(path, rep, "unreadable: "+err.Error())
		return &Registry{Runs: map[int]Entry{}}, rep
	}
	reg := &Registry{Runs: map[int]Entry{}}
	if err := json.Unmarshal(data, reg); err != nil {
		quarantine(path, rep, "parse error: "+err.Error())
		return &Registry{Runs: map[int]Entry{}}, rep
	}
	if reg.Runs == nil {
		return reg, rep
	}
	for key, e := range reg.Runs {
		if reason := invalidEntry(key, e); reason != "" {
			quarantine(path, rep, reason)
			return &Registry{Runs: map[int]Entry{}}, rep
		}
	}
	return reg, rep
}

// invalidEntry validates one record structurally.
func invalidEntry(key int, e Entry) string {
	if e.PID <= 0 {
		return fmt.Sprintf("invalid entry at pid %d: pid field is %d", key, e.PID)
	}
	if e.PID != key {
		return fmt.Sprintf("invalid entry at pid %d: pid field disagrees (%d)", key, e.PID)
	}
	if e.StartedAt != "" {
		if _, err := time.Parse(time.RFC3339, e.StartedAt); err != nil {
			return fmt.Sprintf("invalid entry at pid %d: unparseable startedAt %q", key, e.StartedAt)
		}
	}
	if e.Birth != "" && ParseTime(e.Birth).IsZero() {
		return fmt.Sprintf("invalid entry at pid %d: unparseable birth %q", key, e.Birth)
	}
	return ""
}

// quarantine moves path aside to a timestamped backup and fills the report.
func quarantine(path string, rep *Report, reason string) {
	var b [4]byte
	_, _ = rand.Read(b[:])
	backup := fmt.Sprintf("%s.corrupt-%s-%s", path,
		time.Now().UTC().Format("20060101T150405.000000000"), hex.EncodeToString(b[:]))
	if err := os.Rename(path, backup); err != nil {
		backup = "" // leave the damaged file in place; the reason is still reported
	}
	rep.Quarantined, rep.Backup, rep.Reason = true, backup, reason
}

// LoadChecked reads the registry and reconciles it with the live system:
// entries whose process is gone or whose identity no longer matches (a reused
// PID) are pruned and written back so the file self-heals. Persistence
// failures are ignored.
func LoadChecked() (*Registry, *Report) {
	fast, rep := loadChecked(Path())
	// The fast read already isolated the file: do not read it again under
	// the lock, or the now-missing file would look like a clean, empty one.
	if rep.Quarantined {
		return fast, rep
	}
	needLock := false
	for pid, e := range fast.Runs {
		if Verify(pid, e.Token, ParseTime(e.Birth)) != StatusAlive {
			needLock = true
			break
		}
	}
	if !needLock {
		return fast, rep
	}

	out := fast
	_ = withLock(func() error {
		fresh, locked := loadChecked(Path())
		// A file replaced by something invalid under the lock updates the
		// report; a missing file (ours moved moments ago) must not erase it.
		if locked.Quarantined {
			rep.Quarantined, rep.Backup, rep.Reason = true, locked.Backup, locked.Reason
		}
		changed := false
		for pid, e := range fresh.Runs {
			switch Verify(pid, e.Token, ParseTime(e.Birth)) {
			case StatusDead:
				delete(fresh.Runs, pid)
				changed = true
			case StatusStale:
				rep.Stale = append(rep.Stale, StaleEntry{PID: pid, ID: e.ID, Token: e.Token})
				delete(fresh.Runs, pid)
				changed = true
			}
		}
		if changed {
			_ = save(fresh)
		}
		out = fresh
		return nil
	})
	return out, rep
}

// Load reads the registry and prunes dead/non-matching entries (stale after a
// crash, hard kill or PID reuse). Pruned entries are written back to disk so
// the file self-heals; quarantine and stale reports are discarded by callers
// that only want the registry.
func Load() *Registry {
	reg, _ := LoadChecked()
	return reg
}

// LoadRaw reads the file with validation and quarantine but without pruning
// entries against the live system and without writing it back. Callers that
// verify identity with their own probe (the daemon's legacy import) use it so
// the package-global probe cannot silently drop entries first.
func LoadRaw() (*Registry, *Report) { return loadChecked(Path()) }

// LookupByPID returns the entry for a PID if present.
func (r *Registry) LookupByPID(pid int) (Entry, bool) {
	e, ok := r.Runs[pid]
	return e, ok
}

// Active returns all live entries (pruning is already applied by Load).
func (r *Registry) Active() []Entry {
	out := make([]Entry, 0, len(r.Runs))
	for _, e := range r.Runs {
		out = append(out, e)
	}
	return out
}

// Add records (or replaces) an entry for e.PID, atomically and under lock.
func Add(e Entry) error {
	if e.PID <= 0 {
		return fmt.Errorf("runs: invalid pid %d", e.PID)
	}
	if e.StartedAt == "" {
		e.StartedAt = time.Now().Format(time.RFC3339)
	}
	return withLock(func() error {
		reg, _ := loadChecked(Path())
		reg.Runs[e.PID] = e
		return save(reg)
	})
}

// Replace writes entries as the whole registry, atomically and under lock.
// Used by the daemon to rewrite the mirror after importing or reconciling
// state, so no stale legacy entry can linger in the file.
func Replace(entries []Entry) error {
	return withLock(func() error {
		reg := &Registry{Runs: map[int]Entry{}}
		for _, e := range entries {
			if e.PID <= 0 {
				continue
			}
			reg.Runs[e.PID] = e
		}
		return save(reg)
	})
}

// Remove deletes the entry for pid, atomically and under lock. Removing a
// missing pid is a no-op.
func Remove(pid int) error {
	return withLock(func() error {
		reg, _ := loadChecked(Path())
		if _, ok := reg.Runs[pid]; !ok {
			return nil
		}
		delete(reg.Runs, pid)
		return save(reg)
	})
}

// save writes the registry atomically: marshal -> temp file -> rename. Callers
// must already hold the lock (see withLock).
func save(reg *Registry) error {
	path := Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("runs: could not create config dir: %w", err)
	}
	if reg.Runs == nil {
		reg.Runs = map[int]Entry{}
	}
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return fmt.Errorf("runs: marshal: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".runs-*.json")
	if err != nil {
		return fmt.Errorf("runs: temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("runs: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("runs: close: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("runs: rename: %w", err)
	}
	return nil
}

// PIDAlive reports whether a process is still running. The daemon's in-memory
// registry prunes with the same existence test where identity evidence is not
// available.
func PIDAlive(pid int) bool { return pidAlive(pid) }
