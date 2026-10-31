package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/history"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/stretchr/testify/require"
)

// fsWriteTestScope builds a FolderScope over the given entries, resolving
// each entry dir through resolveScopedPath the same way the batch runner
// resolves item paths, so matcher and items compare resolved-to-resolved.
func fsWriteTestScope(t *testing.T, workingDir string, entries ...permission.FolderScopeEntry) permission.FolderScope {
	t.Helper()
	if len(entries) == 0 {
		entries = []permission.FolderScopeEntry{{
			Dir: ".",
			Ops: []permission.FileOp{permission.FileOpCreate, permission.FileOpOverwrite},
		}}
	}
	resolved := make([]permission.FolderScopeEntry, len(entries))
	for i, e := range entries {
		dir, err := resolveScopedPath(context.Background(), OSDisk(), workingDir, e.Dir)
		require.NoError(t, err)
		resolved[i] = permission.FolderScopeEntry{Dir: dir, Ops: e.Ops}
	}
	scope, err := permission.BuildFolderScope(permission.FolderScopeSpec{WorkingDir: workingDir, Entries: resolved})
	require.NoError(t, err)
	return scope
}

func fsWriteRun(t *testing.T, ctx context.Context, tool fantasy.AgentTool, params FSWriteParams) (fantasy.ToolResponse, error) {
	t.Helper()
	raw, err := json.Marshal(params)
	require.NoError(t, err)
	return tool.Run(ctx, fantasy.ToolCall{ID: "call", Name: FSWriteToolName, Input: string(raw)})
}

func TestFSWriteForbiddenResolvedTargetIsDenied(t *testing.T) {
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-forbidden")
	target := filepath.Join(dir, "blocked.txt")
	t.Setenv(ForbiddenWritesEnv, target)
	tool := NewFSWriteTool(fsWriteTestScope(t, dir), &mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir, nil)
	resp, err := fsWriteRun(t, ctx, tool, FSWriteParams{Items: []FSWriteItem{{Path: "blocked.txt", Content: "must not write"}}})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, ForbiddenWritesEnv)
	_, statErr := os.Stat(target)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestFSWriteForbiddenAliasTargetIsDenied(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require elevated privileges on Windows")
	}
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	aliasDir := filepath.Join(dir, "alias")
	require.NoError(t, os.Mkdir(realDir, 0o755))
	require.NoError(t, os.Symlink(realDir, aliasDir))
	blocked := filepath.Join(realDir, "blocked.txt")
	t.Setenv(ForbiddenWritesEnv, blocked)
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-forbidden")
	tool := NewFSWriteTool(fsWriteTestScope(t, dir), &mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir, nil)
	resp, err := fsWriteRun(t, ctx, tool, FSWriteParams{Items: []FSWriteItem{{Path: filepath.Join("alias", "blocked.txt"), Content: "must not write"}}})
	require.NoError(t, err)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, ForbiddenWritesEnv)
	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, 0, meta.Succeeded)
	require.Equal(t, 1, meta.Failed)
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, ForbiddenWritesEnv)
	_, statErr := os.Stat(blocked)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestFSWriteCreateVsOverwrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-write")
	scope := fsWriteTestScope(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "exist.txt"), []byte("old"), 0o644))

	tool := NewFSWriteTool(scope, &mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir, nil)
	resp, err := fsWriteRun(t, ctx, tool, FSWriteParams{Items: []FSWriteItem{
		{Path: "new.txt", Content: "hello"},
		{Path: "exist.txt", Content: "new"},
	}})
	require.NoError(t, err)
	require.False(t, resp.IsError)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, 2, meta.Succeeded)
	require.Equal(t, "create", meta.Items[0].Op)
	require.Equal(t, "overwrite", meta.Items[1].Op)
	require.Equal(t, FSStatusOK, meta.Items[0].Status)
	require.Equal(t, FSStatusOK, meta.Items[1].Status)

	content, err := os.ReadFile(filepath.Join(dir, "new.txt"))
	require.NoError(t, err)
	require.Equal(t, "hello", string(content))
	content, err = os.ReadFile(filepath.Join(dir, "exist.txt"))
	require.NoError(t, err)
	require.Equal(t, "new", string(content))

	require.Contains(t, resp.Content, "fs_write: 2 of 2 items ok")
	require.Contains(t, resp.Content, "new.txt (create")
	require.Contains(t, resp.Content, "exist.txt (overwrite")
}

func TestFSWriteCreateOnlyRefusesExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-write")
	scope := fsWriteTestScope(t, dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("keep"), 0o644))

	tool := NewFSWriteTool(scope, &mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir, nil)
	resp, err := fsWriteRun(t, ctx, tool, FSWriteParams{Items: []FSWriteItem{
		{Path: "keep.txt", Content: "clobber", CreateOnly: true},
		{Path: "fresh.txt", Content: "brand", CreateOnly: true},
	}})
	require.NoError(t, err)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, "already exists")
	require.Equal(t, FSStatusOK, meta.Items[1].Status)

	content, err := os.ReadFile(filepath.Join(dir, "keep.txt"))
	require.NoError(t, err)
	require.Equal(t, "keep", string(content))
	content, err = os.ReadFile(filepath.Join(dir, "fresh.txt"))
	require.NoError(t, err)
	require.Equal(t, "brand", string(content))
}

