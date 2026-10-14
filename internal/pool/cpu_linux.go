//go:build linux

package pool

import (
	"os"
	"strconv"
	"strings"
)

// cpuSeconds returns the user + system CPU time of a process, from
// /proc/<pid>/stat, or 0.
func cpuSeconds(pid int) float64 {
	if pid <= 0 {
		return 0
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// The command name (field 2) is in parentheses and can hold spaces.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0
	}
	f := strings.Fields(s[i+1:])
	// f[0] is field 3 (state); utime and stime are fields 14 and 15.
	if len(f) < 13 {
		return 0
	}
	ut, _ := strconv.ParseUint(f[11], 10, 64)
	st, _ := strconv.ParseUint(f[12], 10, 64)
	const clkTck = 100 // USER_HZ on Linux
	return float64(ut+st) / clkTck
}
