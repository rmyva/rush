package tools

// Fork patch (concurrency): the three file writes in this file
// (createNewFile, deleteContent, replaceContent) all use
// fsext.AtomicWriteFile (write-to-tmp + rename) instead of
// os.WriteFile so a kill -9 / OOM mid-write cannot leave the user's
// file half-truncated. See CHANGELOG.fork.md (Section 4.I).

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

type EditParams struct {
	FilePath   string `json:"file_path" description:"The absolute path to the file to modify"`
	OldString  string `json:"old_string" description:"The text to replace"`
	NewString  string `json:"new_string" description:"The text to replace it with"`
	ReplaceAll bool   `json:"replace_all,omitempty" description:"Replace all occurrences of old_string (default false)"`
}

type EditPermissionsParams struct {
	FilePath   string `json:"file_path"`
	OldContent string `json:"old_content,omitempty"`
	NewContent string `json:"new_content,omitempty"`
}

type EditResponseMetadata struct {
	Additions  int    `json:"additions"`
	Removals   int    `json:"removals"`
	OldContent string `json:"old_content,omitempty"`
	NewContent string `json:"new_content,omitempty"`
}

const EditToolName = "edit"

// editMaxFileSizeBytes bounds the size of a file the edit tool will read
// into memory. Reading a file means os.ReadFile, which allocates the whole
// file size up front; a single Edit call pointed at a multi-gigabyte file
// (a generated data dump, a log, a scratch file in the working tree) can
// therefore VirtualAlloc its way into a fatal Go out-of-memory error that
// kills the whole Rush process — session, lock lifecycle and pending
// completion reporting included. 256 MiB is far above any source file
// (this repository's largest .go file is ~90 KB), and even with ~2x
// transient overhead during the string replacement that lands well below
// operator memory ceilings.
const editMaxFileSizeBytes = 256 * 1024 * 1024

// editMaxFileSize is the mutable form of editMaxFileSizeBytes so tests can
// shrink the bound instead of allocating hundreds of megabytes. Production
// code only ever reads it; it is assigned exactly once, at initialization.
var editMaxFileSize int64 = editMaxFileSizeBytes

// checkEditFileSize fails fast, before any read or allocation, when an
// existing file is larger than the edit tool's read bound. It returns a
// text-error response (the tool's standard shape for correctable input)
// naming the file, its exact byte size, the limit, and how to proceed.
func checkEditFileSize(filePath string, size int64) (fantasy.ToolResponse, bool) {
	if size <= editMaxFileSize {
		return fantasy.ToolResponse{}, false
	}
	return fantasy.NewTextErrorResponse(fmt.Sprintf(
		"file is too large to edit: %s is %d bytes, which exceeds the %d byte limit. Nothing was read or modified. Use a narrower tool (bash with sed or scripted processing) or trim the file before editing.",
		filePath, size, editMaxFileSize)), true
}

//go:embed edit.md
var editDescription string

type editContext struct {
	ctx         context.Context
	permissions permission.Service
	files       history.Service
	filetracker filetracker.Service
	workingDir  string
	// disk is the filesystem commitFileChange's final write lands on.
	// The zero value (nil) means the real disk (see diskOrOS): every
	// legacy caller (edit, write, multiedit) constructs editContext
	// without ever setting this field, so they are unaffected. Only
	// fs_write/fs_replace/fs_write_lines set it, to an already-
	// normalised non-nil DiskProvider.
	disk DiskProvider
}

func NewEditTool(
	permissions permission.Service,
	files history.Service,
	filetracker filetracker.Service,
	workingDir string,
) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		EditToolName,
		editDescription,
		func(ctx context.Context, params EditParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.FilePath == "" {
				return fantasy.NewTextErrorResponse("file_path is required"), nil
			}

			params.FilePath = filepathext.SmartJoin(workingDir, params.FilePath)

			if err := CheckForbiddenWrite(params.FilePath); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			var response fantasy.ToolResponse
			var err error

			editCtx := editContext{ctx, permissions, files, filetracker, workingDir, nil}

			if params.OldString == "" {
				response, err = createNewFile(editCtx, params.FilePath, params.NewString, call)
			} else if params.NewString == "" {
				response, err = deleteContent(editCtx, params.FilePath, params.OldString, params.ReplaceAll, call)
			} else {
				response, err = replaceContent(editCtx, params.FilePath, params.OldString, params.NewString, params.ReplaceAll, call)
			}

			if err != nil {
				return response, err
			}
			if response.IsError {
				return response, nil
			}

			response.Content = fmt.Sprintf("<result>\n%s\n</result>\n", response.Content)
			return response, nil
		},
	)
}

