package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

func fsDeleteRun(t *testing.T, ctx context.Context, tool fantasy.AgentTool, params FSDeleteParams) (fantasy.ToolResponse, error) {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	return tool.Run(ctx, fantasy.ToolCall{ID: "call", Name: FSDeleteToolName, Input: string(raw)})
}

func TestFSDeleteRemovesFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-delete")
	scope := fsWriteTestScope(t, dir, permission.FolderScopeEntry{
		Dir: ".",
		Ops: []permission.FileOp{permission.FileOpDelete},
	})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gone.txt"), []byte("gone"), 0o644))

	tool := NewFSDeleteTool(scope, &mockPermissionService{}, dir, nil)
	resp, err := fsDeleteRun(t, ctx, tool, FSDeleteParams{Items: []FSDeleteItem{
		{Path: "gone.txt"},
	}})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, 1, meta.Succeeded)
	require.Equal(t, FSStatusOK, meta.Items[0].Status)

	_, statErr := os.Stat(filepath.Join(dir, "gone.txt"))
	require.True(t, os.IsNotExist(statErr))

	require.Contains(t, resp.Content, "fs_delete: 1 of 1 items ok")
}

func TestFSDeleteRefusesDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-delete")
	scope := fsWriteTestScope(t, dir, permission.FolderScopeEntry{
		Dir: ".",
		Ops: []permission.FileOp{permission.FileOpDelete},
	})
	require.NoError(t, os.Mkdir(filepath.Join(dir, "subdir"), 0o755))

	tool := NewFSDeleteTool(scope, &mockPermissionService{}, dir, nil)
	resp, err := fsDeleteRun(t, ctx, tool, FSDeleteParams{Items: []FSDeleteItem{
		{Path: "subdir"},
	}})
	require.NoError(t, err)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, "directory")

	_, statErr := os.Stat(filepath.Join(dir, "subdir"))
	require.NoError(t, statErr)
}

func TestFSDeleteRemovesSymlinkWithoutFollowingOutsideTarget(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("real symlink behavior is covered on non-Windows platforms")
	}
	// Symlink tests do not skip on GOOS; they skip only if the OS
	// refuses to create the symlink (see fs_scope_test.go).
	tmp := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-delete")
	secret := filepath.Join(tmp, "secret")
	require.NoError(t, os.MkdirAll(secret, 0o755))
	target := filepath.Join(secret, "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o644))
	root := filepath.Join(tmp, "root")
	require.NoError(t, os.MkdirAll(root, 0o755))
	if err := os.Symlink(target, filepath.Join(root, "link.txt")); err != nil {
		t.Skipf("skipping: symlink creation requires elevated privileges on this platform: %v", err)
	}

	scope := fsWriteTestScope(t, tmp, permission.FolderScopeEntry{
		Dir: "root",
		Ops: []permission.FileOp{permission.FileOpDelete},
	})

	tool := NewFSDeleteTool(scope, &mockPermissionService{}, tmp, nil)
	resp, err := fsDeleteRun(t, ctx, tool, FSDeleteParams{Items: []FSDeleteItem{
		{Path: filepath.Join("root", "link.txt")},
	}})
	require.NoError(t, err)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, FSStatusOK, meta.Items[0].Status)

	_, linkErr := os.Lstat(filepath.Join(root, "link.txt"))
	require.True(t, os.IsNotExist(linkErr), "named symlink should be removed")
	_, statErr := os.Stat(target)
	require.NoError(t, statErr, "outside symlink target must remain")
}

func TestFSDeleteContainmentUsesSymlinkOwnPath(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("real symlink behavior is covered on non-Windows platforms")
	}
	tmp := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-delete")
	inside := filepath.Join(tmp, "inside")
	outside := filepath.Join(tmp, "outside")
	require.NoError(t, os.MkdirAll(inside, 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	target := filepath.Join(inside, "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("inside"), 0o644))
	link := filepath.Join(outside, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	scope := fsWriteTestScope(t, tmp, permission.FolderScopeEntry{
		Dir: "inside",
		Ops: []permission.FileOp{permission.FileOpDelete},
	})
	tool := NewFSDeleteTool(scope, &mockPermissionService{}, tmp, nil)
	resp, err := fsDeleteRun(t, ctx, tool, FSDeleteParams{Items: []FSDeleteItem{{Path: link}}})
	require.NoError(t, err)
	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, FSStatusDenied, meta.Items[0].Status)
	require.FileExists(t, link)
	require.FileExists(t, target)
}

func TestFSDeleteParentSymlinkContainmentStillResolves(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("real symlink behavior is covered on non-Windows platforms")
	}
	tmp := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-delete")
	inside := filepath.Join(tmp, "inside")
	outside := filepath.Join(tmp, "outside")
	require.NoError(t, os.MkdirAll(inside, 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	target := filepath.Join(outside, "target.txt")
	require.NoError(t, os.WriteFile(target, []byte("outside"), 0o644))
	if err := os.Symlink(outside, filepath.Join(inside, "parent")); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	scope := fsWriteTestScope(t, tmp, permission.FolderScopeEntry{
		Dir: "inside",
		Ops: []permission.FileOp{permission.FileOpDelete},
	})
	tool := NewFSDeleteTool(scope, &mockPermissionService{}, tmp, nil)
	resp, err := fsDeleteRun(t, ctx, tool, FSDeleteParams{Items: []FSDeleteItem{{Path: filepath.Join("inside", "parent", "target.txt")}}})
	require.NoError(t, err)
	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, FSStatusDenied, meta.Items[0].Status)
	require.FileExists(t, target)
}

func TestFSDeleteMissingFileFailsPerItem(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-delete")
	scope := fsWriteTestScope(t, dir, permission.FolderScopeEntry{
		Dir: ".",
		Ops: []permission.FileOp{permission.FileOpDelete},
	})

	tool := NewFSDeleteTool(scope, &mockPermissionService{}, dir, nil)
	resp, err := fsDeleteRun(t, ctx, tool, FSDeleteParams{Items: []FSDeleteItem{
		{Path: "nope.txt"},
		{Path: "nope.txt"},
	}})
	require.NoError(t, err)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, 2, meta.Failed)
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Equal(t, FSStatusFailed, meta.Items[1].Status)
	require.Contains(t, meta.Items[0].Error, "file not found")
	require.Contains(t, meta.Items[1].Error, "file not found")

	require.True(t, resp.IsError)
	require.False(t, resp.StopTurn)
}

// fsDeleteDenyService refuses every permission request so the whole-call
// gate in NewFSDeleteTool takes its denial branch.
type fsDeleteDenyService struct {
	*mockPermissionService
}

func (m *fsDeleteDenyService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	return false, nil
}

func TestFSDeletePermissionDeniedStopsTurn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-delete")
	scope := fsWriteTestScope(t, dir, permission.FolderScopeEntry{
		Dir: ".",
		Ops: []permission.FileOp{permission.FileOpDelete},
	})
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0o644))

	tool := NewFSDeleteTool(scope, &fsDeleteDenyService{}, dir, nil)
	resp, err := fsDeleteRun(t, ctx, tool, FSDeleteParams{Items: []FSDeleteItem{
		{Path: "keep.txt"},
	}})
	require.NoError(t, err)
	require.True(t, resp.StopTurn)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "User denied permission")

	_, statErr := os.Stat(filepath.Join(dir, "keep.txt"))
	require.NoError(t, statErr)
	require.NotContains(t, resp.Content, "items ok")
}
