package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCheckForUpdate_Old(t *testing.T) {
	info, err := Check(t.Context(), "v0.10.0", testClient{"v0.11.0"})
	require.NoError(t, err)
	require.NotNil(t, info)
	require.True(t, info.Available())
}

func TestCheckForUpdate_Beta(t *testing.T) {
	t.Run("current is stable", func(t *testing.T) {
		info, err := Check(t.Context(), "v0.10.0", testClient{"v0.11.0-beta.1"})
		require.NoError(t, err)
		require.NotNil(t, info)
		require.False(t, info.Available())
	})

	t.Run("current is also beta", func(t *testing.T) {
		info, err := Check(t.Context(), "v0.11.0-beta.1", testClient{"v0.11.0-beta.2"})
		require.NoError(t, err)
		require.NotNil(t, info)
		require.True(t, info.Available())
	})

	t.Run("current is beta, latest isn't", func(t *testing.T) {
		info, err := Check(t.Context(), "v0.11.0-beta.1", testClient{"v0.11.0"})
		require.NoError(t, err)
		require.NotNil(t, info)
		require.True(t, info.Available())
	})
}

func TestInfo_AvailableSemver(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, current, latest string
		want                  bool
	}{
		{"numeric prerelease advances", "0.2.0-alpha.9", "0.2.0-alpha.19", true},
		{"numeric prerelease regresses", "0.2.0-alpha.19", "0.2.0-alpha.9", false},
		{"equal", "0.2.0-alpha.19", "0.2.0-alpha.19", false},
		{"stable downgrade", "0.3.0", "0.2.9", false},
		{"numeric core ordering", "0.9.0", "0.10.0", true},
		{"stable after prerelease", "0.2.0-alpha.19", "0.2.0", true},
		{"prerelease after stable", "0.2.0", "0.2.0-alpha.19", false},
		{"higher-core prerelease after stable", "0.2.0", "0.3.0-alpha.1", false},
		{"v prefix", "v0.2.0-alpha.9", "v0.2.0-alpha.19", true},
		{"build metadata ignored", "0.2.0+one", "0.2.0+two", false},
		{"malformed current", "not-a-version", "0.2.0", false},
		{"malformed latest", "0.1.0", "0.2", false},
		{"leading zero numeric prerelease invalid", "0.2.0-alpha.1", "0.2.0-alpha.01", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, (Info{Current: tt.current, Latest: tt.latest}).Available())
		})
	}
}

func TestGitHubLatestRejectsInvalidReleaseTag(t *testing.T) {
	t.Parallel()
	for _, body := range []string{`{}`, `{"tag_name":"not-semver"}`} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			release, err := (&github{url: server.URL}).Latest(t.Context())
			require.Error(t, err)
			require.Nil(t, release)
		})
	}
}

func TestInfo_IsDevelopment(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		current string
		want    bool
	}{
		{name: "plain devel", current: "devel", want: true},
		{name: "devel with build id", current: "devel (abc123)", want: true},
		{name: "devel with commit", current: "devel-90c57af", want: true},
		{name: "dirty build", current: "v0.0.0-20260705112643-90c57af7ca7a+dirty", want: true},
		{name: "go install pseudo version", current: "v0.0.0-0.20251231235959-06c807842604", want: true},
		{name: "checkout release-line version", current: "06c8078-0.2.0-alpha.19", want: true},
		{name: "checkout stable version", current: "06c8078-0.2.0", want: true},
		{name: "short checkout hash", current: "abc-0.2.0", want: true},
		{name: "checkout version with build id", current: "06c8078-0.2.0-alpha.19 (abc123)", want: true},
		{name: "invalid checkout version", current: "06c8078-not-a-version", want: false},
		{name: "long checkout hash", current: "06c80789-0.2.0", want: false},
		{name: "false checkout prefix", current: "06c8078x-0.2.0", want: false},
		{name: "release", current: "v0.1.0", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, Info{Current: tt.current}.IsDevelopment())
		})
	}
}

type testClient struct{ tag string }

// Latest implements Client.
func (t testClient) Latest(ctx context.Context) (*Release, error) {
	return &Release{
		TagName: t.tag,
		HTMLURL: "https://example.org",
	}, nil
}
