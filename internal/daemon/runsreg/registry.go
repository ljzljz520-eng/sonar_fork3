// Package runsreg is the daemon's registry of processes started through
// `sonar start` (and `runs.spawn`). It answers three questions: what is
// running, which run owns a listening port, and where does a detached run log.
//
// The registry is authoritative while the daemon lives and durably journals
// every transition (journal.go): a restart replays the live runs, the exit
// history and every run's provenance and log cursor. It also mirrors itself
// into the legacy ~/.config/sonar/runs.json, because the port scanner
// attributes listeners by walking their PPID ancestry against that file;
// keeping one writer (the daemon) and one reader (the scanner) is what makes
// `sonar list` show `group_source: start` with or without a daemon.
//
// A PID is only an address, never an identity. Runs are keyed by a random
// start token (and matched additionally against the process birth time the
// kernel reports), so a PID reused by a different process is diagnosed as
// stale_identity and is never attributed ports or sent a kill signal.
package runsreg

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/raskrebs/sonar/internal/ports"
	"github.com/raskrebs/sonar/internal/runs"
	"github.com/raskrebs/sonar/internal/sessions"
	"github.com/raskrebs/sonar/internal/state"
)

// parentsTTL bounds how long one process table is reused while attributing a
// snapshot's worth of ports.
const parentsTTL = 2 * time.Second

// maxAncestry guards the PPID walk against a cyclic process table.
const maxAncestry = 64

// Record is one registered run.
type Record struct {
	ID    string
	Token string
	PID   int
	PPID  int
	Group string
	Name  string
	Cmd   string
	Cwd   string

	PortHint  int
	StartedAt time.Time
	// Birth is the process creation time observed at registration, the
	// platform-independent identity evidence alongside Token.
	Birth time.Time
	// ConfigPath, StartID and Origin say where a run came from: the
	// sonar.yaml it was started from, the groups.start that started it with
	// its siblings, and the client that asked for it (cli, app, mcp).
	ConfigPath string
	StartID    string
	Origin     string
	// LogPath and LogOffset are where a detached run's output goes and where
	// this run's part of the file begins; its last lines are kept on exit.
	LogPath   string
	LogOffset int64
	// Session is the agent session that asked for this run, or the zero value
	// when nothing did (spec 2 §3). It travels with the run so every port the
	// run opens can be stamped with it.
	Session state.Session

	// stopping is set when sonar is about to stop this run, so its exit is
	// recorded as stopped rather than a crash.
	stopping bool
}

// Diagnostic codes.
const (
	DiagStaleIdentity       = "stale_identity"
	DiagLegacyQuarantined   = "legacy_quarantined"
	DiagJournalQuarantined  = "journal_quarantined"
	DiagJournalTruncated    = "journal_truncated"
	DiagSnapshotQuarantined = "snapshot_quarantined"
)

// maxDiagnostics bounds how many diagnostics are retained.
const maxDiagnostics = 200

// Diagnostic is an explicit report about identity or persisted-state trouble.
type Diagnostic struct {
	Code   string
	PID    int
	Token  string
	RunID  string
	At     time.Time
	Detail string
	// Backup is the quarantine copy, when one was made.
	Backup string
}

// Registry holds the live runs. The zero value is not usable; call New.
type Registry struct {
	mu sync.Mutex
	// runs is token -> live record.
	runs map[string]Record
	// byPID is pid -> token of the run currently claiming it.
	byPID map[int]string
	// exits is the history of runs that ended, oldest first, capped.
	exits []Exit
	diags []Diagnostic

	// Probe gathers identity evidence for a pid. Tests replace it; default is
	// the platform probe.
	Probe func(pid int) runs.Info
	// Parents returns a pid -> ppid table for the ancestry walk. Tests replace
	// it; production reads the same process table the scanner builds.
	Parents func() map[int]int
	// Mirror writes every change through to runs.json. Off in tests.
	Mirror bool

	parents   map[int]int
	parentsAt time.Time
	now       func() time.Time

	// crash is the optional crash-injection hook, installed on the journal
	// when one opens.
	crash CrashHook

	// j is the open durable journal; persist says it is usable.
	j       *journal
	persist bool
}

// New returns an empty registry that mirrors to runs.json.
func New() *Registry {
	return &Registry{
		runs:    map[string]Record{},
		byPID:   map[int]string{},
		Probe:   runs.Probe,
		Parents: ports.ParentTable,
		Mirror:  true,
		now:     time.Now,
	}
}

