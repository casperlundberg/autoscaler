// Package buildinfo is which code this process is: the release it was built
// as, and the commit it was built from.
//
// It exists so a result can name the code that produced it. A recorded run
// that says only "the autoscaler" cannot be reproduced once the autoscaler has
// changed; one that names a commit can be rebuilt from it.
package buildinfo

import (
	"runtime"
	"runtime/debug"
)

// Stamped at link time, by the Makefile and the Dockerfile:
//
//	-X github.com/casperlundberg/autoscaler/internal/buildinfo.version=1.3.0
//	-X github.com/casperlundberg/autoscaler/internal/buildinfo.commit=<full hash>
//	-X github.com/casperlundberg/autoscaler/internal/buildinfo.modified=false
var (
	version  string
	commit   string
	modified string
)

// Info identifies a build.
type Info struct {
	// Version is the semantic version from scripts/version.sh: a release such
	// as 1.3.0, or a development build such as 1.3.1-dev.2+abc1234. Empty when
	// the build was not stamped.
	Version string `json:"version"`

	// Commit is the full hash of the commit built. Empty when unknown.
	Commit string `json:"commit"`

	// Modified is whether the tree had changes the commit does not contain.
	// A modified build cannot be rebuilt from its commit alone.
	Modified bool `json:"modified"`

	GoVersion string `json:"go_version"`

	// Platform is the operating system and architecture, as GOOS/GOARCH.
	// Floating point can round differently across architectures, so a replay
	// that has to match to the bit has to know which one it is matching.
	Platform string `json:"platform"`
}

// Read is this process's build.
func Read() Info {
	return read(version, commit, modified, debug.ReadBuildInfo)
}

func read(version, commit, modified string, recorded func() (*debug.BuildInfo, bool)) Info {
	info := Info{
		Version: version, Commit: commit, Modified: modified == "true",
		GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}

	// Go's own record: the fallback for an unstamped build. Only `go build
	// -buildvcs=true` reliably embeds it; `make build` and CI stamp instead.
	build, ok := recorded()
	if !ok || build == nil {
		return info
	}
	if build.GoVersion != "" {
		info.GoVersion = build.GoVersion
	}
	if commit != "" {
		// Stamped: the stamp is the authority, including about modification.
		return info
	}
	for _, setting := range build.Settings {
		switch setting.Key {
		case "vcs.revision":
			info.Commit = setting.Value
		case "vcs.modified":
			info.Modified = setting.Value == "true"
		}
	}
	return info
}
