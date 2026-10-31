package version

import (
	"fmt"
	"os"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
)

// forkBaseVersion is the fork's current release-line version, mirrored by hand
// from the "version" field in npm/rush/package.json. It is embedded into local
// dev-build version strings so the operator can see at a glance which release
// line a devel binary was built from. This fork bumps versions deliberately and
// manually (see CLAUDE.md at the repo root), so this constant must be kept in
// lockstep with npm/rush/package.json on every bump.
const forkBaseVersion = "0.2.0-alpha.19"

// VersionLine is what `rush --version`/`rush version` prints: a release-only
// marker and the release-line version, with no "v" prefix, followed by this
// specific build's provenance — short commit hash and build time — e.g.
//
//	release 0.2.0-alpha.0 (c34a7334, built 2026-07-31 16:24:26)
//
// The marker distinguishes an injected release version from a development
// fallback even when both versions have the same semver. The remaining
// provenance and upstream-watermark notes below describe common suffix rules.
//
// This used to also append "@<UpstreamTriagedVersion>" — how far
// charmbracelet/crush had been triaged into this fork — but that constant
// and the suffix were removed: the fork no longer tracks or advertises a
// point-in-time relationship to upstream in its version string. Per-commit
// PORT/EVAL/SKIP decisions during merges (see CLAUDE.md's Merge Workflow)
// are unaffected; only the runtime-visible watermark is gone.
//
// The provenance suffix exists because the release-line part alone is
// identical across every build of that line: a deployed binary was
// indistinguishable from any other, so "is this binary actually built from
// the current source?" could not be answered from the binary itself. Each
// component is omitted when genuinely unknown rather than printed as
// "unknown", so the line stays clean for builds that carry no VCS metadata.
func VersionLine() string {
	return versionLine(Version, Commit, BuildTime)
}

func versionLine(version, commit, buildTime string) string {
	if releaseVersion := injectedReleaseVersion(version); releaseVersion != "" {
		return "release " + releaseVersion + buildProvenanceSuffix(commit, buildTime)
	}
	return forkBaseVersion + buildProvenanceSuffix(commit, buildTime)
}

// injectedReleaseVersion exposes the actual ldflags-injected semver for
// release binaries while leaving locally-derived development versions alone.
func injectedReleaseVersion(v string) string {
	if !strings.HasPrefix(v, "v") || len(v) < 2 || v[1] < '0' || v[1] > '9' {
		return ""
	}
	return strings.TrimPrefix(v, "v")
}

