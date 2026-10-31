package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

type denyCreatePermissionService struct {
	mockPermissionService
	requests int
}

func (m *denyCreatePermissionService) Request(context.Context, permission.CreatePermissionRequest) (bool, error) {
	m.requests++
	return false, nil
}

func TestWriteDeniedDoesNotCreateParentDirectories(t *testing.T) {
	workingDir := t.TempDir()
	permissions := &denyCreatePermissionService{}
	tool := NewWriteTool(permissions, &mockHistoryService{}, mockFileTrackerService{}, workingDir)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "test-session")
	input, err := json.Marshal(WriteParams{FilePath: filepath.Join("missing", "nested", "file.txt"), Content: "content"})
	require.NoError(t, err)

	resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "write-denied", Name: WriteToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Equal(t, 1, permissions.requests)
	_, err = os.Stat(filepath.Join(workingDir, "missing"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestEditCreateDeniedDoesNotCreateParentDirectories(t *testing.T) {
	workingDir := t.TempDir()
	permissions := &denyCreatePermissionService{}
	edit := editContext{
		ctx:         context.WithValue(context.Background(), SessionIDContextKey, "test-session"),
		permissions: permissions,
		files:       &mockHistoryService{},
		filetracker: &mockEditFileTracker{},
		workingDir:  workingDir,
	}
	filePath := filepath.Join(workingDir, "missing", "nested", "file.txt")

	resp, err := createNewFile(edit, filePath, "content", fantasy.ToolCall{ID: "edit-denied"})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Equal(t, 1, permissions.requests)
	_, err = os.Stat(filepath.Join(workingDir, "missing"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
