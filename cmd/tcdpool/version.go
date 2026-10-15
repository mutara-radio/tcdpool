package main

import "runtime/debug"

// version is set at build time:
//
//	go build -ldflags "-X main.version=v1.2.3" ./cmd/tcdpool
var version string

// buildVersion returns the version for GET /v1/status: the -X value,
// else the VCS revision from the build info, else "dev".
func buildVersion() string {
	if version != "" {
		return version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	switch {
	case rev == "":
		return "dev"
	case dirty:
		return rev + "-dirty"
	}
	return rev
}
