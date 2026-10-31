package tools

// fs_delete is the batch-capable delete member of the scoped fs_* family:
// N files, one permission request per target, per-item outcomes via RunFSBatch.
// Deleting is not a content write, so unlike fs_write/fs_replace/
// fs_write_lines it keeps no history and reads no filetracker state:
// each item is a stat-then-remove on the resolved path.

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/filepathext"
	"github.com/PHPCraftdream/rush/internal/permission"
)

//go:embed fs_delete.md
var fsDeleteDescription string

type FSDeleteItem struct {
	Path string `json:"path" description:"File path, absolute or relative to the working directory"`
}

type FSDeleteParams struct {
	Items []FSDeleteItem `json:"items" description:"Files to delete. Files only: directories and anything that is not a regular file are refused per item; there is no recursion"`
}

type FSDeletePermissionsParams struct {
	Paths []string `json:"paths"`
}

const FSDeleteToolName = "fs_delete"

func NewFSDeleteTool(scope permission.FolderScope, permissions permission.Service, workingDir string, disk DiskProvider) fantasy.AgentTool {
	disk = diskOrOS(disk)
	return fantasy.NewAgentTool(
		FSDeleteToolName,
		fsDeleteDescription,
		func(ctx context.Context, params FSDeleteParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if len(params.Items) == 0 {
				return fantasy.NewTextErrorResponse("at least one item is required"), nil
			}
			if len(params.Items) > FSBatchMaxItems {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"too many items: %d (maximum %d per call)", len(params.Items), FSBatchMaxItems)), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session_id is required")
			}

			joinedPaths := make([]string, len(params.Items))
			for i, item := range params.Items {
				if item.Path == "" {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("items[%d]: path is required", i)), nil
				}
				joinedPaths[i] = filepathext.SmartJoin(workingDir, item.Path)
			}

			// Every target path is authorized before any batch work, so a denial
			// ends the turn before the runner touches the filesystem.
			p, err := requestFSBatchPermissions(ctx, permissions, sessionID, call.ID, FSDeleteToolName, "delete", fmt.Sprintf("delete %d files", len(params.Items)), joinedPaths, workingDir, FSDeletePermissionsParams{Paths: joinedPaths})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			resp, err := RunFSBatch(ctx, FSBatch[FSDeleteItem]{
				Tool:       FSDeleteToolName,
				WorkingDir: workingDir,
				Scope:      scope,
				Items:      params.Items,
				Disk:       disk,
				PathOf: func(i FSDeleteItem) string {
					return i.Path
				},
				Preflight:   fsDeletePreflight,
				ResolvePath: fsDeleteResolvePath,
				Execute: func(ctx context.Context, group FSBatchGroup[FSDeleteItem]) ([]FSItemOutcome, error) {
					return fsDeleteExecuteGroup(ctx, disk, group)
				},
			})
			return resp, err
		},
	)
}

// fsDeleteResolvePath resolves parent components (including parent symlinks)
// while retaining the final entry's own name. Deleting a symlink acts on the
// link itself, so its target must not affect containment.
func fsDeleteResolvePath(ctx context.Context, disk DiskProvider, workingDir, raw string) (string, error) {
	joined := filepath.Clean(filepathext.SmartJoin(workingDir, raw))
	parent, err := resolveScopedPath(ctx, disk, workingDir, filepath.Dir(joined))
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(joined)), nil
}

// fsDeletePreflight grants delete unconditionally: existence and type are
// verified during execution, where the lstat check happens anyway.
func fsDeletePreflight(_ context.Context, item FSDeleteItem, _ int, _ string) (permission.FileOp, error) {
	return permission.FileOpDelete, nil
}

// fsDeleteExecuteGroup removes each item of one same-path group in
// order, independently: the first removes the file, later duplicates
// honestly report "file not found". There is no history write, no
// filetracker read and no diff — deletion is not a content write.
func fsDeleteExecuteGroup(ctx context.Context, disk DiskProvider, group FSBatchGroup[FSDeleteItem]) ([]FSItemOutcome, error) {
	disk = diskOrOS(disk)
	outcomes := make([]FSItemOutcome, len(group.Items))

	for i := range group.Items {
		info, err := disk.Stat(ctx, group.Path)
		if lstat, ok := disk.(interface {
			Lstat(context.Context, string) (fs.FileInfo, error)
		}); ok {
			info, err = lstat.Lstat(ctx, group.Path)
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				outcomes[i] = FSItemOutcome{
					Status: FSStatusFailed,
					Error:  "file not found: " + group.Path,
				}
			} else {
				outcomes[i] = FSItemOutcome{
					Status: FSStatusFailed,
					Error:  fmt.Sprintf("cannot access %s: %v", group.Path, err),
				}
			}
			continue
		}
		if info.IsDir() {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  fmt.Sprintf("%s is a directory — fs_delete removes regular files only and does not recurse", group.Path),
			}
			continue
		}
		if !info.Mode().IsRegular() && info.Mode()&fs.ModeSymlink == 0 {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  fmt.Sprintf("%s is not a regular file", group.Path),
			}
			continue
		}
		if err := disk.Remove(ctx, group.Path); err != nil {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  fmt.Sprintf("cannot delete %s: %v", group.Path, err),
			}
			continue
		}
		outcomes[i] = FSItemOutcome{Status: FSStatusOK}
	}

	return outcomes, nil
}