// SetCrashHook installs a crash-injection hook (tests).
func (r *Registry) SetCrashHook(h CrashHook) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.crash = h
	if r.j != nil {
		r.j.crash = h
	}
}

// Reset returns the registry to an empty, non-persistent state. Called by the
// daemon's OnStart hook before opening fresh files, so one Registry value is
// reusable across daemon instances in-process.
func (r *Registry) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = map[string]Record{}
	r.byPID = map[int]string{}
	r.exits = nil
	r.diags = nil
	r.parents = nil
	r.parentsAt = time.Time{}
	r.j = nil
	r.persist = false
}

// probe gathers evidence for pid.
func (r *Registry) probe(pid int) runs.Info {
	p := r.Probe
	if p == nil {
		p = runs.Probe
	}
	return p(pid)
}

// statusOfLocked matches a recorded identity against the live process.
func (r *Registry) statusOfLocked(rec Record) runs.Status {
	return runs.VerifyInfo(r.probe(rec.PID), rec.Token, rec.Birth)
}

// newToken returns a random start token.
func newToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "st" + strconv.Itoa(os.Getpid())
	}
	return "st" + hex.EncodeToString(b[:])
}

// installLocked puts rec into the live maps (register semantics).
func (r *Registry) installLocked(rec Record) {
	r.runs[rec.Token] = rec
	r.byPID[rec.PID] = rec.Token
}

// forgetLocked removes a token from the live maps.
func (r *Registry) forgetLocked(token string) {
	delete(r.runs, token)
	for pid, t := range r.byPID {
		if t == token {
			delete(r.byPID, pid)
		}
	}
}

// applyExitLocked moves a live run to the exit history. It is idempotent: a
// token already in exits is left alone, which is what makes replaying an exit
// (or a journal that contains a duplicate) safe.
func (r *Registry) applyExitLocked(rec Record, code int, reason string, at time.Time, lines []string) {
	if _, ok := r.exitIndex(rec.Token); ok {
		return
	}
	r.forgetLocked(rec.Token)
	r.exits = append(r.exits, Exit{
		Record:    rec,
		Code:      code,
		Reason:    reason,
		ExitedAt:  at,
		LastLines: lines,
	})
	if len(r.exits) > maxExits {
		r.exits = append([]Exit(nil), r.exits[len(r.exits)-maxExits:]...)
	}
}

// exitIndex finds a token's exit record.
func (r *Registry) exitIndex(token string) (int, bool) {
	for i := range r.exits {
		if r.exits[i].Token == token {
			return i, true
		}
	}
	return 0, false
}

// addDiagLocked records a diagnostic, newest first, capped.
func (r *Registry) addDiagLocked(d Diagnostic) {
	if d.At.IsZero() {
		d.At = r.clock()
	}
	r.diags = append([]Diagnostic{d}, r.diags...)
	if len(r.diags) > maxDiagnostics {
		r.diags = r.diags[:maxDiagnostics]
	}
}

// Diagnostics returns the diagnostics, newest first.
func (r *Registry) Diagnostics() []Diagnostic {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Diagnostic, len(r.diags))
	copy(out, r.diags)
	return out
}

// registerEvent builds the durable event for a register.
func registerEvent(rec Record) event {
	sr := encodeRecord(rec)
	return event{Type: evRegister, Record: &sr}
}

// exitEvent builds the durable event for an exit.
func exitEvent(rec Record, code int, reason string, lines []string) event {
	sr := encodeRecord(rec)
	return event{Type: evExit, Record: &sr, Code: code, Reason: reason, LastLines: lines}
}

// commitLocked durably commits ev and triggers compaction when due.
//
// Compaction happens BEFORE the commit: the snapshot must cover only the
// already-installed state (the new event's in-memory change comes after the
// commit), and the new event starts the fresh tail. Compacting after the
// commit would snapshot state one event behind and then truncate that event
// away, silently losing a run across restart.
func (r *Registry) commitLocked(op string, ev event) error {
	if !r.persist || r.j == nil {
		return nil
	}
	if r.j.since >= compactAfter {
		if err := r.j.compact(r); err != nil {
			// Compaction is an optimization; the journal still holds the
			// truth and the next commit tries again.
		}
	}
	if err := r.j.commit(op, ev); err != nil {
		return err
	}
	return nil
}

