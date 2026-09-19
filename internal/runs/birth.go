package runs

import (
	"bytes"
	"strconv"
	"strings"
	"time"
)

// Birth evidence conversion. These are pure functions: the platform probe
// files only gather the raw numbers the kernel reports, so the same logic is
// testable from fixture strings on every platform.

// LinuxBirth is the boot epoch (btime, seconds since 1970) plus the process
// starttime in clock ticks since boot. hz is USER_HZ (100 on every mainstream
// Linux port).
func LinuxBirth(btimeSec, startJiffies, hz int64) time.Time {
	if hz <= 0 {
		hz = 100
	}
	sec := btimeSec + startJiffies/hz
	nsec := (startJiffies % hz) * int64(time.Second) / hz
	return time.Unix(sec, nsec).UTC()
}

// DarwinBirth converts the kinfo_proc start timeval (seconds, microseconds).
func DarwinBirth(sec, usec int64) time.Time {
	return time.Unix(sec, usec*int64(time.Microsecond)).UTC()
}

// windowsEpoch100ns is the offset between the Windows FILETIME epoch
// (1601-01-01) and the Unix epoch (1970-01-01), in 100ns ticks.
const windowsEpoch100ns = 116444736000000000

// WindowsBirth converts a kernel32 process creation FILETIME (100ns ticks
// since 1601-01-01 UTC).
func WindowsBirth(ft uint64) time.Time {
	if ft < windowsEpoch100ns {
		return time.Time{}
	}
	unix100ns := int64(ft - windowsEpoch100ns)
	return time.Unix(unix100ns/int64(1e7), (unix100ns%int64(1e7))*100).UTC()
}

// ParseBTime pulls the boot epoch (seconds) out of /proc/stat's btime line.
func ParseBTime(data []byte) (int64, bool) {
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "btime ") {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
		return v, err == nil
	}
	return 0, false
}

// ParseLinuxProcStat extracts starttime (field 22, clock ticks since boot)
// from /proc/<pid>/stat. The comm field (2) is parenthesized and may itself
// contain spaces and parentheses, so fields are counted after the last ')'.
func ParseLinuxProcStat(data []byte) (int64, bool) {
	close := bytes.LastIndexByte(data, ')')
	if close < 0 {
		return 0, false
	}
	fields := strings.Fields(string(data[close+1:]))
	// After comm: field 3 is state, ..., field 22 starttime -> index 19.
	if len(fields) < 20 {
		return 0, false
	}
	v, err := strconv.ParseInt(fields[19], 10, 64)
	return v, err == nil
}

// environToken extracts the start token from a /proc/<pid>/environ block
// (NUL-separated KEY=VALUE entries).
func environToken(data []byte) string {
	const prefix = EnvStartToken + "="
	for _, kv := range strings.Split(string(data), "\x00") {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

// EnvStartToken is the environment variable carrying a run's start token.
// Kept here (rather than in spawn) because the Linux probe reads it back from
// /proc/<pid>/environ without importing the spawn package.
const EnvStartToken = "SONAR_START_TOKEN"

// ParseTime parses a persisted RFC3339 timestamp, returning the zero time for
// an empty or malformed string.
func ParseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// FormatTime renders t for persistence; "" for the zero time.
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
