// Package refraktal is a meta-harness: one prompt in, a spectrum of agents out.
//
// The CLI in cmd/refraktal is a thin front door over this package.
package refraktal

import "runtime/debug"

// version is stamped into release builds:
//
//	go build -ldflags "-X refraktal.dev/refraktal.version=v0.1.0" ./cmd/refraktal
var version = ""

// Version is this build's version: the release stamp, else the module
// version `go install refraktal.dev/refraktal/cmd/refraktal@<version>`
// recorded, else "dev" for a local build. doctor reports it.
var Version = resolveVersion(version, debug.ReadBuildInfo)

func resolveVersion(stamp string, buildInfo func() (*debug.BuildInfo, bool)) string {
	if stamp != "" {
		return stamp
	}
	if bi, ok := buildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}
