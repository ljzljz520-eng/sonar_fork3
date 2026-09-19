package runs

import "time"

// Info is the identity evidence a process at PID carries right now.
type Info struct {
	PID int
	// Alive says a process with this PID currently exists.
	Alive bool
	// Birth is the kernel-reported process creation time; zero when the
	// platform probe could not read it.
	Birth time.Time
	// Token is the start token the process's environment carries (Linux reads
	// /proc/<pid>/environ); empty when unreadable or unset.
	Token string
}

// Status is the outcome of matching a recorded run against the live process.
type Status int

const (
	// StatusDead: no process with this PID exists.
	StatusDead Status = iota
	// StatusAlive: the PID's process matches the recorded identity.
	StatusAlive
	// StatusStale: a process exists but its identity differs — the PID was
	// reused after the recorded run ended.
	StatusStale
)

// probeFn is the active process probe, implemented per platform
// (probe_linux.go, probe_darwin.go, probe_windows.go). Tests replace it.
var probeFn = probeOS

// Probe gathers identity evidence for PID.
func Probe(pid int) Info {
	if pid <= 0 {
		return Info{PID: pid}
	}
	return probeFn(pid)
}

// VerifyInfo matches already-collected evidence Info against the identity a
// run was recorded with (token, birth). Evidence is combined conservatively:
// a mismatching token is stale outright; birth times are compared when both
// sides carry them; when the platform yields no usable evidence at all, the
// run is reported alive rather than dropped, so an unsupported platform never
// prunes runs it merely cannot verify.
func VerifyInfo(info Info, token string, birth time.Time) Status {
	if !info.Alive {
		return StatusDead
	}
	if info.Token != "" {
		// The live process proves its token: the recorded one must match.
		if token == "" || info.Token != token {
			return StatusStale
		}
		return StatusAlive
	}
	if !birth.IsZero() {
		if info.Birth.IsZero() {
			return StatusAlive // cannot re-verify; don't prune
		}
		if !info.Birth.Equal(birth) {
			return StatusStale
		}
		return StatusAlive
	}
	return StatusAlive
}

// Verify matches the run (pid, token, birth) against the live system.
func Verify(pid int, token string, birth time.Time) Status {
	return VerifyInfo(Probe(pid), token, birth)
}
