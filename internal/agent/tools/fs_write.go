package tools

// fs_write is the batch-capable member of the scoped fs_* write family:
// N files, one permission request per target, per-item outcomes via RunFSBatch, and
// one atomic write plus one history version per distinct file. It reuses
// edit.go's commitFileChange so durability, history and read-tracking
// behave exactly like the single-file write path.

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/diff"
	"github.com/PHPCraftdream/rush/internal/filepathext"
	"github.com/PHPCraftdream/rush/internal/filetracker"
	"github.com/PHPCraftdream/rush/internal/history"
	"github.com/PHPCraftdream/rush/internal/permission"
)

//go:embed fs_write.md
var fsWriteDescription string

type FSWriteItem struct {
	Path       string `json:"path" description:"File path, absolute or relative to the working directory"`
	Content    string `json:"content" description:"Complete new content of the file"`
	CreateOnly bool   `json:"create_only,omitempty" description:"Fail this item if the file already exists (create, never overwrite)"`
	// AllowShrink: see the shrink guard in write_shrink_guard.go.
	AllowShrink bool `json:"allow_shrink,omitempty" description:"Set true only to deliberately replace a large existing file with much smaller content (refused otherwise: use fs_replace or fs_write_lines for partial changes)"`
}

type FSWriteParams struct {
	Items []FSWriteItem `json:"items" description:"Files to write. Each item is validated and reported independently; existing files are overwritten unless create_only is set"`
}

type FSWritePermissionsParams struct {
	Paths []string `json:"paths"`
}

const FSWriteToolName = "fs_write"

func NewFSWriteTool(scope permission.FolderScope, permissions permission.Service, files history.Service, filetracker filetracker.Service, workingDir string, disk DiskProvider) fantasy.AgentTool {
	disk = diskOrOS(disk)
	return fantasy.NewAgentTool(
		FSWriteToolName,
		fsWriteDescription,
		func(ctx context.Context, params FSWriteParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
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

			// One whole-call gate, multiedit's precedent: every target path is
			// authorized before any batch work, so a denial ends the turn
			// before the runner touches the filesystem.
			p, err := requestFSBatchPermissions(ctx, permissions, sessionID, call.ID, FSWriteToolName, "write", fmt.Sprintf("write %d files", len(params.Items)), joinedPaths, workingDir, FSWritePermissionsParams{Paths: joinedPaths})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			resp, err := RunFSBatch(ctx, FSBatch[FSWriteItem]{
				Tool:       FSWriteToolName,
				WorkingDir: workingDir,
				Scope:      scope,
				Items:      params.Items,
				Disk:       disk,
				PathOf: func(i FSWriteItem) string {
					return i.Path
				},
				Preflight: fsWritePreflight(disk),
				Execute: func(ctx context.Context, group FSBatchGroup[FSWriteItem]) ([]FSItemOutcome, error) {
					return fsWriteExecuteGroup(ctx, scope, files, filetracker, workingDir, disk, group)
				},
			})
			return resp, err
		},
	)
}

// fsWritePreflight resolves each item's operation without touching the
// filesystem beyond read-only stats, so the runner's scope check sees
// create-vs-overwrite exactly as execution will.
func fsWritePreflight(disk DiskProvider) FSPreflightFunc[FSWriteItem] {
	return func(ctx context.Context, item FSWriteItem, _ int, absPath string) (permission.FileOp, error) {
		info, err := disk.Stat(ctx, absPath)
		switch {
		case err == nil:
			if info.IsDir() {
				return "", fmt.Errorf("path is a directory, not a file: %s", absPath)
			}
			if item.CreateOnly {
				return "", fmt.Errorf("file already exists: %s", absPath)
			}
			return permission.FileOpOverwrite, nil
		case errors.Is(err, fs.ErrNotExist):
			return permission.FileOpCreate, nil
		default:
			return "", fmt.Errorf("cannot access %s: %w", absPath, err)
		}
	}
}

