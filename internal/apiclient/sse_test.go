package apiclient

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func isolateSSETestEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"RUSH_GLOBAL_DATA", "RUSH_GLOBAL_CONFIG", "RUSH_GLOBAL_DIR"} {
		t.Setenv(key, t.TempDir())
	}
}

func TestReadSSEFrame(t *testing.T) {
	isolateSSETestEnvironment(t)
	tests := []struct {
		name         string
		input        string
		wantID       string
		wantEvent    string
		wantData     string
		wantRetry    time.Duration
		wantHasData  bool
		wantHasID    bool
		wantHasRetry bool
	}{
		{name: "multiline", input: "id: 42\nevent: update\ndata: first\ndata: second\nretry: 1500\n\n", wantID: "42", wantEvent: "update", wantData: "first\nsecond", wantRetry: 1500 * time.Millisecond, wantHasData: true, wantHasID: true, wantHasRetry: true},
		{name: "crlf and comments", input: ": heartbeat\r\ndata: ok\r\n\r\n", wantData: "ok", wantHasData: true},
		{name: "id only", input: "id: 9\n\n", wantID: "9", wantHasID: true},
		{name: "unknown field", input: "unknown: ignored\ndata: yes\n\n", wantData: "yes", wantHasData: true},
		{name: "empty data", input: "data:\n\n", wantHasData: true},
		{name: "nul id ignored", input: "id: bad\x00id\ndata: x\n\n", wantData: "x", wantHasData: true},
		{name: "retry ASCII digits", input: "retry: 12\n\n", wantRetry: 12 * time.Millisecond, wantHasRetry: true},
		{name: "retry zero is present", input: "retry: 0\n\n", wantHasRetry: true},
		{name: "retry rejects signs and non-ASCII digits", input: "retry: +12\nretry: ١٢\ndata: x\n\n", wantData: "x", wantHasData: true},
		{name: "empty id is present", input: "id:\n\n", wantHasID: true},
		{name: "empty retry is absent", input: "retry:\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readSSEFrame(bufio.NewReader(strings.NewReader(tt.input)))
			if err != nil {
				t.Fatalf("readSSEFrame() error = %v", err)
			}
			if got.id != tt.wantID || got.event != tt.wantEvent || got.data != tt.wantData || got.retry != tt.wantRetry || got.hasData != tt.wantHasData || got.hasID != tt.wantHasID || got.hasRetry != tt.wantHasRetry {
				t.Fatalf("readSSEFrame() = %+v", got)
			}
		})
	}
}

func TestReadSSEFrameSplitReads(t *testing.T) {
	isolateSSETestEnvironment(t)
	const input = "id: 7\r\nevent: chunk\r\ndata: alpha\r\ndata: beta\r\n\r\n"
	want := sseFrame{id: "7", event: "chunk", data: "alpha\nbeta", hasData: true, hasID: true}
	for boundary := 1; boundary <= len(input); boundary++ {
		t.Run(fmt.Sprint(boundary), func(t *testing.T) {
			got, err := readSSEFrame(bufio.NewReader(&chunkReader{data: []byte(input), chunk: boundary}))
			if err != nil {
				t.Fatalf("readSSEFrame() error = %v", err)
			}
			if got != want {
				t.Fatalf("readSSEFrame() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestReadSSEFrameBoundsAndIncomplete(t *testing.T) {
	isolateSSETestEnvironment(t)
	tests := []struct {
		name  string
		input string
		want  error
	}{
		{name: "oversize line", input: "data: " + strings.Repeat("x", sseMaxLineBytes) + "\n\n", want: errSSELineTooLong},
		{name: "oversize frame", input: "data: " + strings.Repeat("x", 850000) + "\ndata: " + strings.Repeat("y", 850000) + "\ndata: " + strings.Repeat("z", 850000) + "\ndata: " + strings.Repeat("w", 850000) + "\ndata: " + strings.Repeat("v", 850000) + "\n\n", want: errSSEFrameTooLarge},
		{name: "CRLF line at exact limit", input: "data: " + strings.Repeat("x", sseMaxLineBytes-len("data: ")) + "\r\n\r\n"},
		{name: "CRLF line over limit", input: "data: " + strings.Repeat("x", sseMaxLineBytes-len("data:")+1) + "\r\n\n", want: errSSELineTooLong},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readSSEFrame(bufio.NewReader(strings.NewReader(tt.input)))
			if !errors.Is(err, tt.want) {
				t.Fatalf("readSSEFrame() error = %v, want %v", err, tt.want)
			}
		})
	}

	frame, err := readSSEFrame(bufio.NewReader(strings.NewReader("id: 4\ndata: partial")))
	if !errors.Is(err, io.EOF) || frame != (sseFrame{}) {
		t.Fatalf("incomplete frame = %+v, %v", frame, err)
	}
}

type chunkReader struct {
	data  []byte
	chunk int
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	n := len(p)
	if n > r.chunk {
		n = r.chunk
	}
	if n > len(r.data) {
		n = len(r.data)
	}
	copy(p[:n], r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

var _ io.Reader = (*chunkReader)(nil)
