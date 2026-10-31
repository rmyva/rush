package tools

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/filepathext"
	"github.com/PHPCraftdream/rush/internal/permission"
)

type DownloadParams struct {
	URL      string `json:"url" description:"The URL to download from"`
	FilePath string `json:"file_path" description:"The local file path where the downloaded content should be saved"`
	Timeout  int    `json:"timeout,omitempty" description:"Optional timeout in seconds (max 600)"`
}

type DownloadPermissionsParams struct {
	URL      string `json:"url"`
	FilePath string `json:"file_path"`
	Timeout  int    `json:"timeout,omitempty"`
}

const DownloadToolName = "download"

// MaxDownloadBytes bounds how much of the response body download will
// write to disk. Found missing by a full-project @rush --role reviewer
// audit: without a limit, a URL serving an unbounded stream (or a slow-drip
// server exploiting Timeout=0, which only bounds the HTTP client's overall
// deadline, not response size) could fill the disk. 500 MiB is generous for
// the tool's normal use (docs, small datasets, archives) while still being
// a hard ceiling.
const MaxDownloadBytes = 500 * 1024 * 1024

//go:embed download.md.tpl
var downloadDescriptionTmpl []byte

var downloadDescriptionTpl = template.Must(
	template.New("downloadDescription").
		Parse(string(downloadDescriptionTmpl)),
)

type downloadDescriptionData struct {
	MaxDownloadTimeout int
}

func downloadDescription() string {
	return renderTemplate(downloadDescriptionTpl, downloadDescriptionData{
		MaxDownloadTimeout: 600,
	})
}

