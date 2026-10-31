package tools

// fs_replace is the batch-capable member of the scoped fs_* write family:
// N replace operations across multiple files, one permission request per target,
// per-item outcomes via RunFSBatch, and — for items sharing one file —
// in-order in-memory application with a single atomic write. It reuses
// edit.go's findAndReplace and commitFileChange so matching semantics,
// durability, history and read-tracking behave exactly like the
// single-file edit path.

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/diff"
	"github.com/PHPCraftdream/rush/internal/filepathext"
	"github.com/PHPCraftdream/rush/internal/filetracker"
	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/PHPCraftdream/rush/internal/history"
	"github.com/PHPCraftdream/rush/internal/permission"
)

//go:embed fs_replace.md
var fsReplaceDescription string

type FSReplaceItem struct {
	Path       string `json:"path" description:"File path, absolute or relative to the working directory"`
	OldString  string `json:"old_string" description:"The exact text to replace, including whitespace and line breaks"`
	NewString  string `json:"new_string" description:"The replacement text"`
	ReplaceAll bool   `json:"replace_all,omitempty" description:"Replace every occurrence of old_string (default: exactly one match required)"`
}

type FSReplaceParams struct {
	Items []FSReplaceItem `json:"items" description:"Replace operations. Items sharing one file are applied in order against the same in-memory content and the file is written once"`
}

type FSReplacePermissionsParams struct {
	Paths []string `json:"paths"`
}

const FSReplaceToolName = "fs_replace"

func NewFSReplaceTool(scope permission.FolderScope, permissions permission.Service, files history.Service, filetracker filetracker.Service, workingDir string, disk DiskProvider) fantasy.AgentTool {
	disk = diskOrOS(disk)
	return fantasy.NewAgentTool(
		FSReplaceToolName,
		fsReplaceDescription,
		func(ctx context.Context, params FSReplaceParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
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
				joined := filepathext.SmartJoin(workingDir, item.Path)
				if err := CheckForbiddenWrite(joined); err != nil {
					return fantasy.NewTextErrorResponse(err.Error()), nil
				}
				joinedPaths[i] = joined
			}

			// Every target path is authorized before any batch work, so a denial
			// ends the turn before the runner touches the filesystem.
			p, err := requestFSBatchPermissions(ctx, permissions, sessionID, call.ID, FSReplaceToolName, "write", fmt.Sprintf("replace content in %d files", len(params.Items)), joinedPaths, workingDir, FSReplacePermissionsParams{Paths: joinedPaths})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			resp, err := RunFSBatch(ctx, FSBatch[FSReplaceItem]{
				Tool:       FSReplaceToolName,
				WorkingDir: workingDir,
				Scope:      scope,
				Items:      params.Items,
				Disk:       disk,
				PathOf: func(i FSReplaceItem) string {
					return i.Path
				},
				Preflight: fsReplacePreflight,
				Execute: func(ctx context.Context, group FSBatchGroup[FSReplaceItem]) ([]FSItemOutcome, error) {
					return fsReplaceExecuteGroup(ctx, scope, files, filetracker, workingDir, disk, group)
				},
			})
			return resp, err
		},
	)
}

// fsReplacePreflight resolves each item's operation without touching the
// filesystem: replacing requires an existing file, so the op is always
// replace and existence is checked (as "file not found") during
// execution, which reads the file anyway.
func fsReplacePreflight(_ context.Context, item FSReplaceItem, _ int, _ string) (permission.FileOp, error) {
	return permission.FileOpReplace, nil
}

