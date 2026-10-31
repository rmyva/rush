package version

import (
	"runtime/debug"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVersionLine(t *testing.T) {
	t.Parallel()
	require.True(t, strings.HasPrefix(VersionLine(), forkBaseVersion),
		"VersionLine must start with the release-line summary, got %q", VersionLine())
	require.False(t, strings.HasPrefix(VersionLine(), "v"), "VersionLine must not carry a \"v\" prefix")
}

func TestInjectedReleaseVersion(t *testing.T) {
	t.Parallel()
	require.Equal(t, "0.2.0-alpha.1", injectedReleaseVersion("v0.2.0-alpha.1"))
	require.Empty(t, injectedReleaseVersion("devel"))
	require.Empty(t, injectedReleaseVersion("abc123-0.2.0-alpha.1"))
}

func TestVersionLineUsesInjectedReleaseVersion(t *testing.T) {
	t.Parallel()
	require.Equal(t, "release 0.2.0-alpha.1 (c34a7334)", versionLine("v0.2.0-alpha.1", "c34a7334aabbccdd", ""))
}

func TestVersionLineReleaseMarkerDistinguishesFallback(t *testing.T) {
	t.Parallel()
	expected := "release " + forkBaseVersion
	injected := versionLine("v"+forkBaseVersion, "", "")
	devel := versionLine("devel", "", "")
	require.Equal(t, expected, injected)
	require.Equal(t, forkBaseVersion, devel)
	require.NotEqual(t, injected, devel)
}

// TestBuildProvenanceSuffix pins the provenance tail appended to VersionLine.
// It exists because the release-line part alone ("0.2.0-alpha.1") is identical
// for every build of that line, so a deployed binary carried no evidence of
// which source tree produced it — making "is the running binary actually built
// from current source?" unanswerable from the binary itself, a question that
// got answered wrong more than once. Each component must be OMITTED (not
// printed as "unknown") when unavailable, so builds without VCS metadata still
// read cleanly.
func TestBuildProvenanceSuffix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		commit    string
		buildTime string
		want      string
	}{
		{
			name:      "both present",
			commit:    "c34a7334aabbccdd1122334455667788",
			buildTime: "2026-07-31_16:24:26",
			want:      " (c34a7334, built 2026-07-31_16:24:26)",
		},
		{
			name:      "full 40-char hash is shortened",
			commit:    "0123456789abcdef0123456789abcdef01234567",
			buildTime: "",
			want:      " (01234567)",
		},
		{
			name:      "already-short hash is left alone",
			commit:    "abc123",
			buildTime: "",
			want:      " (abc123)",
		},
		{
			name:      "full dirty hash retains marker after truncation",
			commit:    "0123456789abcdef0123456789abcdef01234567-dirty",
			buildTime: "",
			want:      " (01234567-dirty)",
		},
		{
			name:      "build time only",
			commit:    "unknown",
			buildTime: "2026-07-31_16:24:26",
			want:      " (built 2026-07-31_16:24:26)",
		},
		{
			name:      "nothing known yields no suffix at all",
			commit:    "unknown",
			buildTime: "",
			want:      "",
		},
		{
			name:      "empty commit is treated as unknown",
			commit:    "",
			buildTime: "",
			want:      "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, buildProvenanceSuffix(tt.commit, tt.buildTime))
		})
	}
}

func TestDeriveDevVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		revision string
		want     string
	}{
		{
			name:     "no vcs info keeps default",
			revision: "",
			want:     "",
		},
		{
			name:     "clean commit is shortened",
			revision: "06c807842604abcdef1234567890abcdef123456",
			want:     "06c8078-" + forkBaseVersion,
		},
		{
			name:     "short revision is not truncated further",
			revision: "abc1234",
			want:     "abc1234-" + forkBaseVersion,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, deriveDevVersion(tt.revision))
		})
	}
}

// TestDeriveDevVersionNoDevelOrDirtyMarker asserts the output never contains
// a "devel" marker or a dirty/modified marker of any kind: the version
// string for a clean checkout and one built with uncommitted changes are
// identical, and neither ever says "devel". Both markers were deliberately
// removed from this format.
func TestDeriveDevVersionNoDevelOrDirtyMarker(t *testing.T) {
	t.Parallel()
	// deriveDevVersion no longer takes a modified flag at all; the only way to
	// distinguish clean vs dirty would be a separate parameter, which we do not
	// pass. Both cases must produce the exact same string.
	clean := deriveDevVersion("06c807842604abcdef1234567890abcdef123456")
	require.Equal(t, "06c8078-"+forkBaseVersion, clean)
	require.NotContains(t, clean, "dirty")
	require.NotContains(t, clean, "modified")
	require.NotContains(t, clean, "devel")
}