// closeRunLocked commits the exit and moves the run to history. It must not
// be called for a token already in exits.
func (r *Registry) closeRunLocked(rec Record, code int, reason string, at time.Time, lines []string) error {
	if err := r.commitLocked("exit", exitEvent(rec, code, reason, lines)); err != nil {
		return err
	}
	r.applyExitLocked(rec, code, reason, at, lines)
	return nil
}

// Register records a run and returns it with its id filled in.
//
// Identity rules: a pid already claimed by a different token is verified; if
// the old record's process is alive and matches, the new registration is
// rejected (the old record is returned); if the old process is gone or the
// pid has been reused by a different process, the old record is closed
// (stale_identity in the reuse case) before the new run takes the pid. The
// journal commit precedes the in-memory install.
func (r *Registry) Register(rec Record) Record {
	if rec.StartedAt.IsZero() {
		rec.StartedAt = r.clock()
	}
	if rec.Token == "" {
		rec.Token = newToken()
	}
	if rec.Birth.IsZero() {
		// Anchor the birth from the live process as early as possible.
		if info := r.probe(rec.PID); info.Alive {
			rec.Birth = info.Birth
		}
	}

	r.mu.Lock()
	if oldToken, ok := r.byPID[rec.PID]; ok && oldToken != rec.Token {
		old := r.runs[oldToken]
		switch st := r.statusOfLocked(old); st {
		case runs.StatusAlive:
			r.mu.Unlock()
			// The live process at this pid belongs to the old identity.
			return old
		default:
			reason := ReasonUnknown
			if st == runs.StatusStale {
				reason = ReasonStaleIdentity
			}
			now := r.clock()
			if err := r.closeRunLocked(old, -1, reason, now, nil); err != nil {
				r.mu.Unlock()
				return Record{}
			}
			if st == runs.StatusStale {
				r.addDiagLocked(Diagnostic{
					Code: DiagStaleIdentity, PID: old.PID, Token: old.Token,
					RunID: old.ID, At: now,
					Detail: "pid reused by a different process before the new run registered",
				})
			}
		}
	}
	if err := r.commitLocked("register", registerEvent(rec)); err != nil {
		r.mu.Unlock()
		return Record{}
	}
	r.installLocked(rec)
	r.mu.Unlock()

	r.mirrorAdd(rec)
	return rec
}

// Unregister drops the run with this pid, reporting whether there was one.
func (r *Registry) Unregister(pid int) bool {
	r.mu.Lock()
	token, ok := r.byPID[pid]
	if !ok {
		r.mu.Unlock()
		return false
	}
	if err := r.commitLocked("unregister", event{Type: evForget, Token: token, PIDs: []int{pid}}); err != nil {
		r.mu.Unlock()
		return false
	}
	r.forgetLocked(token)
	r.mu.Unlock()

	r.mirrorRemove(pid)
	return true
}

// RenameGroups moves every run recorded under an old group name to its new one
// (`groups.rename`), so a service started before its project was renamed stays
// in the project's group instead of keeping a group of the old name to itself.
// It reports how many live runs moved.
func (r *Registry) RenameGroups(renames map[string]string) int {
	if len(renames) == 0 {
		return 0
	}
	r.mu.Lock()
	moved := map[string]Record{}
	for token, rec := range r.runs {
		next, ok := renames[rec.Group]
		if !ok || next == "" {
			continue
		}
		rec.Group = next
		r.runs[token] = rec
		moved[token] = rec
	}
	if len(moved) > 0 {
		if err := r.commitLocked("rename", event{Type: evRename, Renames: renames}); err != nil {
			r.mu.Unlock()
			return 0
		}
	}
	for i := range r.exits {
		if next, ok := renames[r.exits[i].Group]; ok && next != "" {
			r.exits[i].Group = next
		}
	}
	out := make([]Record, 0, len(moved))
	for _, rec := range moved {
		out = append(out, rec)
	}
	r.mu.Unlock()

	for _, rec := range out {
		r.mirrorAdd(rec)
	}
	return len(out)
}

// List returns the live runs, oldest first, after reconciling them with the
// live system (dead and identity-stale runs are closed).
func (r *Registry) List() []Record {
	r.Prune()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Record, 0, len(r.runs))
	for _, rec := range r.runs {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StartedAt.Equal(out[j].StartedAt) {
			return out[i].PID < out[j].PID
		}
		return out[i].StartedAt.Before(out[j].StartedAt)
	})
	return out
}

