package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/platform"
	"github.com/spf13/cobra"
)

var sessionsDiffCmd = &cobra.Command{
	Use:   "diff <session-id>",
	Short: "Show files touched by a session",
	Long: `Analyze tool calls in a session to find files that were created, edited,
or written by the agent. Works by scanning messages for ToolCall parts
where the tool name is edit, write, multiedit, or create — no database
migration needed.

For each file found, tries git diff --name-status HEAD to show the
current working-tree change (M/A/D). If not in a git repo or git fails,
the file is listed without a status letter.

Use --stat to include line-change statistics per file.
Use --json for machine-readable output.`,
	Example: `
# Show files touched by a session
rush sessions diff myid-123

# Include line-change statistics
rush sessions diff myid-123 --stat

# Machine-readable output
rush sessions diff myid-123 --json | jq '.files[] | .path'
  `,
	Args: cobra.ExactArgs(1),
	RunE: sessionsDiffCmdRun,
}

// fileWritingTools are tool names that write to files.
var fileWritingTools = map[string]bool{
	"edit": true, "write": true, "multiedit": true, "create": true,
}

type diffFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

type diffOutput struct {
	SessionID string     `json:"session_id"`
	Files     []diffFile `json:"files"`
}

func sessionsDiffCmdRun(cmd *cobra.Command, args []string) error {
	showStat, _ := cmd.Flags().GetBool("stat")
	asJSON, _ := cmd.Flags().GetBool("json")

	a, err := setupApp(cmd)
	if err != nil {
		return err
	}
	defer a.Shutdown()

	sess, err := resolveSessionID(cmd.Context(), a.Sessions, args[0])
	if err != nil {
		return err
	}

	msgs, err := a.Messages.List(cmd.Context(), sess.ID)
	if err != nil {
		return fmt.Errorf("failed to list messages: %w", err)
	}

	// Collect file paths from ToolCall parts.
	fileSet := make(map[string]bool)
	for _, msg := range msgs {
		for _, part := range msg.Parts {
			tc, ok := part.(message.ToolCall)
			if !ok {
				continue
			}
			if !fileWritingTools[strings.ToLower(tc.Name)] {
				continue
			}
			paths := extractFilePaths(tc.Name, tc.Input)
			for _, p := range paths {
				fileSet[p] = true
			}
		}
	}

	if len(fileSet) == 0 {
		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			_ = enc.Encode(diffOutput{
				SessionID: sess.ID,
				Files:     []diffFile{},
			})
		} else {
			fmt.Fprintf(os.Stderr, "session %s: no file-modifying tool calls found\n", truncate(sess.Title, 40))
		}
		return nil
	}

	// Sort paths.
	paths := make([]string, 0, len(fileSet))
	for p := range fileSet {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	// Try git status for each file.
	gitStatus := make(map[string]string)
	gitAvailable := isGitAvailable()
	if gitAvailable {
		for _, p := range paths {
			gitStatus[p] = gitNameStatus(p)
		}
	}

	if asJSON {
		files := make([]diffFile, len(paths))
		for i, p := range paths {
			files[i] = diffFile{Path: p, Status: gitStatus[p]}
		}
		enc := json.NewEncoder(os.Stdout)
		return enc.Encode(diffOutput{
			SessionID: sess.ID,
			Files:     files,
		})
	}

	fmt.Fprintf(os.Stdout, "Files touched by session %s:\n", truncate(sess.Title, 40))
	for _, p := range paths {
		status := gitStatus[p]
		if status == "" {
			status = "?"
		}
		fmt.Fprintf(os.Stdout, "%s  %s\n", status, p)
		if showStat && gitAvailable {
			stat := gitDiffStat(p)
			if stat != "" {
				fmt.Fprintf(os.Stdout, "    %s\n", stat)
			}
		}
	}
	fmt.Fprintf(os.Stdout, "\n%d files changed\n", len(paths))
	return nil
}

// extractFilePaths parses the JSON input of a tool call to find file_path
// values. For multiedit, iterates the edits array.
func extractFilePaths(toolName, input string) []string {
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &object); err != nil {
		return nil
	}
	if strings.ToLower(toolName) != "multiedit" {
		fp := decodeJSONString(object["file_path"])
		if fp == "" {
			return nil
		}
		return []string{fp}
	}
	var edits []map[string]json.RawMessage
	if err := json.Unmarshal(object["edits"], &edits); err != nil {
		return nil
	}
	var paths []string
	for _, edit := range edits {
		if fp := decodeJSONString(edit["file_path"]); fp != "" {
			paths = append(paths, fp)
		}
	}
	return paths
}

func decodeJSONString(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

// extractJSONStringValue extracts a single string value for a key from JSON.
func extractJSONStringValue(jsonStr, key string) string {
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(jsonStr), &object) != nil {
		return ""
	}
	return decodeJSONString(object[key])
}

func extractJSONStringValues(jsonStr, key string) []string {
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(jsonStr), &object) != nil {
		return nil
	}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(object["edits"], &entries) != nil {
		return nil
	}
	var values []string
	for _, entry := range entries {
		if value := decodeJSONString(entry[key]); value != "" {
			values = append(values, value)
		}
	}
	return values
}

func isGitAvailable() bool {
	_, err := exec.LookPath("git")
	if err != nil {
		return false
	}
	cmd := platform.Command(context.Background(), "git", "rev-parse", "--is-inside-work-tree")
	cmd.Stderr = nil
	cmd.Stdout = nil
	return cmd.Run() == nil
}

// gitNameStatus returns the git status letter (M/A/D) for a file, or "" on error.
func gitNameStatus(path string) string {
	cmd := platform.Command(context.Background(), "git", "diff", "--name-status", "HEAD", "--", path)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		// Maybe untracked.
		cmd2 := platform.Command(context.Background(), "git", "ls-files", "--error-unmatch", "--", path)
		if cmd2.Run() != nil {
			return "?"
		}
		return ""
	}
	fields := strings.Fields(line)
	if len(fields) >= 1 {
		return fields[0]
	}
	return ""
}

// gitDiffStat returns the git diff --stat output for a file.
func gitDiffStat(path string) string {
	cmd := platform.Command(context.Background(), "git", "diff", "--stat", "HEAD", "--", path)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 0 {
		return strings.TrimSpace(lines[len(lines)-1])
	}
	return ""
}

func init() {
	sessionsDiffCmd.Flags().Bool("stat", false, "Show line-change statistics per file")
	sessionsDiffCmd.Flags().Bool("json", false, "Emit JSON output")
}
