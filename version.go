package main

import "runtime/debug"

// version reads the build info: Go stamps the module version from the VCS
// tag (v1.0.0 on a clean tagged checkout, a pseudo-version past it) and
// the commit.
func version() string {
	v := "(unknown)"
	if bi, ok := debug.ReadBuildInfo(); ok {
		v = bi.Main.Version
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				v += " (" + s.Value[:7] + ")"
			}
		}
	}
	return "deskpresence " + v
}
