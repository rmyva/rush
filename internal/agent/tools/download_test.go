package tools

// BUG-1 regression test (found by a full-project @rush --role reviewer
// audit, 2026-08-11): download.go used os.Create + io.Copy directly on the
// final destination path. A network failure or ctx-cancel partway through
// the copy left a truncated file at that path — a later view/edit tool call
// would read it as valid, complete content.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

// TestDownloadTool_NoTruncatedFileOnMidCopyFailure proves that a download
// aborted partway through the response body does not leave a partial file
// at the destination path — only a complete, successful download should
// ever produce a file there.
//
// REVERT CHECK PROCEDURE:
//  1. In download.go, replace the temp-file-then-rename block with the
//     original `os.Create(filePath)` + `io.Copy(outFile, resp.Body)`
//     (writing directly to filePath).
//  2. Run: go test ./internal/agent/tools -run TestDownloadTool_NoTruncatedFileOnMidCopyFailure -v
//  3. FAIL: a truncated file exists at the destination path after the
//     aborted download.
//  4. Restore the atomic-write fix and PASS.
func TestDownloadTool_NoTruncatedFileOnMidCopyFailure(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial-content-before-the-connection-dies"))
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		// http.ErrAbortHandler is net/http's documented, idiomatic way to
		// abruptly close the connection mid-response without logging a
		// stack trace — simulates a real network failure partway through
		// the body.
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)

	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	// These tests exercise the atomic-write behaviour, not the SSRF
	// guard, so they use a client that explicitly allows loopback
	// (httptest.Server listens on 127.0.0.1). The SSRF behaviour itself
	// is covered in ssrf_guard_test.go.
	tool := NewDownloadTool(&mockPermissionService{}, workingDir, NewSSRFGuardedClient(5*time.Second, true))

	input, err := json.Marshal(DownloadParams{URL: srv.URL, FilePath: "downloaded.bin"})
	require.NoError(t, err)

	// The download itself is expected to fail (connection aborted
	// mid-copy) — what this test actually verifies is what's on disk
	// afterward, not the tool's own error handling.
	_, _ = tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  DownloadToolName,
		Input: string(input),
	})

	destPath := filepath.Join(workingDir, "downloaded.bin")
	_, statErr := os.Stat(destPath)
	require.True(t, os.IsNotExist(statErr), "a failed/aborted download must not leave a truncated file at the destination path, got: %v", statErr)

	// No stray temp file left behind either.
	entries, err := os.ReadDir(workingDir)
	require.NoError(t, err)
	require.Empty(t, entries, "no temp or partial files should remain in the working directory after a failed download")
}

// TestDownloadTool_SuccessfulDownloadWritesCompleteFile is the companion
// happy-path check: a normal, uninterrupted download still produces the
// expected file with the expected content.
func TestDownloadTool_SuccessfulDownloadWritesCompleteFile(t *testing.T) {
	t.Parallel()

	const content = "hello from the download tool"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, content)
	}))
	t.Cleanup(srv.Close)

	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	tool := NewDownloadTool(&mockPermissionService{}, workingDir, NewSSRFGuardedClient(5*time.Second, true))

	input, err := json.Marshal(DownloadParams{URL: srv.URL, FilePath: "ok.txt"})
	require.NoError(t, err)

	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  DownloadToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	got, err := os.ReadFile(filepath.Join(workingDir, "ok.txt"))
	require.NoError(t, err)
	require.Equal(t, content, string(got))
}

func TestDownloadTool_ForbiddenSymlinkTargetNotReplaced(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated privileges on Windows")
	}
	const original = "preserve this content"
	workingDir := t.TempDir()
	realDir := filepath.Join(workingDir, "real")
	aliasDir := filepath.Join(workingDir, "alias")
	require.NoError(t, os.Mkdir(realDir, 0o755))
	target := filepath.Join(realDir, "blocked.txt")
	require.NoError(t, os.WriteFile(target, []byte(original), 0o644))
	require.NoError(t, os.Symlink(realDir, aliasDir))
	t.Setenv(ForbiddenWritesEnv, target)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("replacement"))
	}))
	t.Cleanup(server.Close)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	tool := NewDownloadTool(&mockPermissionService{}, workingDir, NewSSRFGuardedClient(5*time.Second, true))
	input, err := json.Marshal(DownloadParams{URL: server.URL, FilePath: filepath.Join("alias", "blocked.txt")})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call", Name: DownloadToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, ForbiddenWritesEnv)
	got, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, original, string(got))
}

// TestDownloadTool_WritesFileWithReadablePermissions is the regression test
// for a gap an independent @oh review found in the temp-file+rename fix
// above: os.CreateTemp creates its file 0600 (owner-only), and unlike
// fsext.AtomicWriteFile's own convention, the rename path here never
// chmod'd it back up before the rename — silently changing a downloaded
// file's permissions from world-readable (os.Create's old 0666&^umask) to
// owner-only. Skipped on Windows, where os.FileMode bits don't carry the
// same POSIX meaning.
func TestDownloadTool_WritesFileWithReadablePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file permission bits don't apply on Windows")
	}
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "content")
	}))
	t.Cleanup(srv.Close)

	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	tool := NewDownloadTool(&mockPermissionService{}, workingDir, NewSSRFGuardedClient(5*time.Second, true))

	input, err := json.Marshal(DownloadParams{URL: srv.URL, FilePath: "readable.txt"})
	require.NoError(t, err)

	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  DownloadToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	info, err := os.Stat(filepath.Join(workingDir, "readable.txt"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o644), info.Mode().Perm(),
		"downloaded file must be world-readable (0644), not the 0600 os.CreateTemp defaults to")
}
