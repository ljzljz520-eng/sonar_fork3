//go:build windows

package runs

import (
	"golang.org/x/sys/windows"
)

func probeOS(pid int) Info {
	info := Info{PID: pid}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return info
	}
	defer windows.CloseHandle(h)

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return info
	}
	info.Alive = true
	ft := uint64(creation.HighDateTime)<<32 | uint64(creation.LowDateTime)
	info.Birth = WindowsBirth(ft)
	return info
}