// buildProvenanceSuffix formats the " (<commit>, built <time>)" tail of
// VersionLine. Split out as a pure function so the omit-when-unknown rules are
// unit-testable without mutating package-level build vars.
func buildProvenanceSuffix(commit, buildTime string) string {
	var parts []string
	if commit != "" && commit != "unknown" {
		dirty := strings.HasSuffix(commit, "-dirty")
		if dirty {
			commit = strings.TrimSuffix(commit, "-dirty")
		}
		short := shortCommit(commit)
		if dirty {
			short += "-dirty"
		}
		parts = append(parts, short)
	}
	if buildTime != "" && buildTime != "unknown" {
		parts = append(parts, "built "+buildTime)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// shortCommitLen is how many hex characters of a commit hash to show — the
// git default for `--short`, long enough to be unambiguous in practice and
// short enough to keep the version line readable.
const shortCommitLen = 8

// shortCommit truncates a full 40-char VCS revision to shortCommitLen, and
// leaves anything already shorter (e.g. an ldflags-injected short hash) alone.
func shortCommit(commit string) string {
	if len(commit) > shortCommitLen {
		return commit[:shortCommitLen]
	}
	return commit
}

// Build-time parameters set via -ldflags. These act as overrides: when a
// release/packaging build injects them (see .goreleaser.yml and the
// publish-fork-npm workflow), the values below are replaced and treated as
// authoritative. When they are left at their defaults (local `go build` /
// `make build` / `go run`), init() fills in meaningful values from the build
// metadata embedded by the Go toolchain.

var (
	Version = "devel"
	Commit  = "unknown"
	// BuildID is a unique identifier for this build. For release builds it
	// equals Commit; for development builds (go run / go build without
	// ldflags) it is derived from the executable's modification time, which
	// changes on every recompilation.
	//
	// Fork merge note (origin/main 2026-05-16): upstream introduced this in
	// 9e126c27 to detect stale REST servers during development. We keep it
	// because the same problem applies to our WebSocket server: when the dev
	// loop rebuilds the binary, the browser tab may still be talking to the
	// previous process. BuildID gives the WUI a cheap freshness signal.
	BuildID = ""

	// BuildTime is the human-readable moment this binary was linked, injected
	// via ldflags by build.go (and left empty for plain `go build`/`go run`).
	//
	// It exists because `rush --version` used to print only the release-line
	// summary ("0.2.0-alpha.0"), which is IDENTICAL for every build of that
	// line — so a deployed binary carried no evidence of which source tree it
	// came from. Answering "is the binary I'm running actually the one I just
	// built?" then came down to guesswork or memory, and got answered wrong
	// more than once. Together with Commit this makes provenance readable at
	// a glance.
	//
	// When not injected, init() derives it from the executable's own mtime —
	// the same signal deriveBuildID already uses, and accurate for any
	// locally-built binary.
	BuildTime = ""
)

// FullVersion is consumed by the web UI's status bar.
//
// Fork merge note: upstream removed BuildTime in favour of BuildID. We keep
// the parenthesised-suffix shape that the WUI already renders and just feed
// it the new value when available.
func FullVersion() string {
	return formatFullVersion(Version, BuildID)
}

// formatFullVersion is the pure formatter behind [FullVersion], split out so
// it can be unit-tested without touching package-level state.
func formatFullVersion(v, buildID string) string {
	if buildID != "" && buildID != "unknown" {
		return fmt.Sprintf("%s (%s)", v, buildID)
	}
	return v
}

// Fork patch: this init() and its helpers (resolveVersion,
// usableModuleVersion, readVCS, deriveDevVersion) diverge from upstream.
// Upstream unconditionally overwrote Version with info.Main.Version; the fork
// makes an ldflags-injected Version authoritative (release/npm builds MUST
// win — see the "Verify" step in .github/workflows/publish-fork-npm.yml) and
// only derives a value from build metadata for un-injected local builds.
//
// A user may install rush using `go install github.com/PHPCraftdream/rush@latest`
// without -ldflags, in which case the version above is unset. As a workaround
// we use the embedded build version that *is* set when using `go install` (and
// is only set for `go install` and not for `go build`). For plain `go build`
// from a checkout, that main version may still be "(devel)" or a pseudo-
// version depending on the toolchain, so we additionally derive a meaningful
// version from the VCS metadata the toolchain embeds (vcs.revision) — this
// lets two local dev builds be told apart. The derived string is always the
// bare "<hash>-<forkBaseVersion>" (e.g. "141ac19-0.2.0-alpha.0"): no upstream-tag-
// shaped prefix is ever recovered or prepended here, even when
// info.Main.Version happens to be a pseudo-version built on top of a real
// upstream tag. That tag is purely incidental — whichever charmbracelet/crush
// release the local module cache happened to resolve against — not a
// deliberate signal about how far this fork has diverged or triaged
// upstream, so showing it here would misrepresent it as one. Neither path
// ever includes a "devel" or dirty marker in the output. Release/packaged
// builds inject Version via ldflags and are left untouched.
func init() {
	info, _ := debug.ReadBuildInfo()
	Version, Commit = resolveVersion(Version, Commit, info)
	if BuildID == "" {
		BuildID = deriveBuildID()
	}
	if BuildTime == "" {
		BuildTime = deriveBuildTime()
	}
}

// deriveBuildTime reports when the running executable was last written, used
// as BuildTime for builds that don't inject it via ldflags. For a locally
// built binary that IS the build time; for a downloaded release it is when
// the file landed on disk, which is still a strictly better provenance signal
// than nothing. Returns "" (not "unknown") on failure so VersionLine simply
// omits the field rather than printing a placeholder.
func deriveBuildTime() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return ""
	}
	return fi.ModTime().Format("2006-01-02 15:04:05")
}

// resolveVersion decides the final Version and Commit from the ldflags-provided
// defaults together with the build metadata embedded by the Go toolchain. It is
// pure and unit-testable; init() is a thin wrapper around it.
//
// Precedence for Version:
//   - an ldflags-injected value (defaultVersion != "devel") always wins — this
//     is the release/packaged-build path and MUST NOT be clobbered here. The
//     npm publish workflow additionally verifies each built binary reports this
//     value (see .github/workflows/publish-fork-npm.yml "Verify" step);
//   - otherwise the module version resolved by `go install pkg@version`, when
//     it is a clean release version (not a pseudo-version and not "(devel)"),
//     wins directly;
//   - otherwise a VCS-derived "<commit>-<forkBaseVersion>" string for local dev
//     builds (no "devel" marker, no upstream-tag-shaped prefix — see
//     deriveDevVersion).
//
// Commit is filled from VCS only when the ldflags default is still "unknown".
func resolveVersion(defaultVersion, defaultCommit string, info *debug.BuildInfo) (version, commit string) {
	version, commit = defaultVersion, defaultCommit
	if info == nil {
		return version, commit
	}
	if version == "devel" && usableModuleVersion(info.Main.Version) {
		mv := info.Main.Version
		version = mv
	}
	vcs := readVCS(info)
	if commit == "unknown" && vcs.revision != "" {
		commit = vcs.revision
	}
	if version == "devel" {
		if dv := deriveDevVersion(vcs.revision); dv != "" {
			version = dv
		}
	}
	return version, commit
}