func TestFSWriteParentCarveOutBlocksDirCreation(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-write")
	scope := fsWriteTestScope(t, dir,
		permission.FolderScopeEntry{Dir: ".", Ops: []permission.FileOp{permission.FileOpCreate}},
		permission.FolderScopeEntry{Dir: "locked"},
	)

	tool := NewFSWriteTool(scope, &mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir, nil)
	resp, err := fsWriteRun(t, ctx, tool, FSWriteParams{Items: []FSWriteItem{
		{Path: filepath.Join("locked", "new.txt"), Content: "inside"},
	}})
	require.NoError(t, err)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, 1, meta.Failed)
	require.Equal(t, FSStatusDenied, meta.Items[0].Status)

	_, statErr := os.Stat(filepath.Join(dir, "locked"))
	require.True(t, os.IsNotExist(statErr))
}

func TestFSWriteExecuteRechecksParentBeforeMkdir(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-deep")
	scope := fsWriteTestScope(t, dir,
		permission.FolderScopeEntry{
			Dir: filepath.Join("locked", "deep"),
			Ops: []permission.FileOp{permission.FileOpCreate},
		},
	)

	groupPath, err := resolveScopedPath(context.Background(), OSDisk(), dir, filepath.Join("locked", "deep", "f.txt"))
	require.NoError(t, err)
	group := FSBatchGroup[FSWriteItem]{
		Path:  groupPath,
		Items: []FSGroupItem[FSWriteItem]{{Index: 0, RawPath: "locked/deep/f.txt", Item: FSWriteItem{Path: "locked/deep/f.txt", Content: "x"}}},
	}

	outcomes, err := fsWriteExecuteGroup(ctx, scope, &mockHistoryService{}, mockFileTrackerService{}, dir, nil, group)
	require.NoError(t, err)
	require.Len(t, outcomes, 1)
	require.Equal(t, FSStatusFailed, outcomes[0].Status)
	require.Contains(t, outcomes[0].Error, "outside every folder scope")

	_, statErr := os.Stat(filepath.Join(dir, "locked"))
	require.True(t, os.IsNotExist(statErr))
}

func TestFSWriteLastWriteWinsWithinGroup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-write")
	scope := fsWriteTestScope(t, dir)

	tool := NewFSWriteTool(scope, &mockPermissionService{}, &mockHistoryService{}, mockFileTrackerService{}, dir, nil)
	resp, err := fsWriteRun(t, ctx, tool, FSWriteParams{Items: []FSWriteItem{
		{Path: "multi.txt", Content: "first"},
		{Path: "multi.txt", Content: "second"},
	}})
	require.NoError(t, err)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, FSStatusOK, meta.Items[0].Status)
	require.Equal(t, FSStatusOK, meta.Items[1].Status)

	content, err := os.ReadFile(filepath.Join(dir, "multi.txt"))
	require.NoError(t, err)
	require.Equal(t, "second", string(content))
	require.GreaterOrEqual(t, meta.Items[1].Additions, 1)
}

// fsWriteFailingReadDisk wraps the real disk but fails ReadFile for one
// chosen path and counts WriteFile calls, so the R5-5 regression can prove
// fs_write treats a failed pre-write snapshot read (on a path Stat says
// exists) as a pre-write error instead of silently overwriting from a
// false empty baseline.
type fsWriteFailingReadDisk struct {
	DiskProvider
	failReadPath string
	writeCalls   int
}

func (f *fsWriteFailingReadDisk) ReadFile(ctx context.Context, name string) ([]byte, error) {
	if name == f.failReadPath {
		return nil, errors.New("simulated transient read failure")
	}
	return f.DiskProvider.ReadFile(ctx, name)
}

func (f *fsWriteFailingReadDisk) WriteFile(ctx context.Context, name string, data []byte, perm fs.FileMode) error {
	f.writeCalls++
	return f.DiskProvider.WriteFile(ctx, name, data, perm)
}

// countingHistoryService wraps mockHistoryService and counts every
// Create/CreateVersion call, so the regression can assert zero history
// entries were recorded.
type countingHistoryService struct {
	*mockHistoryService
	createCalls        int
	createVersionCalls int
}

func (h *countingHistoryService) Create(ctx context.Context, sessionID, path, content string) (history.File, error) {
	h.createCalls++
	return h.mockHistoryService.Create(ctx, sessionID, path, content)
}

func (h *countingHistoryService) CreateVersion(ctx context.Context, sessionID, path, content string) (history.File, error) {
	h.createVersionCalls++
	return h.mockHistoryService.CreateVersion(ctx, sessionID, path, content)
}

