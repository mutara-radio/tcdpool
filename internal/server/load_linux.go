//go:build linux

package server

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// loadPerCPU returns the 1-minute load average from /proc/loadavg
// divided by the number of CPUs.
func loadPerCPU() (float64, bool) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, false
	}
	l, err := strconv.ParseFloat(f[0], 64)
	if err != nil || l < 0 {
		return 0, false
	}
	return l / float64(runtime.NumCPU()), true
}
