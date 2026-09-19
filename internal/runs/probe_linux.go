//go:build linux

package runs

import (
	"os"
	"strconv"
	"sync"
)

// linuxHZ is USER_HZ: 100 on every mainstream Linux architecture.
const linuxHZ = 100

func probeOS(pid int) Info {
	info := Info{PID: pid}
	stat, err := os.ReadFile(procFile(pid, "stat"))
	if err != nil {
		return info
	}
	info.Alive = true
	startJ, ok := ParseLinuxProcStat(stat)
	if ok {
		if btime, bOK := bootTime(); bOK {
			info.Birth = LinuxBirth(btime, startJ, linuxHZ)
		}
	}
	if env, err := os.ReadFile(procFile(pid, "environ")); err == nil {
		info.Token = environToken(env)
	}
	return info
}

func procFile(pid int, leaf string) string {
	return "/proc/" + strconv.Itoa(pid) + "/" + leaf
}

var (
	bootOnce sync.Once
	bootSec  int64
	bootOK   bool
)

// bootTime returns /proc/stat's btime; it is constant for the machine's
// uptime, so it is read at most once.
func bootTime() (int64, bool) {
	bootOnce.Do(func() {
		data, err := os.ReadFile("/proc/stat")
		if err != nil {
			return
		}
		bootSec, bootOK = ParseBTime(data)
	})
	return bootSec, bootOK
}
