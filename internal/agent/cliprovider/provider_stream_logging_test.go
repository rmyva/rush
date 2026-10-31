package cliprovider

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// REVERT CHECK: remove the logRawPromptEnabled guard in StartStream and the
// raw event content will be logged without opt-in; these fields must stay safe.
func TestCLIEventLogFieldsRequireRawOptIn(t *testing.T) {
	raw := []byte(`{"type":"assistant","prompt":"secret prompt text"}`)
	fields := cliEventLogFields(raw, false)
	safe := fmt.Sprint(fields)
	if strings.Contains(safe, "secret prompt text") || !strings.Contains(safe, "assistant") || !strings.Contains(safe, fmt.Sprint(len(raw))) {
		t.Fatalf("safe event log fields = %v", fields)
	}
	injected := []byte(`{"type":"assistant: secret prompt text","prompt":"secret prompt text"}`)
	if got := fmt.Sprint(cliEventLogFields(injected, false)); strings.Contains(got, "secret prompt text") {
		t.Fatalf("untrusted event type leaked into safe log fields: %v", got)
	}
	if got := fmt.Sprint(cliEventLogFields(raw, true)); !strings.Contains(got, "secret prompt text") {
		t.Fatalf("explicit raw opt-in fields = %v", got)
	}
}

func TestStartStreamEventLoggingRequiresRawOptIn(t *testing.T) {
	const secret = "event secret prompt text"
	raw := `{"type":"assistant","prompt":"` + secret + `"}`
	for _, tc := range []struct {
		name       string
		rawOptIn   string
		wantSecret bool
	}{
		{name: "default safe logging", rawOptIn: "", wantSecret: false},
		{name: "explicit raw logging", rawOptIn: "1", wantSecret: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RUSH_CLIPROVIDER_LOG_RAW_PROMPT", tc.rawOptIn)
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(previous) })

			m := &cliModel{
				spec: CLISpec{Binary: binary, NoPTY: true, BuildArgs: func(bool) []string { return nil }, NewPartParser: func() func([]byte) (fantasy.StreamPart, bool) {
					return func([]byte) (fantasy.StreamPart, bool) { return fantasy.StreamPart{}, false }
				}},
				streamProcess: func(bool, []string) procHandle {
					return procHandle{stdout: bytes.NewBufferString(raw + "\n"), kill: func() {}, wait: func() (string, error) { return "", nil }}
				},
			}
			stream, err := m.Stream(context.Background(), fantasy.Call{})
			if err != nil {
				t.Fatalf("Stream(): %v", err)
			}
			for range stream {
			}
			logged := logs.String()
			if got := strings.Contains(logged, secret); got != tc.wantSecret {
				t.Fatalf("event secret logged = %v, want %v; logs: %s", got, tc.wantSecret, logged)
			}
			if !tc.wantSecret && (!strings.Contains(logged, "assistant") || !strings.Contains(logged, "bytes")) {
				t.Fatalf("safe event metadata absent from logs: %s", logged)
			}
		})
	}
}
