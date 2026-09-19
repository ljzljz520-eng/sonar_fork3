package runsreg

import (
	"strconv"
	"testing"
	"time"

	"github.com/raskrebs/sonar/internal/runs"
	"github.com/raskrebs/sonar/internal/state"
)

// itoa formats n (test helper, cheaper than fmt.Sprintf in loops).
func itoa(n int) string { return strconv.Itoa(n) }

// testRegistry is a registry with no mirror, no process table and a probe
// that reports every pid in alive as alive without identity evidence (the
// conservative path).
func testRegistry(alive ...int) *Registry {
	info := map[int]runs.Info{}
	for _, pid := range alive {
		info[pid] = runs.Info{PID: pid, Alive: true}
	}
	r := New()
	r.Mirror = false
	r.Probe = fakeProbe(info)
	r.Parents = func() map[int]int { return nil }
	return r
}

// fakeProbe answers pid from info; unknown pids are dead.
func fakeProbe(info map[int]runs.Info) func(int) runs.Info {
	return func(pid int) runs.Info {
		if i, ok := info[pid]; ok {
			return i
		}
		return runs.Info{PID: pid}
	}
}

func aliveInfo(pid int, token string, birth time.Time) runs.Info {
	return runs.Info{PID: pid, Alive: true, Token: token, Birth: birth}
}

// identityRegistry reports one pid carrying token and birth, the live shape
// for PID-reuse tests.
func identityRegistry(pid int, token string, birth time.Time) *Registry {
	r := testRegistry()
	r.Probe = fakeProbe(map[int]runs.Info{pid: aliveInfo(pid, token, birth)})
	return r
}

func TestRegisterListAndUnregister(t *testing.T) {
	r := testRegistry(100, 200)
	base := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)

	r.Register(Record{ID: "b", PID: 200, Group: "g", Name: "api", StartedAt: base.Add(time.Minute)})
	r.Register(Record{ID: "a", PID: 100, Group: "g", Name: "web", StartedAt: base})

	got := r.List()
	if len(got) != 2 {
		t.Fatalf("List = %d runs, want 2", len(got))
	}
	if got[0].ID != "a" || got[1].ID != "b" {
		t.Fatalf("List is not oldest first: %v", []string{got[0].ID, got[1].ID})
	}

	if !r.Unregister(100) {
		t.Fatal("Unregister(100) reported nothing to remove")
	}
	if r.Unregister(100) {
		t.Fatal("Unregister(100) removed the same run twice")
	}
	if len(r.List()) != 1 {
		t.Fatalf("List after unregister = %v", r.List())
	}
}

// TestRegisterRejectsAnAlivePIDOwnedByAnotherToken: the new registration
// must not take over a verified live process; the old record is returned.
func TestRegisterRejectsAnAlivePIDOwnedByAnotherToken(t *testing.T) {
	birth := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	r := identityRegistry(100, "old-token", birth)
	r.Register(Record{ID: "keep", Token: "old-token", PID: 100, Group: "g", Name: "web", Birth: birth})

	got := r.Register(Record{ID: "new", Token: "new-token", PID: 100, Group: "g", Name: "web"})
	if got.Token != "old-token" {
		t.Fatalf("registration returned %q, want the old owner", got.Token)
	}
	if len(r.List()) != 1 || r.List()[0].Token != "old-token" {
		t.Fatalf("a second live owner appeared: %+v", r.List())
	}
}