// fsReplaceExecuteGroup applies every item of one same-path group in
// memory, in order, and writes the file once, so a group is one atomic
// write and one history version regardless of item count.
func fsReplaceExecuteGroup(ctx context.Context, scope permission.FolderScope, files history.Service, filetracker filetracker.Service, workingDir string, disk DiskProvider, group FSBatchGroup[FSReplaceItem]) ([]FSItemOutcome, error) {
	disk = diskOrOS(disk)
	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return nil, &FSBatchAbortError{Err: errors.New("session_id is required")}
	}

	outcomes := make([]FSItemOutcome, len(group.Items))

	info, statErr := disk.Stat(ctx, group.Path)
	isDir := statErr == nil && info.IsDir()
	switch {
	case errors.Is(statErr, fs.ErrNotExist):
		for i := range outcomes {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  fmt.Sprintf("file not found: %s", group.Path),
			}
		}
		return outcomes, nil
	case isDir:
		for i := range outcomes {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  fmt.Sprintf("path is a directory, not a file: %s", group.Path),
			}
		}
		return outcomes, nil
	case statErr != nil:
		if osFailureIsFatal(statErr) {
			return nil, &FSBatchAbortError{Err: fmt.Errorf("failed to access file: %w", statErr)}
		}
		for i := range outcomes {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error: fmt.Sprintf("Cannot access %s: %v. The OS rejected this path — common causes are no permission somewhere along the path, a path component that is a file rather than a directory, or a name too long for the filesystem. Nothing was read or modified. Try a different path or a corrected form of this one.",
					group.Path, statErr),
			}
		}
		return outcomes, nil
	}

	if resp, tooLarge := checkEditFileSize(group.Path, info.Size()); tooLarge {
		for i := range outcomes {
			outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: resp.Content}
		}
		return outcomes, nil
	}

	lastRead := filetracker.LastReadTime(ctx, sessionID, group.Path)
	if lastRead.IsZero() {
		notRead := "you must read the file before replacing content in it. Use the fs_read tool first"
		// When the scope for this call lacks the read op, the generic
		// read-before-write advice is impossible to follow, so the
		// error must say the scope itself makes it unachievable.
		if scope.Check(group.Path, permission.FileOpRead) != nil {
			notRead = "the folder scope for this call grants replace but not read, so the file can never be read first — this item can never succeed under the current scope"
		}
		for i := range outcomes {
			outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: notRead}
		}
		return outcomes, nil
	}

	modTime := info.ModTime().Truncate(time.Second)
	if modTime.After(lastRead) {
		slog.Warn("File was modified externally since last read, proceeding with replace", "file", group.Path, "mod_time", modTime.Format(time.RFC3339), "last_read", lastRead.Format(time.RFC3339))
		filetracker.RecordRead(ctx, sessionID, group.Path)
	}

	content, readErr := disk.ReadFile(ctx, group.Path)
	if readErr != nil {
		if osFailureIsFatal(readErr) {
			return nil, &FSBatchAbortError{Err: fmt.Errorf("failed to read file: %w", readErr)}
		}
		for i := range outcomes {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error: fmt.Sprintf("Cannot read %s: %v. The file was found but could not be read — it may have been locked by another process, or the storage failed mid-read. Nothing was modified. Try the replace again or on a different file.",
					group.Path, readErr),
			}
		}
		return outcomes, nil
	}
	oldContent, isCrlf := fsext.ToUnixLineEndings(string(content))

	current := oldContent
	for i, member := range group.Items {
		if member.Item.OldString == "" {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  "old_string cannot be empty for content replacement",
			}
			continue
		}
		newContent, err := findAndReplace(current, member.Item.OldString, member.Item.NewString, member.Item.ReplaceAll)
		if err != nil {
			outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: err.Error()}
			continue
		}
		if newContent == current {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  "new content is the same as old content. No changes made.",
			}
			continue
		}
		diffText, additions, removals := diff.GenerateDiff(
			current, newContent, strings.TrimPrefix(group.Path, workingDir))
		outcomes[i] = FSItemOutcome{
			Status:    FSStatusOK,
			Additions: additions,
			Removals:  removals,
			Diff:      capFSDiff(diffText),
		}
		current = newContent
	}

	// Nothing changed: every item already failed, so the disk must
	// stay untouched and no write is issued.
	if current == oldContent {
		return outcomes, nil
	}

	writeContent := current
	if isCrlf {
		writeContent, _ = fsext.ToWindowsLineEndings(writeContent)
	}

	editCtx := editContext{
		ctx:         ctx,
		permissions: nil,
		files:       files,
		filetracker: filetracker,
		workingDir:  workingDir,
		disk:        disk,
	}
	resp, err := commitFileChange(editCtx, sessionID, group.Path, oldContent, writeContent)
	if err != nil {
		// commitFileChange only returns an error for level-3
		// failures: a fatal write error or a history-service
		// failure.
		return nil, &FSBatchAbortError{Err: err}
	}
	if resp.IsError {
		// One atomic write per group: a refusal to write fails the
		// whole group, so every ok item is downgraded.
		for i := range outcomes {
			if outcomes[i].Status == FSStatusOK {
				outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: resp.Content}
			}
		}
	}
	return outcomes, nil
}
