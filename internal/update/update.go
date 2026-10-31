package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	// The FORK's releases, not upstream's. This pointed at
	// charmbracelet/crush, which is wrong for a fork that CLAUDE.md
	// describes as "NOT a passive mirror": the versions are not comparable
	// (this fork is 0.2.0-alpha.x while upstream is 0.89.x), and the update
	// notice links the operator to this repository's releases page — so
	// announcing an upstream tag would send them somewhere that tag does not
	// exist, to install something they must not install.
	//
	// While this fork publishes no releases the endpoint 404s, Check returns
	// an error, and the notice is simply never sent. That is the correct
	// behaviour, and it starts working on its own the day a release is cut.
	githubApiUrl = "https://api.github.com/repos/PHPCraftdream/rush/releases/latest"
	userAgent    = "rush/1.0"

	// maxReleaseBodyBytes caps how much of the GitHub releases API HTTP
	// response body we read into memory. It's a real network endpoint (not
	// a trusted local source), so an unbounded io.ReadAll/Decode is avoided
	// even though a normal release payload is small. Mirrors the limit used
	// for similar small JSON API responses elsewhere (e.g.
	// internal/oauth/hyper/device.go, internal/oauth/copilot/oauth.go).
	maxReleaseBodyBytes = 1 << 20 // 1MB
)

// Default is the default [Client].
var Default Client = &github{}

// Info contains information about an available update.
type Info struct {
	Current string
	Latest  string
	URL     string
}

var (
	goInstallRegexp       = regexp.MustCompile(`^v?\d+\.\d+\.\d+-\d+\.\d{14}-[0-9a-f]{12}$`)
	checkoutVersionRegexp = regexp.MustCompile(`^([0-9a-f]{1,7})-(.+?)(?: \(([^()\r\n]+)\))?$`)
	semverRegexp          = regexp.MustCompile(`^[vV]?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)
)

type semver struct {
	major, minor, patch string
	prerelease          []string
}

func parseSemver(version string) (semver, bool) {
	parts := semverRegexp.FindStringSubmatch(version)
	if parts == nil {
		return semver{}, false
	}
	parsed := semver{major: parts[1], minor: parts[2], patch: parts[3]}
	if parts[4] != "" {
		parsed.prerelease = strings.Split(parts[4], ".")
		for _, identifier := range parsed.prerelease {
			if len(identifier) > 1 && identifier[0] == '0' && allDigits(identifier) {
				return semver{}, false
			}
		}
	}
	return parsed, true
}

func allDigits(value string) bool {
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func compareNumeric(a, b string) int {
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func compareSemver(a, b semver) int {
	for _, pair := range [][2]string{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if c := compareNumeric(pair[0], pair[1]); c != 0 {
			return c
		}
	}
	if len(a.prerelease) == 0 {
		if len(b.prerelease) == 0 {
			return 0
		}
		return 1
	}
	if len(b.prerelease) == 0 {
		return -1
	}
	for n := 0; n < len(a.prerelease) && n < len(b.prerelease); n++ {
		x, y := a.prerelease[n], b.prerelease[n]
		xn, yn := allDigits(x), allDigits(y)
		if xn && yn {
			if c := compareNumeric(x, y); c != 0 {
				return c
			}
			continue
		}
		if xn != yn {
			if xn {
				return -1
			}
			return 1
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	if len(a.prerelease) < len(b.prerelease) {
		return -1
	}
	if len(a.prerelease) > len(b.prerelease) {
		return 1
	}
	return 0
}

func (i Info) IsDevelopment() bool {
	// Fork patch: recognise the fork's dev-version formats. internal/version
	// now emits "devel", "devel (buildID)", and "devel-<commit>[-dirty]" for
	// local `go build`/`make build`; the go-install pseudo-version and any
	// dirty marker are also treated as development. These prefixes must stay
	// in lockstep with deriveDevVersion/formatFullVersion in internal/version.
	checkoutVersion := checkoutVersionRegexp.FindStringSubmatch(i.Current)
	validCheckoutVersion := checkoutVersion != nil
	if validCheckoutVersion {
		_, validCheckoutVersion = parseSemver(checkoutVersion[2])
	}
	return i.Current == "devel" || i.Current == "unknown" ||
		strings.HasPrefix(i.Current, "devel ") ||
		strings.HasPrefix(i.Current, "devel-") ||
		strings.Contains(i.Current, "dirty") ||
		goInstallRegexp.MatchString(i.Current) || validCheckoutVersion
}

// Available returns true only when Latest is a valid semver strictly newer
// than Current. Stable current versions never receive prerelease updates;
// build metadata is ignored for precedence.
func (i Info) Available() bool {
	current, ok := parseSemver(i.Current)
	if !ok {
		return false
	}
	latest, ok := parseSemver(i.Latest)
	if !ok {
		return false
	}
	if len(current.prerelease) == 0 && len(latest.prerelease) > 0 {
		return false
	}
	return compareSemver(latest, current) > 0
}

// Check checks if a new version is available.
func Check(ctx context.Context, current string, client Client) (Info, error) {
	info := Info{
		Current: current,
		Latest:  current,
	}

	release, err := client.Latest(ctx)
	if err != nil {
		return info, fmt.Errorf("failed to fetch latest release: %w", err)
	}

	info.Latest = strings.TrimPrefix(release.TagName, "v")
	info.Current = strings.TrimPrefix(info.Current, "v")
	info.URL = release.HTMLURL
	return info, nil
}

// Release represents a GitHub release.
type Release struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// Client is a client that can get the latest release.
type Client interface {
	Latest(ctx context.Context) (*Release, error)
}

type github struct {
	url string
}

// Latest implements [Client].
func (c *github) Latest(ctx context.Context) (*Release, error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	url := c.url
	if url == "" {
		url = githubApiUrl
	}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxReleaseBodyBytes))
		return nil, fmt.Errorf("GitHub API returned status %d: %s", resp.StatusCode, string(body))
	}

	var release Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseBodyBytes)).Decode(&release); err != nil {
		return nil, err
	}
	if strings.TrimSpace(release.TagName) == "" {
		return nil, fmt.Errorf("GitHub API response has an empty release tag")
	}
	if _, ok := parseSemver(release.TagName); !ok {
		return nil, fmt.Errorf("GitHub API response has an invalid release tag %q", release.TagName)
	}

	return &release, nil
}
