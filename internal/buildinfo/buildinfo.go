// Package buildinfo identifies artifacts without requiring runtime configuration.
package buildinfo

import (
	"fmt"
	"io"
	"runtime/debug"
)

// Revision is set by Docker/Make. Direct Go builds fall back to VCS build info.
var Revision = "unknown"

func SourceRevision() string {
	if Revision != "unknown" && Revision != "" {
		return Revision
	}
	revision, dirty := "unknown", false
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	if dirty && revision != "unknown" {
		revision += "-dirty"
	}
	return revision
}

func PrintVersion(args []string, out io.Writer) bool {
	if len(args) != 2 || args[1] != "--version" {
		return false
	}
	fmt.Fprintln(out, SourceRevision())
	return true
}