// TestFSWriteFailedSnapshotReadBlocksWrite is the R5-5 regression: Stat
// reports the file exists, ReadFile fails, and WriteFile would otherwise
// succeed. fs_write must perform ZERO writes and record ZERO history
// entries instead of silently treating the unreadable old content as
// empty and overwriting anyway.
func TestFSWriteFailedSnapshotReadBlocksWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-write")
	scope := fsWriteTestScope(t, dir)
	existing := filepath.Join(dir, "exist.txt")
	require.NoError(t, os.WriteFile(existing, []byte("old"), 0o644))

	resolvedExisting, err := resolveScopedPath(context.Background(), OSDisk(), dir, "exist.txt")
	require.NoError(t, err)

	disk := &fsWriteFailingReadDisk{DiskProvider: OSDisk(), failReadPath: resolvedExisting}
	hist := &countingHistoryService{mockHistoryService: &mockHistoryService{}}

	tool := NewFSWriteTool(scope, &mockPermissionService{}, hist, mockFileTrackerService{}, dir, disk)
	resp, err := fsWriteRun(t, ctx, tool, FSWriteParams{Items: []FSWriteItem{
		{Path: "exist.txt", Content: "new"},
	}})
	require.NoError(t, err)
	require.True(t, resp.IsError)

	var meta FSBatchResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(resp.Metadata), &meta))
	require.Equal(t, FSStatusFailed, meta.Items[0].Status)
	require.Contains(t, meta.Items[0].Error, "cannot read existing content")

	require.Equal(t, 0, disk.writeCalls, "fs_write must never call WriteFile when the pre-write snapshot read fails")
	require.Equal(t, 0, hist.createCalls, "fs_write must never record a history Create when the pre-write snapshot read fails")
	require.Equal(t, 0, hist.createVersionCalls, "fs_write must never record a history CreateVersion when the pre-write snapshot read fails")

	content, err := os.ReadFile(existing)
	require.NoError(t, err)
	require.Equal(t, "old", string(content), "the file on disk must remain unchanged")
}

type fsBatchPerPathPermissionService struct {
	*mockPermissionService
	requests []permission.CreatePermissionRequest
	denyPath string
}

func (m *fsBatchPerPathPermissionService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	m.requests = append(m.requests, req)
	return filepath.Clean(req.Path) != filepath.Clean(m.denyPath), nil
}

func TestFSWriteBatchRequestsPermissionForEveryPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	workingDir := filepath.Join(dir, "work")
	firstRoot := filepath.Join(dir, "approved")
	secondRoot := filepath.Join(dir, "denied")
	require.NoError(t, os.MkdirAll(workingDir, 0o755))
	require.NoError(t, os.MkdirAll(firstRoot, 0o755))
	require.NoError(t, os.MkdirAll(secondRoot, 0o755))
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-write")
	scope := fsWriteTestScope(t, workingDir,
		permission.FolderScopeEntry{Dir: firstRoot, Ops: []permission.FileOp{permission.FileOpCreate}},
		permission.FolderScopeEntry{Dir: secondRoot, Ops: []permission.FileOp{permission.FileOpCreate}},
	)
	perms := &fsBatchPerPathPermissionService{
		mockPermissionService: &mockPermissionService{},
		denyPath:              filepath.Join(secondRoot, "b.txt"),
	}
	tool := NewFSWriteTool(scope, perms, &mockHistoryService{}, mockFileTrackerService{}, workingDir, nil)
	resp, err := fsWriteRun(t, ctx, tool, FSWriteParams{Items: []FSWriteItem{
		{Path: filepath.Join(firstRoot, "a.txt"), Content: "approved"},
		{Path: filepath.Join(secondRoot, "b.txt"), Content: "denied"},
	}})
	require.NoError(t, err)
	require.True(t, resp.StopTurn)
	require.Len(t, perms.requests, 2)
	require.Equal(t, filepath.Join(firstRoot, "a.txt"), perms.requests[0].Path)
	require.Equal(t, filepath.Join(secondRoot, "b.txt"), perms.requests[1].Path)
	require.NoFileExists(t, filepath.Join(firstRoot, "a.txt"))
	require.NoFileExists(t, filepath.Join(secondRoot, "b.txt"))
}

// fsWriteDenyService refuses every per-target permission request so the
// whole-call gate in NewFSWriteTool takes its denial branch.
type fsWriteDenyService struct {
	*mockPermissionService
}

func (m *fsWriteDenyService) Request(ctx context.Context, req permission.CreatePermissionRequest) (bool, error) {
	return false, nil
}

func TestFSWritePermissionDeniedStopsTurn(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ctx := context.WithValue(context.Background(), SessionIDContextKey, "sess-write")
	scope := fsWriteTestScope(t, dir)

	tool := NewFSWriteTool(scope, &fsWriteDenyService{}, &mockHistoryService{}, mockFileTrackerService{}, dir, nil)
	resp, err := fsWriteRun(t, ctx, tool, FSWriteParams{Items: []FSWriteItem{
		{Path: "denied.txt", Content: "never"},
	}})
	require.NoError(t, err)
	require.True(t, resp.StopTurn)
	require.True(t, resp.IsError)
	require.Contains(t, resp.Content, "User denied permission")

	_, statErr := os.Stat(filepath.Join(dir, "denied.txt"))
	require.True(t, os.IsNotExist(statErr))
	require.NotContains(t, resp.Content, "items ok")
}
