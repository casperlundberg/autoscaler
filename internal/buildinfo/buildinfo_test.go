package buildinfo

import (
	"runtime/debug"
	"testing"
)

func vcs(revision, modified string) func() (*debug.BuildInfo, bool) {
	return func() (*debug.BuildInfo, bool) {
		return &debug.BuildInfo{GoVersion: "go1.24.1", Settings: []debug.BuildSetting{
			{Key: "vcs", Value: "git"}, {Key: "vcs.revision", Value: revision}, {Key: "vcs.modified", Value: modified},
		}}, true
	}
}

// CI stamps the version and commit at link time, because a Docker build has
// no .git to read them from.
func TestAStampedBuildReportsWhatItWasStampedWith(t *testing.T) {
	got := read("1.3.0", "9caaa12b934d2f03af94e79341e72d08bf230cf3", "false", vcs("ignored", "true"))

	if got.Version != "1.3.0" || got.Commit != "9caaa12b934d2f03af94e79341e72d08bf230cf3" || got.Modified {
		t.Errorf("read() = %+v", got)
	}
}

// A build nobody stamped — `go build -buildvcs=true` in a checkout — still
// carries the commit and whether the tree was clean, as Go recorded them.
func TestAnUnstampedBuildFallsBackToWhatGoRecorded(t *testing.T) {
	got := read("", "", "", vcs("abc123", "true"))

	if got.Version != "" || got.Commit != "abc123" || !got.Modified {
		t.Errorf("read() = %+v", got)
	}
	if got.GoVersion != "go1.24.1" {
		t.Errorf("GoVersion = %q", got.GoVersion)
	}
	if got.Platform == "" {
		t.Error("Platform is empty")
	}
}

// Built outside any checkout, it knows nothing, and says so rather than
// inventing a commit.
func TestABuildWithNoRecordSaysNothing(t *testing.T) {
	got := read("", "", "", func() (*debug.BuildInfo, bool) { return nil, false })

	if got.Version != "" || got.Commit != "" || got.Modified {
		t.Errorf("read() = %+v", got)
	}
}

// A stamp that says modified is believed: the Makefile stamps a dirty working
// tree honestly rather than pretending it was a clean commit.
func TestAStampOfModifiedIsBelieved(t *testing.T) {
	if got := read("1.3.1-dev.2+abc1234.dirty", "abc", "true", vcs("x", "false")); !got.Modified {
		t.Errorf("read() = %+v, want modified", got)
	}
}