func createNewFile(edit editContext, filePath, content string, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	fileInfo, err := os.Stat(filePath)
	if err == nil {
		if fileInfo.IsDir() {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("path is a directory, not a file: %s", filePath)), nil
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf("file already exists: %s", filePath)), nil
	} else if !os.IsNotExist(err) {
		if osFailureIsFatal(err) {
			return fantasy.ToolResponse{}, fmt.Errorf("failed to access file: %w", err)
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"Cannot access %s: %v. The OS rejected this path — common causes are no permission somewhere along the path, a path component that is a file rather than a directory, a name too long for the filesystem, or an invalid character in the name. The file was not created and nothing was written. Try a different path or a corrected form of this one.",
			filePath, err)), nil
	}

	dir := filepath.Dir(filePath)

	sessionID := GetSessionFromContext(edit.ctx)
	if sessionID == "" {
		return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for creating a new file")
	}

	_, additions, removals := diff.GenerateDiff(
		"",
		content,
		strings.TrimPrefix(filePath, edit.workingDir),
	)
	p, err := edit.permissions.Request(
		edit.ctx,
		permission.CreatePermissionRequest{
			SessionID:   sessionID,
			Path:        fsext.PathOrPrefix(filePath, edit.workingDir),
			ToolCallID:  call.ID,
			ToolName:    EditToolName,
			Action:      "write",
			Description: fmt.Sprintf("Create file %s", filePath),
			Params: EditPermissionsParams{
				FilePath:   filePath,
				OldContent: "",
				NewContent: content,
			},
		},
	)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if !p {
		resp := NewPermissionDeniedResponse()
		resp = fantasy.WithResponseMetadata(resp, EditResponseMetadata{
			OldContent: "",
			NewContent: content,
			Additions:  additions,
			Removals:   removals,
		})
		return resp, nil
	}

	if err := CheckForbiddenWriteResolved(filePath); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if err = os.MkdirAll(dir, 0o755); err != nil {
		if osFailureIsFatal(err) {
			return fantasy.ToolResponse{}, fmt.Errorf("failed to create parent directories: %w", err)
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"Cannot create the parent directory for %s: %v. A path component is a file rather than a directory, the target exists with different permissions, or the OS refused to create it. The file was not created and nothing was written. Try a different path or a corrected form of this one.",
			filePath, err)), nil
	}

	if err := CheckForbiddenWriteResolved(filePath); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	err = fsext.AtomicWriteFile(filePath, []byte(content), 0o644)
	if err != nil {
		if osFailureIsFatal(err) {
			return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"Cannot write %s: %v. The OS refused the final write or rename — the file or a directory along its path is read-only, the target is a directory, or another process holds it. Nothing was changed and the temporary file was cleaned up. Try a different path, or fix the permissions and retry.",
			filePath, err)), nil
	}

	// File can't be in the history so we create a new file history
	_, err = edit.files.Create(edit.ctx, sessionID, filePath, "")
	if err != nil {
		// Log error but don't fail the operation
		return fantasy.ToolResponse{}, fmt.Errorf("error creating file history: %w", err)
	}

	// Add the new content to the file history
	_, err = edit.files.CreateVersion(edit.ctx, sessionID, filePath, content)
	if err != nil {
		// Log error but don't fail the operation
		slog.Error("Error creating file history version", "error", err)
	}

	edit.filetracker.RecordRead(edit.ctx, sessionID, filePath)

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse("File created: "+filePath),
		EditResponseMetadata{
			OldContent: "",
			NewContent: content,
			Additions:  additions,
			Removals:   removals,
		},
	), nil
}

// findAndReplace performs a find-and-replace on content. When replaceAll is
// false it requires exactly one match. Returns the new content or an error
// describing why the replacement could not be made.
func findAndReplace(content, old, new string, replaceAll bool) (string, error) {
	if replaceAll {
		if !strings.Contains(content, old) {
			return "", fmt.Errorf("old_string not found in file. Make sure it matches exactly, including whitespace and line breaks")
		}
		return strings.ReplaceAll(content, old, new), nil
	}

	index := strings.Index(content, old)
	if index == -1 {
		return "", fmt.Errorf("old_string not found in file. Make sure it matches exactly, including whitespace and line breaks")
	}

	lastIndex := strings.LastIndex(content, old)
	if index != lastIndex {
		return "", fmt.Errorf("old_string appears multiple times in the file. Please provide more context to ensure a unique match, or set replace_all to true")
	}

	return content[:index] + new + content[index+len(old):], nil
}

