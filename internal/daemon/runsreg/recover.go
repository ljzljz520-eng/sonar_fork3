package runsreg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/raskrebs/sonar/internal/runs"
)

// stateDir is the directory holding journal.ndjson and state.json.
func stateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "sonar", "runs"), nil
}

// Open prepares durable service: it creates the state directory, reads the
// snapshot and journal, replays them (live runs, exit history and every
// record's provenance and log cursor), then reconciles the replayed runs with
// the live system: only processes whose identity still matches stay live.
// Finally it imports the legacy runs.json when one is present and returns how
// many legacy runs were imported. The daemon calls Reset and then Open.
func (r *Registry) Open() int {
	dir, err := stateDir()
	if err != nil {
		return 0
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0
	}
	rec := openFiles(dir, r.clock)

	r.mu.Lock()
	baseSeq := int64(0)
	lastSeq := int64(0)
	// 1. Replay the snapshot first.
	if rec.snapshot != nil {
		baseSeq = rec.snapshot.Seq
		lastSeq = baseSeq
		for _, s := range rec.snapshot.Runs {
			r.installReplayLocked(decodeRecord(s))
		}
		for _, se := range rec.snapshot.Exits {
			r.applyExitFromEventLocked(decodeRecord(se.Record), se.Code, se.Reason,
				runs.ParseTime(se.ExitedAt), se.LastLines)
		}
	}
	// 2. Replay journal events over it.
	for _, ev := range rec.events {
		r.applyReplayEvent(ev)
		if ev.Seq > lastSeq {
			lastSeq = ev.Seq
		}
	}
	// 3. Recovery diagnostics (quarantined files, torn tails, gaps).
	for _, d := range rec.diags {
		r.addDiagLocked(d)
	}
	// 4. Open the journal for further appends at the recovered sequence.
	since := 0
	for _, ev := range rec.events {
		if ev.Seq > baseSeq {
			since++
		}
	}
	r.j = &journal{
		dir:    dir,
		path:   journalPath(dir),
		seq:    lastSeq,
		since:  since,
		crash:  r.crash,
		now:    r.clock,
		parent: r,
	}
	r.persist = true
	r.mu.Unlock()

	// 5. Take over identity-matching processes only. Dead and stale runs are
	// journaled as exits, stale ones with a stale_identity diagnostic.
	r.Prune()

	// 6. Import the legacy file; deterministic tokens make it repeatable.
	return r.ImportLegacy()
}

// installReplayLocked applies a register event during replay. Event order in
// a sealed journal already guarantees one owner per pid, but the defensive
// pid check makes any damaged history converge too: the later registration
// wins the pid, so two live owners can never result.
func (r *Registry) installReplayLocked(rec Record) {
	if rec.Token == "" {
		return
	}
	if t, ok := r.byPID[rec.PID]; ok && t != rec.Token {
		r.forgetLocked(t)
	}
	r.installLocked(rec)
}

// applyReplayEvent applies one journaled event to the in-memory state without
// touching files. Every branch is idempotent, which is what replaying after a
// crash relies on.
func (r *Registry) applyReplayEvent(ev event) {
	switch ev.Type {
	case evRegister:
		if ev.Record == nil {
			return
		}
		r.installReplayLocked(decodeRecord(*ev.Record))
	case evExit:
		if ev.Record == nil {
			return
		}
		r.applyExitFromEventLocked(decodeRecord(*ev.Record), ev.Code, ev.Reason,
			runs.ParseTime(ev.At), ev.LastLines)
	case evForget:
		token := ev.Token
		if token == "" && ev.Record != nil {
			token = ev.Record.Token
		}
		r.forgetLocked(token)
	case evStopping:
		for _, token := range ev.Tokens {
			if rec, ok := r.runs[token]; ok {
				rec.stopping = true
				r.runs[token] = rec
			}
		}
	case evRename:
		for token, rec := range r.runs {
			if next, ok := ev.Renames[rec.Group]; ok && next != "" {
				rec.Group = next
				r.runs[token] = rec
			}
		}
		for i := range r.exits {
			if next, ok := ev.Renames[r.exits[i].Group]; ok && next != "" {
				r.exits[i].Group = next
			}
		}
	case evDiag:
		if ev.Diag != nil {
			r.addDiagLocked(Diagnostic{
				Code: ev.Diag.Code, PID: ev.Diag.PID, Token: ev.Diag.Token,
				RunID: ev.Diag.RunID, Detail: ev.Diag.Detail,
			})
		}
	}
}

