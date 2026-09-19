package runsreg

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/raskrebs/sonar/internal/runs"
	"github.com/raskrebs/sonar/internal/state"
)

// Durable state for the run registry.
//
// Two files live in ~/.config/sonar/runs:
//
//   - journal.ndjson: append-only, one JSON event per line. Each event is
//     sealed with a sha256 over its content and carries a monotonic sequence
//     number. A write is a commit only once the file has been fsynced.
//   - state.json: a compacted snapshot (live runs and exit history) at a
//     sequence number; journal events after that number are replayed over it.
//
// Every state transition journals BEFORE the in-memory state changes, so a
// crash at any point leaves files that replay to the same result: one live
// owner per token, one exit per token, never an exited process marked
// running. A torn tail is quarantined and truncated; any other journal
// damage quarantines the whole file and falls back to the snapshot.

const (
	snapVersion      = 1
	journalName      = "journal.ndjson"
	snapshotName     = "state.json"
	compactAfter     = 64 // events between compactions
	journalTailBytes = 4 << 20
)

// ErrSimulatedCrash is returned by a crash-instrumented journal commit. The
// caller treats it like the process having died at that point: no in-memory
// mutation happens, and tests re-open the registry from the files as they
// stand.
var ErrSimulatedCrash = errors.New("runsreg: simulated crash")

// CrashHook decides whether an operation crashes at a point. Points:
// "beforeWrite" (no bytes), "tornWrite" (a partial line is left),
// "beforeSync" (a full line is written then rolled back), and
// "afterCommit" (the fsynced event stays).
type CrashHook func(op, point string) bool

// storedRecord is the durable form of Record.
type storedRecord struct {
	ID         string         `json:"id,omitempty"`
	Token      string         `json:"token"`
	PID        int            `json:"pid"`
	PPID       int            `json:"ppid,omitempty"`
	Group      string         `json:"group,omitempty"`
	Name       string         `json:"name,omitempty"`
	Cmd        string         `json:"cmd,omitempty"`
	Cwd        string         `json:"cwd,omitempty"`
	PortHint   int            `json:"portHint,omitempty"`
	StartedAt  string         `json:"startedAt,omitempty"`
	Birth      string         `json:"birth,omitempty"`
	ConfigPath string         `json:"configPath,omitempty"`
	StartID    string         `json:"startId,omitempty"`
	Origin     string         `json:"origin,omitempty"`
	LogPath    string         `json:"logPath,omitempty"`
	LogOffset  int64          `json:"logOffset,omitempty"`
	Session    *state.Session `json:"session,omitempty"`
	Stopping   bool           `json:"stopping,omitempty"`
}

func encodeRecord(rec Record) storedRecord {
	s := storedRecord{
		ID:         rec.ID,
		Token:      rec.Token,
		PID:        rec.PID,
		PPID:       rec.PPID,
		Group:      rec.Group,
		Name:       rec.Name,
		Cmd:        rec.Cmd,
		Cwd:        rec.Cwd,
		PortHint:   rec.PortHint,
		StartedAt:  runs.FormatTime(rec.StartedAt),
		Birth:      runs.FormatTime(rec.Birth),
		ConfigPath: rec.ConfigPath,
		StartID:    rec.StartID,
		Origin:     rec.Origin,
		LogPath:    rec.LogPath,
		LogOffset:  rec.LogOffset,
		Stopping:   rec.stopping,
	}
	if rec.Session.ID != "" {
		s.Session = &rec.Session
	}
	return s
}

func decodeRecord(s storedRecord) Record {
	rec := Record{
		ID:         s.ID,
		Token:      s.Token,
		PID:        s.PID,
		PPID:       s.PPID,
		Group:      s.Group,
		Name:       s.Name,
		Cmd:        s.Cmd,
		Cwd:        s.Cwd,
		PortHint:   s.PortHint,
		StartedAt:  runs.ParseTime(s.StartedAt),
		Birth:      runs.ParseTime(s.Birth),
		ConfigPath: s.ConfigPath,
		StartID:    s.StartID,
		Origin:     s.Origin,
		LogPath:    s.LogPath,
		LogOffset:  s.LogOffset,
		stopping:   s.Stopping,
	}
	if s.Session != nil {
		rec.Session = *s.Session
	}
	return rec
}

// eventType values.
const (
	evRegister = "register"
	evExit     = "exit"
	evForget   = "forget"
	evStopping = "stopping"
	evRename   = "rename"
	evDiag     = "diag"
)

