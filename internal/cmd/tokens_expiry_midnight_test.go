package cmd

import (
	"testing"
	"time"
)

func TestParseTokenExpiryMidnightBoundary(t *testing.T) {
	tokenTestEnv(t, t.TempDir())
	now := mustParseTime(t, "2026-11-01T00:00:00Z")
	for _, tc := range []struct {
		name    string
		expires string
		wantErr bool
	}{
		{name: "one second before midnight", expires: "2026-10-31T23:59:59Z", wantErr: true},
		{name: "exactly midnight", expires: "2026-11-01T00:00:00Z", wantErr: true},
		{name: "one second after midnight", expires: "2026-11-01T00:00:01Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTokenExpiry(tc.expires, now)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseTokenExpiry(%q) error = %v, wantErr %v", tc.expires, err, tc.wantErr)
			}
			if !tc.wantErr && (got == nil || !got.Equal(now.Add(time.Second))) {
				t.Fatalf("parseTokenExpiry(%q) = %v, want %v", tc.expires, got, now.Add(time.Second))
			}
		})
	}
}
