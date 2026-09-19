package daemon

import (
	"strconv"

	"github.com/raskrebs/sonar/internal/killer"
	"github.com/raskrebs/sonar/internal/state"
)

// pidGuarder is the optional identity gate a run registry can implement. It
// lets the kill path verify a PID-addressed target before signaling, so a
// reused PID is never killed as the run that used to own it.
type pidGuarder interface {
	PIDGuard(pids []int) []int
}

// guardPIDTargets filters direct-PID killer targets through the registry's
// identity check, before anything is marked stopping or signaled. A target at
// a PID a different process now owns becomes a failed stale_identity row and
// is never signaled; dead PIDs pass through so the killer reports them as not
// found, as it does for an unknown PID.
func guardPIDTargets(reg RunRegistry, targets []killer.Target) ([]killer.Target, []state.KillResult) {
	var pids []int
	seen := map[int]bool{}
	for _, t := range targets {
		if t.PID > 0 && !seen[t.PID] {
			seen[t.PID] = true
			pids = append(pids, t.PID)
		}
	}
	if len(pids) == 0 {
		return targets, nil
	}
	g, ok := reg.(pidGuarder)
	if !ok {
		return targets, nil
	}
	safe := map[int]bool{}
	for _, pid := range g.PIDGuard(pids) {
		safe[pid] = true
	}
	kept := make([]killer.Target, 0, len(targets))
	var blocked []state.KillResult
	for _, t := range targets {
		if t.PID > 0 && !safe[t.PID] {
			blocked = append(blocked, state.KillResult{
				Host:   "localhost",
				PID:    t.PID,
				Method: state.MethodNone,
				OK:     false,
				Error: "stale_identity: pid " + strconv.Itoa(t.PID) +
					" is now a different process; nothing was signaled",
			})
			continue
		}
		kept = append(kept, t)
	}
	return kept, blocked
}
