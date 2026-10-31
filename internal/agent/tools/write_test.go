package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

type mockFileTrackerService struct{}

func (m mockFileTrackerService) RecordRead(ctx context.Context, sessionID, path string) {}

func (m mockFileTrackerService) LastReadTime(ctx context.Context, sessionID, path string) time.Time {
	return time.Now()
}

func (m mockFileTrackerService) ListReadFiles(ctx context.Context, sessionID string) ([]string, error) {
	return nil, nil
}

func TestWriteToolForbiddenTargetThroughSymlinkedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated privileges on Windows")
	}

	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	aliasDir := filepath.Join(root, "alias")
	require.NoError(t, os.Mkdir(realDir, 0o755))
	require.NoError(t, os.Symlink(realDir, aliasDir))
	blocked := filepath.Join(realDir, "blocked.txt")
	t.Setenv(ForbiddenWritesEnv, blocked)

	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	tool := NewWriteTool(&mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, root)
	input, err := json.Marshal(WriteParams{FilePath: filepath.Join("alias", "blocked.txt"), Content: "must not write"})
	require.NoError(t, err)
	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "test-call", Name: WriteToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, ForbiddenWritesEnv)
	require.NoFileExists(t, blocked)
}

func TestWriteToolWritesEmptyNewFile(t *testing.T) {
	t.Parallel()

	workingDir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")

	tool := NewWriteTool(&mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, workingDir)

	input, err := json.Marshal(WriteParams{FilePath: "empty.txt", Content: ""})
	require.NoError(t, err)

	resp, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "test-call",
		Name:  WriteToolName,
		Input: string(input),
	})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	b, err := os.ReadFile(filepath.Join(workingDir, "empty.txt"))
	require.NoError(t, err)
	require.Equal(t, "", string(b))
}
