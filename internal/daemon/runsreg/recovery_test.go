package runsreg

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/runs"
	"github.com/raskrebs/sonar/internal/sessions"
	"github.com/raskrebs/sonar/internal/state"
)

var (
	birth1 = time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	birth2 = birth1.Add(time.Hour)
)

// openDurable creates a fresh registry value, points it at HOME's state dir,
// and opens it. Each call models one daemon generation sharing the same files.
// It also reports how many legacy runs the open imported.
func openDurable(t *testing.T, info map[int]runs.Info, hook CrashHook) (*Registry, int) {
	t.Helper()
	r := New()
	r.Mirror = false
	r.Parents = func() map[int]int { return nil }
	r.Probe = fakeProbe(info)
	if hook != nil {
		r.SetCrashHook(hook)
	}
	r.Reset()
	return r, r.Open()
}

// assertInvariants verifies the crash-replay invariants: at most one live
// owner per pid, no token both live and exited, and exactly one exit per token.
func assertInvariants(t *testing.T, r *Registry) {
	t.Helper()
	owners := map[int]int{}
	for _, rec := range r.List() {
		owners[rec.PID]++
		if owners[rec.PID] > 1 {
			t.Fatalf("pid %d has %d live owners", rec.PID, owners[rec.PID])
		}
		if _, exited := r.exitIndex(rec.Token); exited {
			t.Fatalf("token %s is both live and in exit history", rec.Token)
		}
	}
	seen := map[string]int{}
	r.mu.Lock()
	for _, e := range r.exits {
		seen[e.Token]++
		if seen[e.Token] > 1 {
			t.Fatalf("token %s has %d exit records", e.Token, seen[e.Token])
		}
	}
	r.mu.Unlock()
}

func diagCode(r *Registry, code string) bool {
	for _, d := range r.Diagnostics() {
		if d.Code == code {
			return true
		}
	}
	return false
}

// TestCrashMatrix crashes at every point around register, exit and stopping,
// then reopens the files as a new daemon generation and checks replay.
func TestCrashMatrix(t *testing.T) {
	points := []string{"beforeWrite", "tornWrite", "beforeSync", "afterCommit"}

	t.Run("register", func(t *testing.T) {
		for _, point := range points {
			t.Run(point, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				info := map[int]runs.Info{42: aliveInfo(42, "tokA", birth1)}
				r, _ := openDurable(t, info,
					func(op, p string) bool { return op == "register" && p == point })
				if got := r.Register(Record{
					ID: "a", Token: "tokA", PID: 42, Group: "g", Name: "web", Birth: birth1,
				}); got.Token != "" {
					t.Fatalf("a crashed register returned %+v", got)
				}

				// Restart: afterCommit leaves the process running; the other
				// points leave no registration at all (model process gone).
				liveInfo := map[int]runs.Info{}
				if point == "afterCommit" {
					liveInfo[42] = aliveInfo(42, "tokA", birth1)
				}
				r2, _ := openDurable(t, liveInfo, nil)
				assertInvariants(t, r2)
				live := r2.List()
				if point == "afterCommit" {
					if len(live) != 1 || live[0].Token != "tokA" {
						t.Fatalf("live = %+v, want tokA", live)
					}
				} else if len(live) != 0 {
					t.Fatalf("live = %+v, want empty", live)
				}
				if len(r2.Exits()) != 0 {
					t.Fatalf("exits = %+v, want none", r2.Exits())
				}
				if point == "tornWrite" && !diagCode(r2, DiagJournalTruncated) {
					t.Errorf("torn tail was not reported: %+v", r2.Diagnostics())
				}
			})
		}
	})

	t.Run("exit", func(t *testing.T) {
		for _, point := range points {
			t.Run(point, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				info := map[int]runs.Info{42: aliveInfo(42, "tokA", birth1)}
				r, _ := openDurable(t, info,
					func(op, p string) bool { return op == "exit" && p == point })
				r.Register(Record{ID: "a", Token: "tokA", PID: 42, Group: "g", Name: "web", Birth: birth1})
				if _, ok := r.Exited(42, 1, false); ok {
					t.Fatal("a crashed exit reported success")
				}

				// Restart: the real process is gone in every case.
				r2, _ := openDurable(t, nil, nil)
				assertInvariants(t, r2)
				if live := r2.List(); len(live) != 0 {
					t.Fatalf("exited process still live: %+v", live)
				}
				exits := r2.Exits()
				if len(exits) != 1 {
					t.Fatalf("exits = %+v, want exactly one", exits)
				}
				if point == "afterCommit" {
					if exits[0].Code != 1 || exits[0].Reason != ReasonCrashed {
						t.Errorf("exit = %+v, want code 1 crashed", exits[0])
					}
				} else {
					if exits[0].Reason != ReasonUnknown {
						t.Errorf("exit = %+v, want reason unknown (discovered on prune)", exits[0])
					}
				}
			})
		}
	})

	t.Run("stopping", func(t *testing.T) {
		for _, point := range points {
			t.Run(point, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				info := map[int]runs.Info{42: aliveInfo(42, "tokA", birth1)}
				r, _ := openDurable(t, info,
					func(op, p string) bool { return op == "stopping" && p == point })
				r.Register(Record{ID: "a", Token: "tokA", PID: 42, Group: "g", Name: "web", Birth: birth1})
				r.Stopping([]int{42})

				// Restart with the same process still alive: only a fsynced
				// stopping event survives.
				r2, _ := openDurable(t, info, nil)
				assertInvariants(t, r2)
				rec, ok := r2.Lookup(42)
				if !ok {
					t.Fatal("the run was lost")
				}
				if point == "afterCommit" {
					if !rec.stopping {
						t.Error("durable stopping mark was not recovered")
					}
				} else if rec.stopping {
					t.Error("a non-durable stopping mark survived the restart")
				}
			})
		}
	})
}