// event is one journal line.
type event struct {
	Seq       int64             `json:"seq"`
	At        string            `json:"at"`
	Type      string            `json:"type"`
	Hash      string            `json:"hash"`
	Record    *storedRecord     `json:"record,omitempty"`
	Token     string            `json:"token,omitempty"`
	Tokens    []string          `json:"tokens,omitempty"`
	PIDs      []int             `json:"pids,omitempty"`
	Code      int               `json:"code,omitempty"`
	Reason    string            `json:"reason,omitempty"`
	LastLines []string          `json:"lastLines,omitempty"`
	Renames   map[string]string `json:"renames,omitempty"`
	Diag      *storedDiag       `json:"diag,omitempty"`
}

// storedDiag is the durable form of Diagnostic.
type storedDiag struct {
	Code   string `json:"code"`
	PID    int    `json:"pid,omitempty"`
	Token  string `json:"token,omitempty"`
	RunID  string `json:"runId,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// seal returns the sha256 over the canonical JSON of ev without its hash.
func seal(ev event) string {
	ev.Hash = ""
	data, err := json.Marshal(ev)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// journal is the open append-only event log.
type journal struct {
	dir    string
	path   string
	mu     sync.Mutex
	seq    int64
	since  int // events committed since compaction
	crash  CrashHook
	now    func() time.Time
	parent *Registry
}

func journalPath(dir string) string { return filepath.Join(dir, journalName) }
func snapPathValue(dir string) string { return filepath.Join(dir, snapshotName) }

// commit appends one event, fsyncs it and reports the crash points. The
// caller (registry) holds its own mutex.
func (j *journal) commit(op string, ev event) error {
	j.mu.Lock()
	defer j.mu.Unlock()

	ev.Seq = j.seq + 1
	if ev.At == "" {
		ev.At = runs.FormatTime(j.now())
	}
	ev.Hash = seal(ev)

	if j.crash != nil && j.crash(op, "beforeWrite") {
		return ErrSimulatedCrash
	}

	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("runsreg: open journal: %w", err)
	}

	line, err := json.Marshal(ev)
	if err != nil {
		f.Close()
		return fmt.Errorf("runsreg: marshal event: %w", err)
	}

	if j.crash != nil && j.crash(op, "tornWrite") {
		half := len(line) / 2
		if half > 0 {
			_, _ = f.Write(line[:half]) // no newline: a torn tail
		}
		_ = f.Close()
		return ErrSimulatedCrash
	}

	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("runsreg: journal write: %w", err)
	}

	if j.crash != nil && j.crash(op, "beforeSync") {
		// Model the event not surviving: roll the file back, then close.
		_ = f.Truncate(j.logSizeBefore(line))
		_ = f.Close()
		return ErrSimulatedCrash
	}

	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("runsreg: journal sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("runsreg: journal close: %w", err)
	}

	j.seq = ev.Seq
	j.since++

	if j.crash != nil && j.crash(op, "afterCommit") {
		return ErrSimulatedCrash
	}
	return nil
}

// logSizeBefore returns the file size immediately before a full line was
// appended (current size minus line+newline, never negative).
func (j *journal) logSizeBefore(line []byte) int64 {
	if info, err := os.Stat(j.path); err == nil {
		size := info.Size() - int64(len(line)) - 1
		if size < 0 {
			return 0
		}
		return size
	}
	return 0
}

// snapshotData is the durable snapshot.
type snapshotData struct {
	Version int           `json:"version"`
	Seq     int64         `json:"seq"`
	At      string        `json:"at"`
	Runs    []storedRecord `json:"runs"`
	Exits   []storedExit  `json:"exits"`
}

type storedExit struct {
	Record    storedRecord `json:"record"`
	Code      int          `json:"code"`
	Reason    string       `json:"reason"`
	ExitedAt  string       `json:"exitedAt"`
	LastLines []string     `json:"lastLines,omitempty"`
}

// writeAtomic writes data to dir/name via a temp file, fsync and rename, then
// fsyncs the directory.
func writeAtomic(dir, name string, data []byte) error {
	tmp, err := os.CreateTemp(dir, ".state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		os.Remove(tmpName)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// compact writes the snapshot at the current sequence and truncates the
// journal. The caller holds the registry mutex.
func (j *journal) compact(r *Registry) error {
	snap := snapshotData{
		Version: snapVersion,
		Seq:     j.seq,
		At:      runs.FormatTime(r.clock()),
		Runs:    make([]storedRecord, 0, len(r.runs)),
		Exits:   make([]storedExit, 0, len(r.exits)),
	}
	for _, rec := range r.runs {
		snap.Runs = append(snap.Runs, encodeRecord(rec))
	}
	// Deterministic snapshot bytes: order by start time, then pid.
	sort.Slice(snap.Runs, func(i, j int) bool {
		ti, tj := runs.ParseTime(snap.Runs[i].StartedAt), runs.ParseTime(snap.Runs[j].StartedAt)
		if ti.Equal(tj) {
			if snap.Runs[i].PID == snap.Runs[j].PID {
				return snap.Runs[i].Token < snap.Runs[j].Token
			}
			return snap.Runs[i].PID < snap.Runs[j].PID
		}
		return ti.Before(tj)
	})
	for _, e := range r.exits {
		snap.Exits = append(snap.Exits, storedExit{
			Record:    encodeRecord(e.Record),
			Code:      e.Code,
			Reason:    e.Reason,
			ExitedAt:  runs.FormatTime(e.ExitedAt),
			LastLines: e.LastLines,
		})
	}
	sort.Slice(snap.Exits, func(i, j int) bool {
		ti, tj := runs.ParseTime(snap.Exits[i].ExitedAt), runs.ParseTime(snap.Exits[j].ExitedAt)
		if ti.Equal(tj) {
			return snap.Exits[i].Record.Token < snap.Exits[j].Record.Token
		}
		return ti.Before(tj)
	})
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(j.dir, snapshotName, data); err != nil {
		return fmt.Errorf("runsreg: write snapshot: %w", err)
	}
	// Snapshot is durable and covers the whole journal: only now truncate it.
	if err := writeAtomic(j.dir, journalName, nil); err != nil {
		return fmt.Errorf("runsreg: truncate journal: %w", err)
	}
	j.since = 0
	return nil
}

// recovery is everything openFiles gathered.
type recovery struct {
	snapshot *snapshotData
	events   []event
	diags    []Diagnostic
}

// openFiles reads the snapshot and journal, isolating damage.
func openFiles(dir string, now func() time.Time) recovery {
	out := recovery{}

	if data, err := os.ReadFile(snapPathValue(dir)); err == nil {
		snap := snapshotData{}
		if jerr := json.Unmarshal(data, &snap); jerr != nil || snap.Version != snapVersion {
			reason := "snapshot parse error"
			if jerr != nil {
				reason += ": " + jerr.Error()
			} else {
				reason = fmt.Sprintf("unsupported snapshot version %d", snap.Version)
			}
			out.diags = append(out.diags, quarantineFile(snapPathValue(dir), DiagSnapshotQuarantined, reason, now))
		} else {
			out.snapshot = &snap
		}
	} else if !os.IsNotExist(err) {
		out.diags = append(out.diags, quarantineFile(snapPathValue(dir), DiagSnapshotQuarantined, "unreadable: "+err.Error(), now))
	}

	data, err := os.ReadFile(journalPath(dir))
	if err != nil {
		if !os.IsNotExist(err) {
			out.diags = append(out.diags, quarantineFile(journalPath(dir), DiagJournalQuarantined, "unreadable: "+err.Error(), now))
		}
		return out
	}

	// The journal may legitimately begin before the snapshot sequence: a
	// compaction writes the snapshot and only then truncates the journal, so
	// an interrupted compaction leaves the whole history in place. Parse it
	// as one sealed stream and keep only events after the snapshot.
	events, validBytes, jdiags := parseJournal(data, journalPath(dir), now)
	out.diags = append(out.diags, jdiags...)

	if len(jdiags) > 0 && validBytes < int64(len(data)) {
		torn := false
		for _, d := range jdiags {
			if d.Code == DiagJournalTruncated {
				torn = true
			}
		}
		if torn {
			// Keep a quarantine copy and truncate the live journal to the
			// valid prefix.
			quarantineBytes(journalPath(dir), data, DiagJournalTruncated, "torn tail", now)
			if err := os.Truncate(journalPath(dir), validBytes); err != nil {
				// Cannot repair in place: quarantine the whole file.
				out.diags = append(out.diags, quarantineFile(journalPath(dir), DiagJournalQuarantined, "cannot truncate torn journal: "+err.Error(), now))
			}
		}
	}

	baseSeq := int64(0)
	if out.snapshot != nil {
		baseSeq = out.snapshot.Seq
	}
	post := make([]event, 0, len(events))
	for _, ev := range events {
		if ev.Seq > baseSeq {
			post = append(post, ev)
		}
	}
	// Alignment: the first event after the snapshot must be exactly the next
	// sequence (seq 1 with no snapshot). A gap means history was lost: the
	// journal cannot be trusted, so it is quarantined and the snapshot is
	// used alone.
	if len(post) > 0 && post[0].Seq != baseSeq+1 {
		out.diags = append(out.diags, quarantineFile(journalPath(dir), DiagJournalQuarantined,
			fmt.Sprintf("journal gap: wanted seq %d, found %d", baseSeq+1, post[0].Seq), now))
		post = nil
	}
	out.events = post
	return out
}

// parseJournal splits the journal into lines and validates each event's hash
// and continuity against the first sealed event found (the file may begin at
// any sequence when a snapshot covers its prefix). It returns the valid
// events, the byte offset through which the input was valid and diagnostics.
func parseJournal(data []byte, path string, now func() time.Time) ([]event, int64, []Diagnostic) {
	var (
		events []event
		diags  []Diagnostic
	)
	// expect < 0 until the first valid event anchors the stream.
	expect := int64(-1)
	offset := int64(0)
	good := int64(0)

	rest := data
	for len(rest) > 0 {
		nl := bytes.IndexByte(rest, '\n')
		var line []byte
		if nl < 0 {
			line = rest
			rest = nil
		} else {
			line = rest[:nl]
			rest = rest[nl+1:]
		}
		lineStart := offset
		offset += int64(len(line)) + 1

		if len(bytes.TrimSpace(line)) == 0 {
			good = offset
			continue
		}

		ev, perr := validEvent(line, expect)
		if perr != "" {
			// Damage at the tail with no later valid lines is a torn append.
			if nl >= 0 && onlyBlankOrBad(rest, expect) {
				diags = append(diags, Diagnostic{
					Code: DiagJournalTruncated, At: now(),
					Detail: "torn or invalid journal tail: " + perr,
				})
				return events, good, diags
			}
			if nl < 0 {
				diags = append(diags, Diagnostic{
					Code: DiagJournalTruncated, At: now(),
					Detail: "torn journal tail (no newline): " + perr,
				})
				return events, good, diags
			}
			diags = append(diags, quarantineFile(path, DiagJournalQuarantined,
				"journal damage: "+perr, now))
			return events, good, diags
		}

		events = append(events, ev)
		if expect < 0 {
			expect = ev.Seq
		}
		expect++
		good = lineStart + int64(len(line)) + 1
	}
	return events, good, diags
}

// onlyBlankOrBad reports whether rest contains no fully-valid next event
// (blank lines or a single invalid tail).
func onlyBlankOrBad(rest []byte, expect int64) bool {
	for len(rest) > 0 {
		nl := bytes.IndexByte(rest, '\n')
		var line []byte
		if nl < 0 {
			line = rest
			rest = nil
		} else {
			line = rest[:nl]
			rest = rest[nl+1:]
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if _, err := validEvent(line, expect); err == "" {
			return false // a valid event follows the bad line: not a torn tail
		}
		if expect >= 0 {
			expect++
		}
	}
	return true
}

// validEvent parses one line, checks continuity (expect < 0 accepts the first
// event at any sequence) and the hash seal.
func validEvent(line []byte, expect int64) (event, string) {
	var ev event
	if err := json.Unmarshal(line, &ev); err != nil {
		return event{}, err.Error()
	}
	if expect >= 0 && ev.Seq != expect {
		return event{}, fmt.Sprintf("seq %d, expected %d", ev.Seq, expect)
	}
	want := seal(ev)
	if ev.Hash == "" || ev.Hash != want {
		return event{}, fmt.Sprintf("hash mismatch at seq %d", ev.Seq)
	}
	return ev, ""
}

// quarantineSuffix builds a unique backup suffix.
func quarantineSuffix(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060101T150405.000000000") + "-" + hex.EncodeToString(b[:])
}

// quarantineFile renames path to a timestamped backup and returns the
// diagnostic. A rename failure leaves the file in place (copy fallback).
func quarantineFile(path, code, reason string, now func() time.Time) Diagnostic {
	backup := fmt.Sprintf("%s.quarantine-%s", path, quarantineSuffix(now()))
	if err := os.Rename(path, backup); err != nil {
		// Same-directory rename failed: copy then remove, so the damaged file
		// still gets out of the way.
		if copyErr := copyFile(path, backup); copyErr != nil {
			backup = ""
		} else {
			_ = os.Remove(path)
		}
	}
	return Diagnostic{Code: code, At: now(), Detail: reason, Backup: backup}
}

// quarantineBytes copies the bytes of a damaged file aside without removing
// the original (the caller truncates it).
func quarantineBytes(path string, data []byte, code, reason string, now func() time.Time) Diagnostic {
	backup := fmt.Sprintf("%s.quarantine-%s", path, quarantineSuffix(now()))
	if err := os.WriteFile(backup, data, 0o600); err != nil {
		backup = ""
	}
	return Diagnostic{Code: code, At: now(), Detail: reason, Backup: backup}
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