// TestRegisterClosesADeadPIDBeforeTakingIt: the old run is recorded as an
// exit (unknown) and the new run owns the pid, with one owner and one exit.
func TestRegisterClosesADeadPIDBeforeTakingIt(t *testing.T) {
	r := testRegistry(100)
	r.Register(Record{ID: "old", Token: "old-token", PID: 100, Group: "g", Name: "web",
		Birth: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)})
	// The old process is gone; the new one then takes the pid. The first
	// probe sees the empty slot, later probes the new process (the spawn
	// handler supplies its token and birth, as here).
	calls := 0
	r.Probe = func(pid int) runs.Info {
		if pid != 100 {
			return runs.Info{PID: pid}
		}
		calls++
		if calls == 1 {
			return runs.Info{PID: 100} // old owner verified as gone
		}
		return aliveInfo(100, "new-token", birth2)
	}

	got := r.Register(Record{ID: "new", Token: "new-token", PID: 100, Group: "g", Name: "api", Birth: birth2})
	if got.Token != "new-token" {
		t.Fatalf("new register = %+v", got)
	}
	live := r.List()
	if len(live) != 1 || live[0].Token != "new-token" {
		t.Fatalf("live owners after takeover: %+v", live)
	}
	exits := r.Exits()
	if len(exits) != 1 || exits[0].Token != "old-token" || exits[0].Reason != ReasonUnknown {
		t.Fatalf("exits = %+v, want one unknown exit for the old run", exits)
	}
}

// TestRegisterOnAReusedPIDReportsStaleIdentity: same pid, different process
// (different token): the old run must not own or be confused with the new one,
// it is closed as stale_identity and an explicit diagnostic is produced.
func TestRegisterOnAReusedPIDReportsStaleIdentity(t *testing.T) {
	birth1 := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	birth2 := birth1.Add(time.Hour)
	r := identityRegistry(100, "new-token", birth2)
	r.Register(Record{ID: "old", Token: "old-token", PID: 100, Group: "g", Name: "web", Birth: birth1})

	got := r.Register(Record{ID: "new", Token: "new-token", PID: 100, Group: "g", Name: "api", Birth: birth2})
	if got.Token != "new-token" {
		t.Fatalf("new register = %+v, want the new run", got)
	}
	live := r.List()
	if len(live) != 1 || live[0].Token != "new-token" {
		t.Fatalf("live owners = %+v, want exactly the new run", live)
	}
	exits := r.Exits()
	if len(exits) != 1 {
		t.Fatalf("exits = %+v, want one", exits)
	}
	if exits[0].Token != "old-token" || exits[0].Reason != ReasonStaleIdentity {
		t.Fatalf("old run exit = %+v, want stale_identity", exits[0])
	}
	if !hasDiag(r, DiagStaleIdentity, 100) {
		t.Fatalf("no stale_identity diagnostic in %+v", r.Diagnostics())
	}
}

// TestPruneDropsDeadRuns.
func TestPruneDropsDeadRuns(t *testing.T) {
	r := testRegistry(100)
	r.Register(Record{ID: "a", Token: "ta", PID: 100, Group: "g", Name: "web"})
	r.Register(Record{ID: "b", Token: "tb", PID: 999, Group: "g", Name: "gone"})

	r.Prune()
	got := r.List()
	if len(got) != 1 || got[0].PID != 100 {
		t.Fatalf("Prune left %v, want only pid 100", got)
	}
}

// TestPruneOnPIDReuseReportsStaleIdentity: while the run was live its pid
// was reused by a process carrying a different token.
func TestPruneOnPIDReuseReportsStaleIdentity(t *testing.T) {
	birth1 := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	birth2 := birth1.Add(2 * time.Hour)
	r := identityRegistry(100, "different", birth2)
	r.Register(Record{ID: "a", Token: "old-token", PID: 100, Group: "g", Name: "web", Birth: birth1})

	r.Prune()
	if live := r.List(); len(live) != 0 {
		t.Fatalf("the stale run survived pruning: %+v", live)
	}
	exits := r.Exits()
	if len(exits) != 1 || exits[0].Reason != ReasonStaleIdentity {
		t.Fatalf("exits = %+v, want one stale_identity", exits)
	}
	if !hasDiag(r, DiagStaleIdentity, 100) {
		t.Fatalf("missing stale_identity diagnostic: %+v", r.Diagnostics())
	}
}