// Lookup returns the run registered for a pid.
func (r *Registry) Lookup(pid int) (Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	token, ok := r.byPID[pid]
	if !ok {
		return Record{}, false
	}
	rec, ok := r.runs[token]
	return rec, ok
}

// Prune closes every run whose process has exited or whose identity no
// longer matches. The scanner calls it each tick; List and the resolver call
// it too, so a stale run never survives a read.
func (r *Registry) Prune() {
	r.mu.Lock()
	var closed []int
	for {
		type decision struct {
			rec Record
			st  runs.Status
		}
		var gone []decision
		for _, rec := range r.runs {
			if st := r.statusOfLocked(rec); st != runs.StatusAlive {
				gone = append(gone, decision{rec, st})
			}
		}
		if len(gone) == 0 {
			break
		}
		for _, d := range gone {
			reason := ReasonUnknown
			if d.st == runs.StatusStale {
				reason = ReasonStaleIdentity
			}
			now := r.clock()
			if err := r.closeRunLocked(d.rec, -1, reason, now, nil); err != nil {
				// Simulated crash: leave all mutation to replay. Mirror
				// removal is likewise deferred to the next open's rewrite.
				r.mu.Unlock()
				return
			}
			closed = append(closed, d.rec.PID)
			if d.st == runs.StatusStale {
				r.addDiagLocked(Diagnostic{
					Code: DiagStaleIdentity, PID: d.rec.PID, Token: d.rec.Token,
					RunID: d.rec.ID, At: now,
					Detail: "pid reused by a different process while the run was live",
				})
			}
		}
	}
	r.mu.Unlock()

	for _, pid := range closed {
		r.mirrorRemove(pid)
	}
}

// Run implements groups.Registry: it attributes a listening port to the run
// that owns it by walking the port's PPID ancestry, so `npm run dev` -> vite ->
// esbuild all resolve to the run that started them.
//
// It answers with the whole run because the resolver stamps it onto the row:
// this registry, not the runs.json mirror the scanner reads, is what a daemon
// knows about its own runs, and it knows it the moment `runs.register` returns.
func (r *Registry) Run(p state.Port) (state.Run, bool) {
	if rec, found := r.ancestor(p.PID, p.PPID); found {
		return state.Run{ID: rec.ID, Group: rec.Group, Name: rec.Name, RootPID: rec.PID}, true
	}
	// The scanner already walked the ancestry against the mirrored file while
	// enriching this port; trust that rather than walking a process table that
	// has moved on since.
	if p.Run != nil && (p.Run.Group != "" || p.Run.Name != "") {
		return *p.Run, true
	}
	return state.Run{}, false
}

// PortHint implements groups.PortHints: the port a live run of this service
// was started to bind, which for a `port: auto` service is the one groups.start
// assigned it.
func (r *Registry) PortHint(group, service string) (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.runs {
		if rec.Group == group && rec.Name == service && rec.PortHint > 0 {
			return rec.PortHint, true
		}
	}
	return 0, false
}

// GroupPIDs lists the verified live runs recorded under a group, so `sonar
// down` can stop the ones that hold no port — a worker, or a service still
// starting — which a kill by port never reaches.
func (r *Registry) GroupPIDs(group string) []int {
	var out []int
	for _, rec := range r.List() {
		if rec.Group == group {
			out = append(out, rec.PID)
		}
	}
	return out
}

// PID identity answers for kill-path callers.
const (
	PIDAliveResult = "alive"
	PIDDeadResult  = "dead"
	PIDStaleResult = "stale"
)

// PIDIdentity reports whether a pid is safe to signal ("alive": the process
// matches the run, or the pid is unknown to sonar), gone ("dead"), or reused
// by a different process ("stale": must not be signaled).
func (r *Registry) PIDIdentity(pid int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	token, ok := r.byPID[pid]
	if !ok {
		return PIDAliveResult
	}
	switch r.statusOfLocked(r.runs[token]) {
	case runs.StatusDead:
		return PIDDeadResult
	case runs.StatusStale:
		return PIDStaleResult
	default:
		return PIDAliveResult
	}
}