// fsWriteExecuteGroup applies every item of one same-path group in
// memory (last write wins) and writes the file once, so a group is one
// atomic write and one history version regardless of item count.
func fsWriteExecuteGroup(ctx context.Context, scope permission.FolderScope, files history.Service, filetracker filetracker.Service, workingDir string, disk DiskProvider, group FSBatchGroup[FSWriteItem]) ([]FSItemOutcome, error) {
	disk = diskOrOS(disk)
	sessionID := GetSessionFromContext(ctx)
	if sessionID == "" {
		return nil, &FSBatchAbortError{Err: errors.New("session_id is required")}
	}

	outcomes := make([]FSItemOutcome, len(group.Items))

	info, statErr := disk.Stat(ctx, group.Path)
	exists := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		for i := range outcomes {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  fmt.Sprintf("cannot access %s: %v", group.Path, statErr),
			}
		}
		return outcomes, nil
	}

	if exists {
		if !info.IsDir() {
			if resp, tooLarge := checkEditFileSize(group.Path, info.Size()); tooLarge {
				for i := range outcomes {
					outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: resp.Content}
				}
				return outcomes, nil
			}
		}
	}

	op := permission.FileOpCreate
	if exists {
		op = permission.FileOpOverwrite
	}

	// Defense in depth: the runner scope-checked each item's full file
	// path during preflight, but MkdirAll below is the step that
	// actually creates directories on disk, so re-check the resolved
	// parent at mutation time. This closes the case where scope was
	// narrowed between check and write, or where a direct caller (as
	// opposed to the runner) hands in a group whose parent was never
	// granted at all.
	denyAll := func(err error) ([]FSItemOutcome, error) {
		reason := err.Error()
		var denied *permission.ScopeDeniedError
		if errors.As(err, &denied) {
			reason = denied.Reason
		}
		for i := range outcomes {
			outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: reason}
		}
		return outcomes, nil
	}
	if err := scope.Check(filepath.Dir(group.Path), op); err != nil {
		return denyAll(err)
	}
	// When the file is new, MkdirAll also creates every missing
	// ancestor above the parent, and creating those directories is a
	// scope escape the file-path check cannot see, so each missing
	// ancestor gets its own check up to the first existing one.
	if !exists {
		for dir := filepath.Dir(group.Path); ; {
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			if _, err := disk.Stat(ctx, parent); err == nil {
				break
			}
			if err := scope.Check(parent, op); err != nil {
				return denyAll(err)
			}
			dir = parent
		}
	}

	if !exists {
		if err := disk.MkdirAll(ctx, filepath.Dir(group.Path), 0o755); err != nil {
			if osFailureIsFatal(err) {
				return nil, &FSBatchAbortError{Err: fmt.Errorf("error creating directory: %w", err)}
			}
			return nil, fmt.Errorf("cannot create the parent directory for %s: %v", group.Path, err)
		}
	}

	// The old content is the undo/diff baseline commitFileChange relies
	// on. A failed read after Stat already reported the file exists
	// (transient provider error, permission race, ...) must never be
	// silently treated as "empty": that would make the write look like
	// a from-scratch create and record a false history baseline. Fail
	// the group the same way a fatal/non-fatal MkdirAll error above
	// does, and return before WriteFile or commitFileChange run.
	oldContent := ""
	if exists {
		oldBytes, err := disk.ReadFile(ctx, group.Path)
		if err != nil {
			if osFailureIsFatal(err) {
				return nil, &FSBatchAbortError{Err: fmt.Errorf("error reading existing content of %s: %w", group.Path, err)}
			}
			return nil, fmt.Errorf("cannot read existing content of %s: %v", group.Path, err)
		}
		oldContent = string(oldBytes)
	}
	current := oldContent

	for i, member := range group.Items {
		if exists && member.Item.CreateOnly {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  "file already exists: " + group.Path,
			}
			continue
		}
		if member.Item.Content == current {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  fmt.Sprintf("File %s already contains the exact content. No changes made.", group.Path),
			}
			continue
		}
		// Baseline is the larger of disk and in-call size: items must not compound.
		baseline := max(len(oldContent), len(current))
		if exists && !member.Item.AllowShrink && writeWouldShrinkFile(baseline, len(member.Item.Content)) {
			outcomes[i] = FSItemOutcome{
				Status: FSStatusFailed,
				Error:  writeShrinkRefusal(FSWriteToolName, "fs_replace or fs_write_lines", group.Path, baseline, len(member.Item.Content)),
			}
			continue
		}
		diffText, additions, removals := diff.GenerateDiff(
			current, member.Item.Content, strings.TrimPrefix(group.Path, workingDir))
		outcomes[i] = FSItemOutcome{
			Status:    FSStatusOK,
			Additions: additions,
			Removals:  removals,
			Diff:      capFSDiff(diffText),
		}
		current = member.Item.Content
	}

	// Net-zero across the group: items cancelled each other out, so
	// the disk must stay untouched and no item can be reported as ok.
	if current == oldContent {
		for i := range outcomes {
			if outcomes[i].Status == FSStatusOK {
				outcomes[i] = FSItemOutcome{
					Status: FSStatusFailed,
					Error:  "no changes made - the file content is identical after applying all items",
				}
			}
		}
		return outcomes, nil
	}

	editCtx := editContext{
		ctx:         ctx,
		permissions: nil,
		files:       files,
		filetracker: filetracker,
		workingDir:  workingDir,
		disk:        disk,
	}
	resp, err := commitFileChange(editCtx, sessionID, group.Path, oldContent, current)
	if err != nil {
		// commitFileChange only returns an error for level-3
		// failures: a fatal write error or a history-service
		// failure.
		return nil, &FSBatchAbortError{Err: err}
	}
	if resp.IsError {
		for i := range outcomes {
			if outcomes[i].Status == FSStatusOK {
				outcomes[i] = FSItemOutcome{Status: FSStatusFailed, Error: resp.Content}
			}
		}
	}
	return outcomes, nil
}

// fsMaxDiffBytes caps the diff text one item reports. The batch runner
// carries FSItemOutcome.Diff verbatim, so the producing tool owns the cap.
const fsMaxDiffBytes = 8 * 1024

// capFSDiff truncates a diff to fsMaxDiffBytes at a line boundary, with an
// explicit truncation marker so the model can see the diff was cut.
func capFSDiff(diffText string) string {
	if len(diffText) <= fsMaxDiffBytes {
		return diffText
	}
	cut := diffText[:fsMaxDiffBytes]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	}
	return cut + "\n... (diff truncated)"
}