// pseudoVersionSuffixRe matches the Go-toolchain pseudo-version suffix built on
// top of a real prior tag, e.g. the "-0.20260628185628-e47711a0e3e4" part of
// "v0.72.1-0.20260628185628-e47711a0e3e4" (optionally followed by "+dirty").
// usableModuleVersion uses it to reject the raw pseudo-version from being
// shown directly.
var pseudoVersionSuffixRe = regexp.MustCompile(`-0\.\d{14}-[0-9a-f]{12}(\+dirty)?$`)

// usableModuleVersion reports whether a BuildInfo main-module version is
// meaningful enough to expose directly. Local checkout builds can report
// v0.0.0-<timestamp>-<commit>[+dirty], which is a Go pseudo-version, not a
// release version users can match to a package. Those fall through to the
// VCS-derived <commit>-<forkBaseVersion> format instead. A pseudo-version
// built on top of a real prior tag (e.g. "v0.72.1-0.<timestamp>-<commit>[+dirty]")
// is rejected here too — it is just as unhelpful as the v0.0.0 case, and its
// base tag is deliberately NOT recovered or shown (see deriveDevVersion).
func usableModuleVersion(v string) bool {
	if v == "" || v == "(devel)" {
		return false
	}
	if strings.HasPrefix(v, "v0.0.0-") || strings.HasPrefix(v, "0.0.0-") {
		return false
	}
	if strings.Contains(v, "+dirty") {
		return false
	}
	if pseudoVersionSuffixRe.MatchString(v) {
		return false
	}
	return true
}

// vcsInfo holds the subset of [debug.BuildInfo] settings that describe the
// source control state the binary was built from.
type vcsInfo struct {
	revision string // "vcs.revision": full commit hash
	modified string // "vcs.modified": "true", "false", or empty
}

// readVCS extracts VCS settings from a build info record. These entries are
// embedded automatically by the Go toolchain (Go 1.18+) when building from a
// VCS checkout.
func readVCS(info *debug.BuildInfo) vcsInfo {
	var v vcsInfo
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			v.revision = s.Value
		case "vcs.modified":
			v.modified = s.Value
		}
	}
	return v
}

// deriveDevVersion builds a human-meaningful version for a development build
// from embedded VCS metadata, embedding the fork's current release-line
// version (forkBaseVersion), e.g. "06c8078-0.2.0-alpha.0" for a clean checkout. It
// returns an empty string when no revision is available, signalling the
// caller to keep the plain "devel" default. No "devel" marker and no dirty
// marker are ever included in the returned string — the commit hash +
// forkBaseVersion are the only content.
//
// This deliberately never prepends an upstream-tag-shaped prefix, even when
// one could be recovered from a Go pseudo-version (e.g. "v0.72.1" from
// "v0.72.1-0.<timestamp>-<commit>"). That recovery previously existed
// (extractBaseTag, removed) on the assumption that a plain `go build .`
// always produces info.Main.Version == "(devel)" with no base tag at all —
// but that assumption does not hold for every Go toolchain: a local `go
// build .` can embed a real pseudo-version with a recoverable base tag,
// which produced a confusing "v0.72.1-<hash>-0.2.0-alpha.0" that looked like it
// carried a deliberate upstream-tracking signal but didn't. The fork no
// longer surfaces any upstream-version signal in `rush --version` output at
// all (see VersionLine's doc comment) — this function still avoids the
// incidental pseudo-version tag on its own merits: it is noise, not signal.
func deriveDevVersion(revision string) string {
	if revision == "" {
		return ""
	}
	short := revision
	if len(short) > 7 {
		short = short[:7]
	}
	return short + "-" + forkBaseVersion
}

// deriveBuildID uses the running executable's modification time as a unique
// build fingerprint. This changes on every recompilation (including `go run`),
// making it reliable for detecting stale servers during development.
func deriveBuildID() string {
	exe, err := os.Executable()
	if err != nil {
		return "unknown"
	}
	fi, err := os.Stat(exe)
	if err != nil {
		return "unknown"
	}
	return strconv.FormatInt(fi.ModTime().UnixNano(), 36)
}