func TestRunAttributesAPortByItsPPIDAncestry(t *testing.T) {
	r := testRegistry(100)
	r.Register(Record{ID: "a", Token: "ta", PID: 100, Group: "itest", Name: "web"})
	// sonar start (100) -> npm (200) -> node (300) -> esbuild (400)
	r.Parents = func() map[int]int { return map[int]int{400: 300, 300: 200, 200: 100} }

	run, ok := r.Run(state.Port{PID: 400, PPID: 300})
	if !ok || run.Group != "itest" || run.Name != "web" || run.RootPID != 100 {
		t.Fatalf("Run(descendant) = %+v/%v, want itest/web rooted at 100", run, ok)
	}

	if _, ok := r.Run(state.Port{PID: 777, PPID: 1}); ok {
		t.Fatal("an unrelated listener was attributed to a run")
	}
}

// TestRunAttributesTheLinuxScannerShape is the shape a Linux scan hands the
// resolver: `ss -tlnp` reports the listening pid and nothing else.
func TestRunAttributesTheLinuxScannerShape(t *testing.T) {
	r := testRegistry(100, 300)
	r.Register(Record{ID: "a", Token: "ta", PID: 100, Group: "itest", Name: "web"})
	// sonar start (100) -> the listener it spawned (300). ss gave no ppid.
	r.Parents = func() map[int]int { return map[int]int{300: 100, 100: 42} }

	run, ok := r.Run(state.Port{PID: 300, PPID: 0})
	if !ok {
		t.Fatal("a listener spawned by a run was not attributed to it")
	}
	if run.ID != "a" || run.Group != "itest" || run.Name != "web" || run.RootPID != 100 {
		t.Fatalf("Run = %+v, want a/itest/web rooted at 100", run)
	}
}

// TestRunAttributesTheRegisteredPIDItself covers `sonar start` in the
// foreground: no process table should be needed.
func TestRunAttributesTheRegisteredPIDItself(t *testing.T) {
	r := testRegistry(300)
	r.Register(Record{ID: "a", Token: "ta", PID: 300, Group: "itest", Name: "web"})
	r.Parents = func() map[int]int { t.Fatal("the process table should not be needed"); return nil }

	run, ok := r.Run(state.Port{PID: 300})
	if !ok || run.ID != "a" || run.Name != "web" || run.RootPID != 300 {
		t.Fatalf("Run = %+v/%v, want the registered run", run, ok)
	}
}

func TestRunUsesTheDirectParentBeforeTheProcessTable(t *testing.T) {
	r := testRegistry(100, 200)
	r.Register(Record{ID: "a", Token: "ta", PID: 100, Group: "itest", Name: "web"})
	r.Parents = func() map[int]int { t.Fatal("the process table should not be needed"); return nil }

	if run, ok := r.Run(state.Port{PID: 200, PPID: 100}); !ok || run.Group != "itest" {
		t.Fatalf("Run(child) = %+v/%v", run, ok)
	}
}

func TestRunFallsBackToTheScannersOwnAttribution(t *testing.T) {
	r := testRegistry()
	run, ok := r.Run(state.Port{
		PID: 400,
		Run: &state.Run{ID: "x", Group: "from-file", Name: "web", RootPID: 100},
	})
	if !ok || run.Group != "from-file" || run.Name != "web" {
		t.Fatalf("Run = %+v/%v", run, ok)
	}
}

// TestPIDGuardBlocksAReusedPID and passes verified and unknown pids.
func TestPIDGuardBlocksAReusedPID(t *testing.T) {
	birth1 := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	r := identityRegistry(100, "different", birth1.Add(time.Hour))
	r.Register(Record{ID: "a", Token: "old-token", PID: 100, Group: "g", Name: "web", Birth: birth1})
	// Unknown pid passes, stale pid blocked.
	got := r.PIDGuard([]int{100, 200})
	if len(got) != 1 || got[0] != 200 {
		t.Fatalf("PIDGuard = %v, want only pid 200", got)
	}
	if !hasDiag(r, DiagStaleIdentity, 100) {
		t.Fatalf("no stale_identity diagnostic: %+v", r.Diagnostics())
	}
}

// hasDiag reports whether r carries a code diagnostic for pid.
func hasDiag(r *Registry, code string, pid int) bool {
	for _, d := range r.Diagnostics() {
		if d.Code == code && d.PID == pid {
			return true
		}
	}
	return false
}
