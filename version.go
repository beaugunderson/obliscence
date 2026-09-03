package main

import "runtime/debug"

// version is stamped by the linker (-X main.version=...) in release and make
// builds. Empty for a plain `go build` or `go install`.
var version string

// versionString reports the stamped version, falling back to what the Go
// toolchain embedded: the module version for `go install pkg@vX.Y.Z`, or the
// VCS revision for a build from a checkout.
func versionString() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	var rev, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if rev == "" {
		return "devel"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if modified == "true" {
		rev += "-dirty"
	}
	return "devel (" + rev + ")"
}