// TestRestartRecoversTheFullRecord: config path, start/session/origin and the
// log cursor all survive a restart; exits survive too.
func TestRestartRecoversTheFullRecord(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	info := map[int]runs.Info{42: aliveInfo(42, "tokA", birth1)}
	r, _ := openDurable(t, info, nil)

	started := time.Date(2026, 9, 6, 9, 30, 0, 0, time.UTC)
	sess := state.Session{
		ID: "claude-code:abc", Tool: sessions.ToolClaudeCode, Label: "ship 2A.4",
		Worktree: "feature-x", Branch: "feature/x", Detected: true,
	}
	r.Register(Record{
		ID: "run1", Token: "tokA", PID: 42, PPID: 7, Group: "web", Name: "dev",
		Cmd: "npm run dev", Cwd: "/home/me/code/shop", PortHint: 5173,
		StartedAt: started, Birth: birth1,
		ConfigPath: "/home/me/code/sonar.yaml", StartID: "start-7", Origin: "cli",
		LogPath: "/home/me/.config/sonar/logs/web-dev.log", LogOffset: 4096,
		Session: sess,
	})

	// New daemon generation: the identity must match to be taken over.
	r2, _ := openDurable(t, info, nil)
	assertInvariants(t, r2)
	live := r2.List()
	if len(live) != 1 {
		t.Fatalf("live = %+v, want the one run", live)
	}
	got := live[0]
	for _, tt := range []struct {
		name string
		want any
		have any
	}{
		{"ID", "run1", got.ID}, {"Token", "tokA", got.Token}, {"PID", 42, got.PID},
		{"PPID", 7, got.PPID}, {"Group", "web", got.Group}, {"Name", "dev", got.Name},
		{"Cmd", "npm run dev", got.Cmd}, {"Cwd", "/home/me/code/shop", got.Cwd},
		{"PortHint", 5173, got.PortHint}, {"StartedAt", started, got.StartedAt},
		{"Birth", birth1, got.Birth}, {"ConfigPath", "/home/me/code/sonar.yaml", got.ConfigPath},
		{"StartID", "start-7", got.StartID}, {"Origin", "cli", got.Origin},
		{"LogPath", "/home/me/.config/sonar/logs/web-dev.log", got.LogPath},
		{"LogOffset", int64(4096), got.LogOffset},
	} {
		if tt.want != tt.have {
			t.Errorf("%s = %v, want %v", tt.name, tt.have, tt.want)
		}
	}
	if got.Session != sess {
		t.Errorf("Session = %+v, want %+v", got.Session, sess)
	}

	// Exit history survives a restart with its last lines.
	e, ok := r2.Exited(42, 0, false)
	if !ok {
		t.Fatal("Exited failed")
	}
	r3, _ := openDurable(t, nil, nil)
	assertInvariants(t, r3)
	exits := r3.Exits()
	if len(exits) != 1 {
		t.Fatalf("exits after restart = %+v", exits)
	}
	if exits[0].Token != "tokA" || exits[0].Reason != ReasonExited ||
		!exits[0].ExitedAt.Equal(e.ExitedAt) {
		t.Errorf("recovered exit = %+v", exits[0])
	}
}

