// Package version holds build information injected with -ldflags.
package version

import (
	"fmt"
	"runtime"
)

var (
	// Version is the semantic version, e.g. v0.1.0.
	Version = "dev"
	// GitCommit is the commit the binary was built from.
	GitCommit = "unknown"
	// GitTreeState is "clean" or "dirty".
	GitTreeState = "unknown"
	// BuildDate is the RFC3339 build time.
	BuildDate = "unknown"
)

// Info is the full build information.
type Info struct {
	Version      string `json:"version"`
	GitCommit    string `json:"gitCommit"`
	GitTreeState string `json:"gitTreeState"`
	BuildDate    string `json:"buildDate"`
	GoVersion    string `json:"goVersion"`
	Compiler     string `json:"compiler"`
	Platform     string `json:"platform"`
}

// Get returns the build information.
func Get() Info {
	return Info{
		Version:      Version,
		GitCommit:    GitCommit,
		GitTreeState: GitTreeState,
		BuildDate:    BuildDate,
		GoVersion:    runtime.Version(),
		Compiler:     runtime.Compiler,
		Platform:     fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
	}
}