// PIDGuard is the kill-path gate: it returns the pids among pids that are safe
// to signal (unknown pids and verified runs). A pid held by a run whose
// identity no longer matches is blocked and gets one stale_identity
// diagnostic, so a reused pid is never signaled. Dead pids are passed
// through so the killer reports them as not found, as it always has.
func (r *Registry) PIDGuard(pids []int) []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, 0, len(pids))
	seen := map[int]bool{}
	for _, pid := range pids {
		if seen[pid] {
			continue
		}
		seen[pid] = true
		token, ok := r.byPID[pid]
		if !ok {
			out = append(out, pid)
			continue
		}
		rec := r.runs[token]
		if runs.VerifyInfo(r.probe(pid), token, rec.Birth) == runs.StatusStale {
			r.addDiagLocked(Diagnostic{
				Code: DiagStaleIdentity, PID: pid, Token: token, RunID: rec.ID,
				Detail: "kill blocked: pid reused by a different process; no signal sent",
			})
			continue
		}
		out = append(out, pid)
	}
	return out
}

// Compact forces a snapshot compaction. Used on graceful daemon shutdown.
func (r *Registry) Compact() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.persist && r.j != nil {
		_ = r.j.compact(r)
	}
}

// Session implements groups.SessionRegistry: it reports the agent session that
// started the run owning this port, using the same PPID walk the run
// attribution uses, so a port and its run can never disagree about who started
// them.
func (r *Registry) Session(p state.Port) (state.Session, bool) {
	rec, found := r.ancestor(p.PID, p.PPID)
	if !found || rec.Session.ID == "" {
		return state.Session{}, false
	}
	return rec.Session, true
}

// SessionRuns lists every live run that carries a session. The daemon's
// sessions handlers read it through an interface assertion on the installed
// run registry, which is how package daemon reaches this data without
// importing the package that registers itself into it.
func (r *Registry) SessionRuns() []sessions.Live {
	out := []sessions.Live{}
	for _, rec := range r.List() {
		if rec.Session.ID == "" {
			continue
		}
		out = append(out, sessions.Live{
			RunID:     rec.ID,
			PID:       rec.PID,
			Group:     rec.Group,
			Name:      rec.Name,
			Cmd:       rec.Cmd,
			Cwd:       rec.Cwd,
			StartedAt: rec.StartedAt,
			Session:   rec.Session,
		})
	}
	return out
}

// ancestor walks up from pid looking for a registered run. hintPPID is the
// parent the scanner already resolved, used before the process table is read.
func (r *Registry) ancestor(pid, hintPPID int) (Record, bool) {
	if pid <= 0 {
		return Record{}, false
	}
	if rec, ok := r.Lookup(pid); ok {
		return rec, true
	}
	if hintPPID > 1 {
		if rec, ok := r.Lookup(hintPPID); ok {
			return rec, true
		}
	}

	r.mu.Lock()
	empty := len(r.runs) == 0
	r.mu.Unlock()
	if empty {
		return Record{}, false
	}

	parents := r.parentTable()
	cur := pid
	for i := 0; i < maxAncestry; i++ {
		next, ok := parents[cur]
		if !ok || next <= 1 || next == cur {
			return Record{}, false
		}
		if rec, ok := r.Lookup(next); ok {
			return rec, true
		}
		cur = next
	}
	return Record{}, false
}

// parentTable returns a recent pid -> ppid map, rebuilding it at most once
// every parentsTTL so attributing a whole snapshot costs one process listing.
func (r *Registry) parentTable() map[int]int {
	r.mu.Lock()
	fresh := r.parents != nil && r.clock().Sub(r.parentsAt) < parentsTTL
	table := r.parents
	load := r.Parents
	r.mu.Unlock()
	if fresh || load == nil {
		return table
	}

	table = load()
	r.mu.Lock()
	r.parents, r.parentsAt = table, r.clock()
	r.mu.Unlock()
	return table
}

// mirrorAdd writes one run through to runs.json.
func (r *Registry) mirrorAdd(rec Record) {
	if !r.Mirror {
		return
	}
	_ = runs.Add(runs.Entry{
		PID:       rec.PID,
		Tag:       rec.Group,
		ID:        rec.ID,
		Cmd:       rec.Cmd,
		StartedAt: rec.StartedAt.Format(time.RFC3339),
		Group:     rec.Group,
		Name:      rec.Name,
		Cwd:       rec.Cwd,
		PPID:      rec.PPID,
		PortHint:  rec.PortHint,
		Token:     rec.Token,
		Birth:     runs.FormatTime(rec.Birth),
	})
}

// mirrorRemove drops one run from runs.json.
func (r *Registry) mirrorRemove(pid int) {
	if !r.Mirror {
		return
	}
	_ = runs.Remove(pid)
}

func (r *Registry) clock() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}