// commitFileChange writes newContent to filePath, updates the file history,
// and records the read in the file tracker. Callers must convert line
// endings before calling this function. It returns a Go error only for
// contract-level-3 failures — media/resource write errors per
// osFailureIsFatal, or a history-service failure; any other write or
// rename refusal comes back as a level-1 text-error response with a nil
// error, so the model can correct the path and retry. Callers must check
// both returns and hand an error response straight back to the model.
//
// Fork patch (concurrency): uses fsext.AtomicWriteFile (write-to-tmp +
// rename) instead of os.WriteFile so a kill -9 / OOM mid-write cannot leave
// the user's file half-truncated. See CHANGELOG.fork.md (Section 4.I).
func commitFileChange(edit editContext, sessionID, filePath, oldContent, newContent string) (fantasy.ToolResponse, error) {
	if err := CheckForbiddenWriteResolved(filePath); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if err := diskOrOS(edit.disk).WriteFile(edit.ctx, filePath, []byte(newContent), 0o644); err != nil {
		if osFailureIsFatal(err) {
			return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
		}
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"Cannot write %s: %v. The OS refused the final write or rename — the file or a directory along its path is read-only, the target is a directory, or another process holds it. Nothing was changed and the temporary file was cleaned up. Try a different path, or fix the permissions and retry.",
			filePath, err)), nil
	}

	file, err := edit.files.GetByPathAndSession(edit.ctx, filePath, sessionID)
	if err != nil {
		_, err = edit.files.Create(edit.ctx, sessionID, filePath, oldContent)
		if err != nil {
			return fantasy.ToolResponse{}, fmt.Errorf("error creating file history: %w", err)
		}
	}
	if file.Content != oldContent {
		// User manually changed the content; store an intermediate version.
		if _, err := edit.files.CreateVersion(edit.ctx, sessionID, filePath, oldContent); err != nil {
			slog.Error("Error creating file history version", "error", err)
		}
	}
	if _, err := edit.files.CreateVersion(edit.ctx, sessionID, filePath, newContent); err != nil {
		slog.Error("Error creating file history version", "error", err)
	}

	edit.filetracker.RecordRead(edit.ctx, sessionID, filePath)
	return fantasy.ToolResponse{}, nil
}

// loadExistingFile stats and reads filePath, validating that it has been
// read by the caller before editing.
//
// Fork merge note: upstream returns a hard error when the file's mtime is
// newer than the last recorded read. In orchestrator mode external
// modifications are normal (other tools, CI regenerators), so instead of
// bailing we warn and refresh the tracker so the edit proceeds.
func loadExistingFile(edit editContext, filePath, sessionError string) (sessionID, oldContent string, isCrlf bool, resp fantasy.ToolResponse, err error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", false, fantasy.NewTextErrorResponse(fmt.Sprintf("file not found: %s", filePath)), nil
		}
		if osFailureIsFatal(err) {
			return "", "", false, fantasy.ToolResponse{}, fmt.Errorf("failed to access file: %w", err)
		}
		return "", "", false, fantasy.NewTextErrorResponse(fmt.Sprintf(
			"Cannot access %s: %v. The OS rejected this path — common causes are no permission somewhere along the path, a path component that is a file rather than a directory, or a name too long for the filesystem. Nothing was read or modified. Try a different path or a corrected form of this one.",
			filePath, err)), nil
	}

	if fileInfo.IsDir() {
		return "", "", false, fantasy.NewTextErrorResponse(fmt.Sprintf("path is a directory, not a file: %s", filePath)), nil
	}

	// Size guard first: os.ReadFile allocates the entire file size up
	// front, so the stat's size is the only thing standing between an
	// oversized file and a process-wide fatal OOM. The stat is already in
	// hand, so this costs one comparison.
	if resp, tooLarge := checkEditFileSize(filePath, fileInfo.Size()); tooLarge {
		return "", "", false, resp, nil
	}

	sessionID = GetSessionFromContext(edit.ctx)
	if sessionID == "" {
		return "", "", false, fantasy.ToolResponse{}, fmt.Errorf("%s", sessionError)
	}

	lastRead := edit.filetracker.LastReadTime(edit.ctx, sessionID, filePath)
	if lastRead.IsZero() {
		return "", "", false, fantasy.NewTextErrorResponse("you must read the file before editing it. Use the View tool first"), nil
	}

	modTime := fileInfo.ModTime().Truncate(time.Second)
	if modTime.After(lastRead) {
		slog.Warn("File was modified externally since last read, proceeding with edit", "file", filePath, "mod_time", modTime.Format(time.RFC3339), "last_read", lastRead.Format(time.RFC3339))
		edit.filetracker.RecordRead(edit.ctx, sessionID, filePath)
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		if osFailureIsFatal(err) {
			return "", "", false, fantasy.ToolResponse{}, fmt.Errorf("failed to read file: %w", err)
		}
		return "", "", false, fantasy.NewTextErrorResponse(fmt.Sprintf(
			"Cannot read %s: %v. The file was found but could not be read — it may have been locked by another process, or the storage failed mid-read. Nothing was modified. Try the edit again or on a different file.",
			filePath, err)), nil
	}

	oldContent, isCrlf = fsext.ToUnixLineEndings(string(content))
	return sessionID, oldContent, isCrlf, fantasy.ToolResponse{}, nil
}

