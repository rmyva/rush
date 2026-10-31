package tools

// Fork patch (concurrency): the file write at the bottom of NewWriteTool
// uses fsext.AtomicWriteFile (write-to-tmp + rename) instead of
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

//go:embed write.md
var writeDescription string

type WriteParams struct {
	FilePath    string `json:"file_path" description:"The path to the file to write"`
	Content     string `json:"content" description:"The content to write to the file"`
	AllowShrink bool   `json:"allow_shrink,omitempty" description:"Set true only to deliberately replace a large existing file with much smaller content (write is refused otherwise: use edit for partial changes)"`
}

type WritePermissionsParams struct {
	FilePath   string `json:"file_path"`
	OldContent string `json:"old_content,omitempty"`
	NewContent string `json:"new_content,omitempty"`
}

type WriteResponseMetadata struct {
	Diff      string `json:"diff"`
	Additions int    `json:"additions"`
	Removals  int    `json:"removals"`
}

const WriteToolName = "write"

func NewWriteTool(
	permissions permission.Service,
	files history.Service,
	filetracker filetracker.Service,
	workingDir string,
) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		WriteToolName,
		writeDescription,
		func(ctx context.Context, params WriteParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.FilePath == "" {
				return fantasy.NewTextErrorResponse("file_path is required"), nil
			}

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session_id is required")
			}

			filePath := filepathext.SmartJoin(workingDir, params.FilePath)

			if err := CheckForbiddenWrite(filePath); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}

			fileInfo, err := os.Stat(filePath)
			if err == nil {
				if fileInfo.IsDir() {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("Path is a directory, not a file: %s", filePath)), nil
				}
				if resp, tooLarge := checkEditFileSize(filePath, fileInfo.Size()); tooLarge {
					return resp, nil
				}

				modTime := fileInfo.ModTime().Truncate(time.Second)
				lastRead := filetracker.LastReadTime(ctx, sessionID, filePath)
				if modTime.After(lastRead) {
					// File was modified externally since last read, update the read time to allow the write
					slog.Warn("File was modified externally since last read, proceeding with write", "file", filePath, "mod_time", modTime.Format(time.RFC3339), "last_read", lastRead.Format(time.RFC3339))
					filetracker.RecordRead(ctx, sessionID, filePath)
				}

				oldContent, readErr := os.ReadFile(filePath)
				if readErr == nil && string(oldContent) == params.Content {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("File %s already contains the exact content. No changes made.", filePath)), nil
				}
				if readErr == nil && !params.AllowShrink && writeWouldShrinkFile(len(oldContent), len(params.Content)) {
					return fantasy.NewTextErrorResponse(writeShrinkRefusal(WriteToolName, "edit or multiedit", filePath, len(oldContent), len(params.Content))), nil
				}
			} else if !os.IsNotExist(err) {
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("error checking file: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Cannot access %s: %v. The OS rejected this path — common causes are no permission somewhere along the path, a path component that is a file rather than a directory, or an invalid character in the name. Nothing was written. Try a different path or a corrected form of this one.",
					filePath, err)), nil
			}

			dir := filepath.Dir(filePath)

			oldContent := ""
			if fileInfo != nil && !fileInfo.IsDir() {
				oldBytes, readErr := os.ReadFile(filePath)
				if readErr == nil {
					oldContent = string(oldBytes)
				}
			}

			diff, additions, removals := diff.GenerateDiff(
				oldContent,
				params.Content,
				strings.TrimPrefix(filePath, workingDir),
			)

			p, err := permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        fsext.PathOrPrefix(filePath, workingDir),
					ToolCallID:  call.ID,
					ToolName:    WriteToolName,
					Action:      "write",
					Description: fmt.Sprintf("Create file %s", filePath),
					Params: WritePermissionsParams{
						FilePath:   filePath,
						OldContent: oldContent,
						NewContent: params.Content,
					},
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				resp := NewPermissionDeniedResponse()
				resp = fantasy.WithResponseMetadata(resp, WriteResponseMetadata{
					Diff:      diff,
					Additions: additions,
					Removals:  removals,
				})
				return resp, nil
			}

			if err = os.MkdirAll(dir, 0o755); err != nil {
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("error creating directory: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Cannot create the parent directory for %s: %v. A path component is a file rather than a directory, the target exists with different permissions, or the OS refused to create it. Nothing was written. Try a different path or a corrected form of this one.",
					filePath, err)), nil
			}

			if err = CheckForbiddenWriteResolved(filePath); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			err = fsext.AtomicWriteFile(filePath, []byte(params.Content), 0o644)
			if err != nil {
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("error writing file: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Cannot write %s: %v. The OS refused the final write or rename — the file or a directory along its path is read-only, the target is a directory, or another process holds it. Nothing was changed and the temporary file was cleaned up. Try a different path, or fix the permissions and retry.",
					filePath, err)), nil
			}

			// Check if file exists in history
			file, err := files.GetByPathAndSession(ctx, filePath, sessionID)
			if err != nil {
				_, err = files.Create(ctx, sessionID, filePath, oldContent)
				if err != nil {
					// Log error but don't fail the operation
					return fantasy.ToolResponse{}, fmt.Errorf("error creating file history: %w", err)
				}
			}
			if file.Content != oldContent {
				// User manually changed the content; store an intermediate version
				_, err = files.CreateVersion(ctx, sessionID, filePath, oldContent)
				if err != nil {
					slog.Error("Error creating file history version", "error", err)
				}
			}
			// Store the new version
			_, err = files.CreateVersion(ctx, sessionID, filePath, params.Content)
			if err != nil {
				slog.Error("Error creating file history version", "error", err)
			}

			filetracker.RecordRead(ctx, sessionID, filePath)

			result := fmt.Sprintf("File successfully written: %s", filePath)
			result = fmt.Sprintf("<result>\n%s\n</result>", result)
			return fantasy.WithResponseMetadata(
				fantasy.NewTextResponse(result),
				WriteResponseMetadata{
					Diff:      diff,
					Additions: additions,
					Removals:  removals,
				},
			), nil
		},
	)
}
