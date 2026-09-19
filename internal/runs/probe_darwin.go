//go:build darwin

package runs

import (
	"golang.org/x/sys/unix"
)

func probeOS(pid int) Info {
	info := Info{PID: pid}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return info
	}
	info.Alive = true
	info.Birth = DarwinBirth(int64(kp.Proc.P_starttime.Sec), int64(kp.Proc.P_starttime.Usec))
	// The macOS kernel exposes neither another process's environment nor a
	// start token; birth time is the identity evidence.
	return info
}
