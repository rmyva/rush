package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// These regression cases use a small bound to prove every whole-file mutation
// rejects an oversized existing target before reading it into a snapshot.
func TestFullFileMutationSizeGuard(t *testing.T) {
	limit := editMaxFileSize
	editMaxFileSize = 4
	t.Cleanup(func() { editMaxFileSize = limit })

	t.Run("write", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "large.txt")
		require.NoError(t, os.WriteFile(path, []byte("12345"), 0o644))
		ctx := context.WithValue(context.Background(), SessionIDContextKey, "size-guard")
		tool := NewWriteTool(&mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir)
		input, err := json.Marshal(WriteParams{FilePath: "large.txt", Content: "changed"})
		require.NoError(t, err)
		resp, err := tool.Run(ctx, fantasy.ToolCall{ID: "call", Name: WriteToolName, Input: string(input)})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.Contains(t, resp.Content, "too large to edit")
		require.Contains(t, resp.Content, "5 bytes")
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "12345", string(got))
	})

	t.Run("fs_write", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "large.txt")
		require.NoError(t, os.WriteFile(path, []byte("12345"), 0o644))
		ctx := context.WithValue(context.Background(), SessionIDContextKey, "size-guard")
		tool := NewFSWriteTool(fsWriteTestScope(t, dir), &mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir, nil)
		resp, err := fsWriteRun(t, ctx, tool, FSWriteParams{Items: []FSWriteItem{{Path: "large.txt", Content: "changed"}}})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.Contains(t, resp.Metadata, "too large to edit")
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "12345", string(got))
	})

	t.Run("fs_replace", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "large.txt")
		require.NoError(t, os.WriteFile(path, []byte("12345"), 0o644))
		ctx := context.WithValue(context.Background(), SessionIDContextKey, "size-guard")
		scope := fsWriteTestScope(t, dir, permission.FolderScopeEntry{Dir: ".", Ops: []permission.FileOp{permission.FileOpReplace, permission.FileOpRead}})
		tool := NewFSReplaceTool(scope, &mockPermissionService{}, &mockHistoryService{}, &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}, dir, nil)
		resp, err := fsReplaceRun(t, ctx, tool, FSReplaceParams{Items: []FSReplaceItem{{Path: "large.txt", OldString: "1", NewString: "x"}}})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.Contains(t, resp.Metadata, "too large to edit")
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "12345", string(got))
	})

	t.Run("fs_write_lines", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "large.txt")
		require.NoError(t, os.WriteFile(path, []byte("12345"), 0o644))
		ctx := context.WithValue(context.Background(), SessionIDContextKey, "size-guard")
		scope := fsWriteTestScope(t, dir, permission.FolderScopeEntry{Dir: ".", Ops: []permission.FileOp{permission.FileOpWriteLines, permission.FileOpRead}})
		tool := NewFSWriteLinesTool(scope, &mockPermissionService{}, &mockHistoryService{}, &mockEditFileTracker{lastRead: time.Now().Add(time.Second)}, dir, nil)
		resp, err := fsWriteLinesRun(t, ctx, tool, FSWriteLinesParams{Items: []FSWriteLinesItem{{Path: "large.txt", StartLine: 1, EndLine: 1, Content: "changed"}}})
		require.NoError(t, err)
		require.True(t, resp.IsError)
		require.Contains(t, resp.Metadata, "too large to edit")
		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "12345", string(got))
	})
}