// applyExitFromEventLocked applies an exit event during replay. A token that
// already has an exit (a duplicate exit event left by the afterCommit crash
// point) is left alone, so replay yields exactly one exit per token.
func (r *Registry) applyExitFromEventLocked(rec Record, code int, reason string, at time.Time, lines []string) {
	if _, ok := r.exitIndex(rec.Token); ok {
		return
	}
	if at.IsZero() {
		at = r.clock()
	}
	r.applyExitLocked(rec, code, reason, at, lines)
}

// ImportLegacy reads the legacy ~/.config/sonar/runs.json and migrates its
// valid records into the registry. A damaged file is quarantined by the runs
// package and reported (legacy_quarantined), never silently emptied.
//
// A record without a token (written by an older sonar) gets a deterministic
// token derived from its fields, so re-running the migration — after a crash
// midway or on the next daemon start — yields the same identities on Linux,
// macOS and Windows and never duplicates a run. Dead and identity-stale
// records are not imported; stale pids are reported with stale_identity.
func (r *Registry) ImportLegacy() int {
	reg, rep := runs.LoadRaw()

	r.mu.Lock()
	defer r.mu.Unlock()
	if rep.Quarantined {
		r.addDiagLocked(Diagnostic{
			Code: DiagLegacyQuarantined, At: r.clock(),
			Detail: rep.Reason, Backup: rep.Backup,
		})
	}

	pids := make([]int, 0, len(reg.Runs))
	for pid := range reg.Runs {
		pids = append(pids, pid)
	}
	sort.Ints(pids)

	imported := 0
	for _, pid := range pids {
		e := reg.Runs[pid]
		token := e.Token
		if token == "" {
			token = legacyToken(e)
		}
		if _, ok := r.runs[token]; ok {
			continue // already migrated earlier
		}
		if _, ok := r.exitIndex(token); ok {
			continue
		}
		birth := runs.ParseTime(e.Birth)
		info := r.probe(pid)
		switch runs.VerifyInfo(info, token, birth) {
		case runs.StatusStale:
			r.addDiagLocked(Diagnostic{
				Code: DiagStaleIdentity, PID: pid, Token: token, RunID: e.ID,
				Detail: "legacy run not imported: pid reused by a different process",
			})
			continue
		case runs.StatusDead:
			continue
		}
		if birth.IsZero() && info.Alive {
			birth = info.Birth
		}
		rec := Record{
			ID: e.ID, Token: token, PID: pid, PPID: e.PPID,
			Group: e.GroupOf(), Name: e.NameOf(), Cmd: e.Cmd, Cwd: e.Cwd,
			PortHint: e.PortHint, Birth: birth,
		}
		if t, err := time.Parse(time.RFC3339, e.StartedAt); err == nil {
			rec.StartedAt = t
		}
		if !r.migrateOneLocked(rec) {
			// A journal crash point: the next open re-imports from scratch;
			// deterministic tokens keep it free of duplicates.
			break
		}
		imported++
	}

	// The legacy file's job is done. With mirroring on, rewrite it as the
	// registry's complete live mirror (dead/stale entries gone); otherwise
	// remove it so it cannot be mistaken for current state later.
	if r.Mirror {
		r.rewriteMirrorLocked()
	} else {
		_ = os.Remove(runs.Path())
	}
	return imported
}

// migrateOneLocked commits and installs one migrated record, returning false
// when the journal commit hit a crash point.
func (r *Registry) migrateOneLocked(rec Record) bool {
	if err := r.commitLocked("register", registerEvent(rec)); err != nil {
		return false
	}
	r.installReplayLocked(rec)
	return true
}

// legacyToken deterministically derives an identity for an entry that never
// carried one. Only persisted, platform-independent fields enter it, so the
// same record yields the same token on every OS and every run.
func legacyToken(e runs.Entry) string {
	h := sha256.New()
	fmt.Fprintf(h, "sonar-legacy\npid=%d\nid=%s\nstartedAt=%s\ngroup=%s\nname=%s\ntag=%s\n",
		e.PID, e.ID, e.StartedAt, e.Group, e.Name, e.Tag)
	return "lg" + hex.EncodeToString(h.Sum(nil)[:8])
}

// rewriteMirrorLocked replaces runs.json with the whole current live set, so
// the mirror the scanner reads can never hold a stale or dead entry.
func (r *Registry) rewriteMirrorLocked() {
	entries := make([]runs.Entry, 0, len(r.runs))
	for _, rec := range r.runs {
		entries = append(entries, runs.Entry{
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
	sort.Slice(entries, func(i, j int) bool { return entries[i].PID < entries[j].PID })
	_ = runs.Replace(entries)
}
