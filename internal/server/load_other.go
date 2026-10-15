//go:build !linux

package server

// loadPerCPU is not known on this system.
func loadPerCPU() (float64, bool) { return 0, false }