// TestRestartTakesOverIdentityMatchesOnly: a replayed run whose process is now
// a different identity is not taken over and is reported as stale_identity.
func TestRestartTakesOverIdentityMatchesOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	r, _ := openDurable(t, map[int]runs.Info{42: aliveInfo(42, "tokA", birth1)}, nil)
	r.Register(Record{ID: "a", Token: "tokA", PID: 42, Group: "g", Name: "web", Birth: birth1})

	// Restart: pid 42 now carries a different token (PID reuse while down).
	reused := map[int]runs.Info{42: aliveInfo(42, "tokB", birth2)}
	r2, _ := openDurable(t, reused, nil)
	assertInvariants(t, r2)
	if live := r2.List(); len(live) != 0 {
		t.Fatalf("the stale run was taken over: %+v", live)
	}
	exits := r2.Exits()
	if len(exits) != 1 || exits[0].Reason != ReasonStaleIdentity {
		t.Fatalf("exits = %+v, want one stale_identity", exits)
	}
	if !diagCode(r2, DiagStaleIdentity) {
		t.Errorf("no stale_identity diagnostic: %+v", r2.Diagnostics())
	}
	// The new process is never attributed to the old run.
	if run, ok := r2.Run(state.Port{PID: 42}); ok && run.ID == "a" {
		t.Fatalf("new process attributed to old run: %+v", run)
	}
}

// TestCompactionAndRestart writes past compactAfter events and reopens:
// the snapshot plus the short tail replay to the same one-owner state.
func TestCompactionAndRestart(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const n = compactAfter + 6
	info := map[int]runs.Info{}
	for pid := 1; pid <= n; pid++ {
		info[pid] = aliveInfo(pid, "", birth1.Add(time.Duration(pid)*time.Second))
	}
	r, _ := openDurable(t, info, nil)
	for pid := 1; pid <= n; pid++ {
		r.Register(Record{
			ID: itoa(pid), Token: "tok" + itoa(pid), PID: pid,
			Group: "g", Name: "job", StartedAt: birth1.Add(time.Duration(pid) * time.Second),
		})
	}

	journalBytes, _ := os.ReadFile(filepath.Join(home, ".config", "sonar", "runs", journalName))
	// Compaction fires before the first commit past the threshold (event 65);
	// the new tail is events 65..70 = 6 lines, all installed by then.
	if lines := bytesTrimEmpty(journalBytes); lines != n-compactAfter {
		t.Errorf("post-compaction journal has %d lines, want %d", lines, n-compactAfter)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "sonar", "runs", snapshotName)); err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}

	r2, _ := openDurable(t, info, nil)
	assertInvariants(t, r2)
	if live := r2.List(); len(live) != n {
		t.Fatalf("live after restart = %d, want %d", len(live), n)
	}
}

// bytesTrimEmpty counts non-empty newline-separated lines.
func bytesTrimEmpty(data []byte) int {
	count := 0
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			for j := start; j < i; j++ {
				if data[j] != ' ' && data[j] != '\t' {
					count++
					break
				}
			}
			start = i + 1
		}
	}
	return count
}
