//go:build !linux

package pool

func cpuSeconds(int) float64 { return 0 }