func TestReadVCS(t *testing.T) {
	t.Parallel()
	info := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "fullcommitabcd"},
			{Key: "vcs.modified", Value: "true"},
			{Key: "GOOS", Value: "linux"},
		},
	}
	v := readVCS(info)
	require.Equal(t, "fullcommitabcd", v.revision)
	require.Equal(t, "true", v.modified)
}

func TestReadVCS_Empty(t *testing.T) {
	t.Parallel()
	// A build with no embedded VCS metadata (e.g. built outside a checkout)
	// yields a zero value rather than garbage.
	require.Equal(t, vcsInfo{}, readVCS(&debug.BuildInfo{}))
}

func TestResolveVersion(t *testing.T) {
	t.Parallel()
	// buildInfo builds a *debug.BuildInfo for a binary produced by `go build .`
	// from a checkout: Main.Version is "(devel)" and the toolchain embeds VCS
	// settings (revision + modified flag).
	buildInfo := func(revision, modified string) *debug.BuildInfo {
		return &debug.BuildInfo{
			Main: debug.Module{Version: "(devel)"},
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: revision},
				{Key: "vcs.modified", Value: modified},
			},
		}
	}
	tests := []struct {
		name          string
		defaultVer    string // ldflags-injected Version (or "devel")
		defaultCommit string // ldflags-injected Commit (or "unknown")
		info          *debug.BuildInfo
		wantVersion   string
		wantCommit    string
	}{
		{
			// The release/packaged build path: ldflags injects "v0.1.0". It
			// MUST survive — init() must never clobber it, even though VCS
			// metadata is present and Main.Version is "(devel)".
			name:       "ldflags-injected version is preserved (npm/packaged)",
			defaultVer: "v0.1.0", defaultCommit: "unknown",
			info:        buildInfo("06c807842604", "false"),
			wantVersion: "v0.1.0",
			wantCommit:  "06c807842604", // populated from VCS (was "unknown")
		},
		{
			name:       "ldflags-injected version and commit both preserved",
			defaultVer: "v1.2.3", defaultCommit: "deadbeef",
			info:        buildInfo("06c807842604", "true"),
			wantVersion: "v1.2.3",
			wantCommit:  "deadbeef", // not "unknown", so VCS does not override
		},
		{
			name:       "ldflags-injected version wins over module version",
			defaultVer: "v1.2.3", defaultCommit: "unknown",
			info: &debug.BuildInfo{
				Main:     debug.Module{Version: "v0.2.0"},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abcdef123456"}},
			},
			wantVersion: "v1.2.3",
			wantCommit:  "abcdef123456",
		},
		{
			// `go install pkg@version` resolves a real module version that wins
			// over the "devel" default.
			name:       "go install main version wins",
			defaultVer: "devel", defaultCommit: "unknown",
			info: &debug.BuildInfo{
				Main:     debug.Module{Version: "v0.2.0"},
				Settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "false"}},
			},
			wantVersion: "v0.2.0",
			wantCommit:  "unknown",
		},
		{
			// `go install pkg@version` where the module resolves to a
			// pseudo-version built on top of a real upstream tag: the raw
			// pseudo-version is rejected, and no base-tag prefix is recovered
			// or prepended — the derived string is always the bare
			// "<hash>-<forkBaseVersion>" form.
			name:       "go install pseudo-version produces bare derived version",
			defaultVer: "devel", defaultCommit: "unknown",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "v0.72.1-0.20260628185628-e47711a0e3e4"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "e47711a0e3e4abcdef1234567890abcdef123456"},
					{Key: "vcs.modified", Value: "false"},
				},
			},
			wantVersion: "e47711a-" + forkBaseVersion,
			wantCommit:  "e47711a0e3e4abcdef1234567890abcdef123456",
		},
		{
			// Same path but the working tree was dirty at build time: still no
			// base-tag prefix, and no "-dirty" marker is appended.
			name:       "go install pseudo-version with dirty tree has no dirty marker",
			defaultVer: "devel", defaultCommit: "unknown",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "v0.72.1-0.20260628185628-e47711a0e3e4+dirty"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "e47711a0e3e4abcdef1234567890abcdef123456"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			wantVersion: "e47711a-" + forkBaseVersion,
			wantCommit:  "e47711a0e3e4abcdef1234567890abcdef123456",
		},
		{
			// Local checkout builds can expose a v0.0.0 pseudo-version through
			// BuildInfo. That pseudo-version never had a meaningful base tag
			// anyway, so the derived dev string is tag-less, and no dirty
			// marker is appended.
			name:       "local v0.0.0 pseudo module version is treated as tagless dev",
			defaultVer: "devel", defaultCommit: "unknown",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "v0.0.0-20260705112643-90c57af7ca7a+dirty"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "90c57af7ca7a5c50d2ee959eaa1668120b4b1729"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			wantVersion: "90c57af-" + forkBaseVersion,
			wantCommit:  "90c57af7ca7a5c50d2ee959eaa1668120b4b1729",
		},
		{
			// Plain `go build .` from a checkout: Main.Version is "(devel)",
			// there is no base tag to extract, and the derived version is the
			// bare "<hash>-<forkBaseVersion>" with no "devel" or dirty marker even
			// when the tree was dirty.
			name:       "plain go build derives tagless dev version from vcs",
			defaultVer: "devel", defaultCommit: "unknown",
			info:        buildInfo("06c807842604", "false"),
			wantVersion: "06c8078-" + forkBaseVersion,
			wantCommit:  "06c807842604",
		},
		{
			name:       "plain go build with dirty tree produces no dirty marker",
			defaultVer: "devel", defaultCommit: "unknown",
			info:        buildInfo("06c807842604", "true"),
			wantVersion: "06c8078-" + forkBaseVersion,
			wantCommit:  "06c807842604",
		},
		{
			name:       "dev build without vcs metadata keeps devel default",
			defaultVer: "devel", defaultCommit: "unknown",
			info:        &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			wantVersion: "devel",
			wantCommit:  "unknown",
		},
		{
			name:       "nil build info keeps defaults",
			defaultVer: "devel", defaultCommit: "unknown",
			info:        nil,
			wantVersion: "devel",
			wantCommit:  "unknown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotVersion, gotCommit := resolveVersion(tt.defaultVer, tt.defaultCommit, tt.info)
			require.Equal(t, tt.wantVersion, gotVersion, "version")
			require.Equal(t, tt.wantCommit, gotCommit, "commit")
		})
	}
}

func TestUsableModuleVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		version string
		want    bool
	}{
		{version: "", want: false},
		{version: "(devel)", want: false},
		{version: "v0.0.0-20260705112643-90c57af7ca7a", want: false},
		{version: "v0.0.0-20260705112643-90c57af7ca7a+dirty", want: false},
		{version: "0.0.0-20260705112643-90c57af7ca7a", want: false},
		// Pseudo-version built on top of a real prior tag (no +dirty). This is
		// the shape Go emits for a checkout whose nearest tag is a real release,
		// e.g. v0.72.1-0.<timestamp>-<12-hex-commit>. Still ugly and unhelpful,
		// so reject it from direct display; its base tag is deliberately never
		// recovered or shown (see deriveDevVersion).
		{version: "v0.72.1-0.20260628185628-e47711a0e3e4", want: false},
		// Same shape but with a +dirty marker.
		{version: "v0.72.1-0.20260628185628-e47711a0e3e4+dirty", want: false},
		{version: "v0.2.0", want: true},
		{version: "v0.2.1-rc.1", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, usableModuleVersion(tt.version))
		})
	}
}

func TestFormatFullVersion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		version string
		buildID string
		want    string
	}{
		{name: "release version with build id", version: "v0.1.0", buildID: "abc123", want: "v0.1.0 (abc123)"},
		{name: "dev version with build id", version: "devel-06c8078", buildID: "xyz", want: "devel-06c8078 (xyz)"},
		{name: "no build id omits suffix", version: "v0.1.0", buildID: "", want: "v0.1.0"},
		{name: "unknown build id omits suffix", version: "v0.1.0", buildID: "unknown", want: "v0.1.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, formatFullVersion(tt.version, tt.buildID))
		})
	}
}