func deleteContent(edit editContext, filePath, oldString string, replaceAll bool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	sessionID, oldContent, isCrlf, resp, err := loadExistingFile(edit, filePath, "session ID is required for deleting content")
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if resp.Content != "" || resp.IsError {
		return resp, nil
	}

	newContent, err := findAndReplace(oldContent, oldString, "", replaceAll)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	_, additions, removals := diff.GenerateDiff(
		oldContent,
		newContent,
		strings.TrimPrefix(filePath, edit.workingDir),
	)

	p, err := edit.permissions.Request(
		edit.ctx,
		permission.CreatePermissionRequest{
			SessionID:   sessionID,
			Path:        fsext.PathOrPrefix(filePath, edit.workingDir),
			ToolCallID:  call.ID,
			ToolName:    EditToolName,
			Action:      "write",
			Description: fmt.Sprintf("Delete content from file %s", filePath),
			Params: EditPermissionsParams{
				FilePath:   filePath,
				OldContent: oldContent,
				NewContent: newContent,
			},
		},
	)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if !p {
		resp := NewPermissionDeniedResponse()
		resp = fantasy.WithResponseMetadata(resp, EditResponseMetadata{
			OldContent: oldContent,
			NewContent: newContent,
			Additions:  additions,
			Removals:   removals,
		})
		return resp, nil
	}

	writeContent := newContent
	if isCrlf {
		writeContent, _ = fsext.ToWindowsLineEndings(writeContent)
	}

	if resp, err := commitFileChange(edit, sessionID, filePath, oldContent, writeContent); err != nil {
		return fantasy.ToolResponse{}, err
	} else if resp.IsError {
		return resp, nil
	}

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse("Content deleted from file: "+filePath),
		EditResponseMetadata{
			OldContent: oldContent,
			NewContent: writeContent,
			Additions:  additions,
			Removals:   removals,
		},
	), nil
}

func replaceContent(edit editContext, filePath, oldString, newString string, replaceAll bool, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	sessionID, oldContent, isCrlf, resp, err := loadExistingFile(edit, filePath, "session ID is required for editing a file")
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if resp.Content != "" || resp.IsError {
		return resp, nil
	}

	result, err := findAndReplace(oldContent, oldString, newString, replaceAll)
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if result == oldContent {
		return fantasy.NewTextErrorResponse("new content is the same as old content. No changes made."), nil
	}

	_, additions, removals := diff.GenerateDiff(
		oldContent,
		result,
		strings.TrimPrefix(filePath, edit.workingDir),
	)

	p, err := edit.permissions.Request(
		edit.ctx,
		permission.CreatePermissionRequest{
			SessionID:   sessionID,
			Path:        fsext.PathOrPrefix(filePath, edit.workingDir),
			ToolCallID:  call.ID,
			ToolName:    EditToolName,
			Action:      "write",
			Description: fmt.Sprintf("Replace content in file %s", filePath),
			Params: EditPermissionsParams{
				FilePath:   filePath,
				OldContent: oldContent,
				NewContent: result,
			},
		},
	)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if !p {
		resp := NewPermissionDeniedResponse()
		resp = fantasy.WithResponseMetadata(resp, EditResponseMetadata{
			OldContent: oldContent,
			NewContent: result,
			Additions:  additions,
			Removals:   removals,
		})
		return resp, nil
	}

	writeContent := result
	if isCrlf {
		writeContent, _ = fsext.ToWindowsLineEndings(writeContent)
	}

	if resp, err := commitFileChange(edit, sessionID, filePath, oldContent, writeContent); err != nil {
		return fantasy.ToolResponse{}, err
	} else if resp.IsError {
		return resp, nil
	}

	return fantasy.WithResponseMetadata(
		fantasy.NewTextResponse("Content replaced in file: "+filePath),
		EditResponseMetadata{
			OldContent: oldContent,
			NewContent: writeContent,
			Additions:  additions,
			Removals:   removals,
		},
	), nil
}