func NewDownloadTool(permissions permission.Service, workingDir string, client *http.Client) fantasy.AgentTool {
	if client == nil {
		// Default client is SSRF-guarded: it blocks dials to
		// loopback/private/link-local/metadata ranges so a
		// prompt-injected or malicious model can't exfiltrate cloud
		// metadata (169.254.169.254 et al.) through a download. A
		// caller that legitimately needs loopback passes its own
		// client built with NewSSRFGuardedClient(timeout, true).
		client = NewSSRFGuardedClient(5*time.Minute, false)
	}
	return fantasy.NewParallelAgentTool(
		DownloadToolName,
		downloadDescription(),
		func(ctx context.Context, params DownloadParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.URL == "" {
				return fantasy.NewTextErrorResponse("URL parameter is required"), nil
			}

			if params.FilePath == "" {
				return fantasy.NewTextErrorResponse("file_path parameter is required"), nil
			}

			if !strings.HasPrefix(params.URL, "http://") && !strings.HasPrefix(params.URL, "https://") {
				return fantasy.NewTextErrorResponse("URL must start with http:// or https://"), nil
			}

			filePath := filepathext.SmartJoin(workingDir, params.FilePath)
			if err := CheckForbiddenWriteResolved(filePath); err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			relPath, _ := filepath.Rel(workingDir, filePath)
			relPath = filepath.ToSlash(cmp.Or(relPath, filePath))

			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session ID is required for downloading files")
			}

			p, err := permissions.Request(
				ctx,
				permission.CreatePermissionRequest{
					SessionID:   sessionID,
					Path:        filePath,
					ToolName:    DownloadToolName,
					Action:      "download",
					Description: fmt.Sprintf("Download file from URL: %s to %s", params.URL, filePath),
					Params:      DownloadPermissionsParams(params),
				},
			)
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !p {
				return NewPermissionDeniedResponse(), nil
			}

			// Handle timeout with context
			requestCtx := ctx
			if params.Timeout > 0 {
				maxTimeout := 600 // 10 minutes
				if params.Timeout > maxTimeout {
					params.Timeout = maxTimeout
				}
				var cancel context.CancelFunc
				requestCtx, cancel = context.WithTimeout(ctx, time.Duration(params.Timeout)*time.Second)
				defer cancel()
			}

			req, err := http.NewRequestWithContext(requestCtx, "GET", params.URL, nil)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"malformed URL %q: %v. The URL could not be parsed into a valid "+
						"HTTP request, so retrying it unchanged will fail the same "+
						"way — fix the url parameter (a well-formed absolute "+
						"http:// or https:// URL: no spaces, valid percent-escapes, "+
						"intact host) and call again. Nothing was downloaded and no "+
						"file was created.",
					params.URL, err,
				)), nil
			}

			req.Header.Set("User-Agent", "rush/1.0")

			resp, err := client.Do(req)
			if err != nil {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"download from %s failed at the network level: %v. The URL "+
						"itself is well-formed; the request just never completed — "+
						"unreachable host, DNS failure, connection refused or reset, "+
						"TLS error, timeout, or a blocked destination. That may be "+
						"transient: retry the same URL later, or try a different "+
						"one. Nothing was downloaded and no file was created.",
					params.URL, err,
				)), nil
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("Request failed with status code: %d", resp.StatusCode)), nil
			}

			// Create parent directories if they don't exist
			if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("failed to create parent directories: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"The download itself succeeded, but the parent directory for %s could not be created: %v. A path component of the file_path is a file rather than a directory, or the OS refused to create it. Nothing was saved. Retry with a corrected file_path.",
					filePath, err)), nil
			}

			// Write to a temp file in the same directory, then rename over
			// filePath — a network failure or ctx-cancel mid-copy must
			// never leave a truncated file at filePath for a later
			// view/edit to read as valid content. Mirrors
			// fsext.AtomicWriteFile's pattern, but streaming (via io.Copy
			// from a size-limited reader) since downloads can be large
			// enough that buffering the whole body first would be wasteful.
			tmpFile, err := os.CreateTemp(filepath.Dir(filePath), filepath.Base(filePath)+".*.tmp")
			if err != nil {
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("failed to create output file: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Cannot create a temporary file next to %s: %v. The directory likely denies file creation to this process — a permission setting or a lock on it. Nothing was saved. Retry with a different path.",
					filePath, err)), nil
			}
			tmpPath := tmpFile.Name()
			cleanupTmp := func() { _ = os.Remove(tmpPath) }

			// Read one byte past the limit so an oversized body is detected
			// (bytesWritten > MaxDownloadBytes) rather than silently
			// truncated to exactly the limit and reported as a "complete"
			// download.
			bytesWritten, err := io.Copy(tmpFile, io.LimitReader(resp.Body, MaxDownloadBytes+1))
			if err != nil {
				tmpFile.Close()
				cleanupTmp()
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("failed to write file: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"The copy to %s stopped midway: %v. The connection broke, or the disk or OS refused the write. The partial temporary file was cleaned up and nothing was saved. Retry the download or use a different path.",
					filePath, err)), nil
			}
			if bytesWritten > MaxDownloadBytes {
				tmpFile.Close()
				cleanupTmp()
				return fantasy.NewTextErrorResponse(fmt.Sprintf("download exceeds the %d byte limit", MaxDownloadBytes)), nil
			}
			// os.CreateTemp creates the file 0600; match fsext.AtomicWriteFile's
			// convention (chmod before rename) so a downloaded file ends up with
			// the same permissive-by-default mode os.Create used to give it,
			// instead of silently becoming unreadable by other local users/
			// processes (found by an independent @oh review of this fix).
			if err := tmpFile.Chmod(0o644); err != nil {
				tmpFile.Close()
				cleanupTmp()
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("failed to set output file permissions: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Could not set permissions on the temporary file for %s: %v. The temporary file was cleaned up and nothing was saved. Retry the download or use a different path.",
					filePath, err)), nil
			}
			if err := tmpFile.Sync(); err != nil {
				tmpFile.Close()
				cleanupTmp()
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("failed to flush output file: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Could not flush the temporary file for %s: %v. The temporary file was cleaned up and nothing was saved. Retry the download or use a different path.",
					filePath, err)), nil
			}
			if err := tmpFile.Close(); err != nil {
				cleanupTmp()
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("failed to close output file: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Could not close the temporary file for %s: %v. The temporary file was cleaned up and nothing was saved. Retry the download or use a different path.",
					filePath, err)), nil
			}
			if err := CheckForbiddenWriteResolved(filePath); err != nil {
				cleanupTmp()
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if err := os.Rename(tmpPath, filePath); err != nil {
				cleanupTmp()
				if osFailureIsFatal(err) {
					return fantasy.ToolResponse{}, fmt.Errorf("failed to finalize downloaded file: %w", err)
				}
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"Cannot finalize the download at %s: %v. The target may be an existing directory, a read-only file, or held by another process. The downloaded bytes were removed with the temporary file. Retry with a corrected file_path.",
					filePath, err)), nil
			}

			contentType := resp.Header.Get("Content-Type")
			responseMsg := fmt.Sprintf("Successfully downloaded %d bytes to %s", bytesWritten, relPath)
			if contentType != "" {
				responseMsg += fmt.Sprintf(" (Content-Type: %s)", contentType)
			}

			return fantasy.NewTextResponse(responseMsg), nil
		},
	)
}
