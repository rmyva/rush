package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/spf13/cobra"
)

var claudeDelCmd = &cobra.Command{
	Use:   "claude-del",
	Args:  cobra.NoArgs,
	Short: "Remove the /rush, /rush-fallback, /wrush and /wcrush slash-commands and strip legacy CLAUDE.md block",
	Long: `Undo ` + "`rush claude-init`" + `: remove the /rush, /rush-fallback, /wrush and /wcrush
slash-commands and strip any crush-claude-init block from CLAUDE.md.

Only files that carry our sentinel are removed — foreign files with the
same name are left alone with a warning.

This also removes legacy crush.md and crush-fallback.md files from a
pre-rename install (if they contain the legacy sentinel). /wrush and
/wcrush have no pre-rename legacy name, since they did not exist before
the rename.

Default is --global (~/.claude/commands/). Use --local (or --cwd, which
implies it) to target the current project's .claude/commands/ instead.
--global and --local/--cwd are mutually exclusive.

Idempotent: running this twice is a no-op the second time.

For per-model commands, agents and skills, use ` + "`cah uninstall`" + ` from the
cc-arch-hands repo.`,
	Example: `
# Remove globally (from ~/.claude/commands/) — the default
rush claude-del

# Remove from the current project instead
rush claude-del --local

# Scope to another project (implies --local)
rush claude-del --cwd /path/to/project
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		global, _ := cmd.Flags().GetBool("global")
		local, _ := cmd.Flags().GetBool("local")
		hasCwd := cmd.Flags().Changed("cwd")
		localMode := local || hasCwd

		if global && localMode {
			return fmt.Errorf("--global and --local/--cwd are mutually exclusive")
		}

		var cmdDir string
		if localMode {
			cwd, err := ResolveCwd(cmd)
			if err != nil {
				return err
			}
			// Strip CLAUDE.md blocks only in local mode.
			claudeMdPath := filepath.Join(cwd, claudeMdFile)
			if _, err := stripClaudeMdBlocks(claudeMdPath); err != nil {
				return err
			}
			cmdDir = filepath.Join(cwd, claudeCommandsDir)
		} else {
			// Default (no flags), or explicit --global: global mode.
			var err error
			cmdDir, err = resolveCommandsDir("", true)
			if err != nil {
				return err
			}
		}

		if err := removeSlashCommandFromDir(cmdDir); err != nil {
			return err
		}
		if err := removeFallbackCommandFromDir(cmdDir); err != nil {
			return err
		}
		if err := removeWrushCommandFromDir(cmdDir); err != nil {
			return err
		}
		return removeWcrushCommandFromDir(cmdDir)
	},
}

// runClaudeDel is kept for tests that call it directly (local mode only).
func runClaudeDel(cwd string) error {
	claudeMdPath := filepath.Join(cwd, claudeMdFile)
	if _, err := stripClaudeMdBlocks(claudeMdPath); err != nil {
		return err
	}
	cmdDir := filepath.Join(cwd, claudeCommandsDir)
	if err := removeSlashCommandFromDir(cmdDir); err != nil {
		return err
	}
	if err := removeFallbackCommandFromDir(cmdDir); err != nil {
		return err
	}
	if err := removeWrushCommandFromDir(cmdDir); err != nil {
		return err
	}
	return removeWcrushCommandFromDir(cmdDir)
}

const (
	legacyClaudeSlashCommandSentinel = "<!-- crush-slash-command:v1 -->"
)

// containsAnySentinel returns true if the data contains either the new
// rush-slash-command sentinel or the legacy crush-slash-command sentinel.
func containsAnySentinel(data string) bool {
	return strings.Contains(data, claudeSlashCommandSentinel) ||
		strings.Contains(data, legacyClaudeSlashCommandSentinel)
}

func removeSlashCommand(cwd string) error {
	return removeSlashCommandFromDir(filepath.Join(cwd, claudeCommandsDir))
}

// removeSlashCommandFromDir removes both rush.md and legacy crush.md files
// from the directory if they contain our sentinel.
func removeSlashCommandFromDir(dir string) error {
	for _, name := range []string{"rush.md", "crush.md"} {
		if err := removeFileIfOurs(dir, name); err != nil {
			return err
		}
	}
	return nil
}

// removeFallbackCommandFromDir removes both rush-fallback.md and legacy
// crush-fallback.md files from the directory if they contain our sentinel.
func removeFallbackCommandFromDir(dir string) error {
	for _, name := range []string{"rush-fallback.md", "crush-fallback.md"} {
		if err := removeFileIfOurs(dir, name); err != nil {
			return err
		}
	}
	return nil
}

// removeWrushCommandFromDir removes wrush.md from the directory if it
// contains our sentinel. No legacy pre-rename name: /wrush postdates the
// crush->rush rename.
func removeWrushCommandFromDir(dir string) error {
	return removeFileIfOurs(dir, "wrush.md")
}

// removeWcrushCommandFromDir removes wcrush.md from the directory if it
// contains our sentinel. No legacy pre-rename name: /wcrush postdates the
// crush->rush rename.
func removeWcrushCommandFromDir(dir string) error {
	return removeFileIfOurs(dir, "wcrush.md")
}

// removeFileIfOurs deletes the file at dir/name if it contains either the
// new rush-slash-command sentinel or the legacy crush-slash-command sentinel.
// If the file exists but doesn't contain either sentinel, it's left alone
// with a warning. Missing file is a no-op.
func removeFileIfOurs(dir, name string) error {
	path := filepath.Join(dir, name)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if !containsAnySentinel(string(data)) {
		fmt.Fprintf(os.Stderr, "refusing to delete %s — does not look like ours (missing sentinel)\n", path)
		return nil
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("failed to remove %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "removed %s\n", path)
	return nil
}

func stripClaudeMdBlocks(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "no %s found — nothing to do\n", claudeMdFile)
			return 0, nil
		}
		return 0, fmt.Errorf("failed to read %s: %w", path, err)
	}

	body := string(data)
	matches := claudeInitBlockPattern.FindAllString(body, -1)
	if len(matches) == 0 {
		fmt.Fprintf(os.Stderr, "no crush-claude-init block found in %s\n", path)
		return 0, nil
	}

	cleaned := claudeInitBlockPattern.ReplaceAllString(body, "")
	cleaned = strings.TrimRight(cleaned, " \t\n")

	if cleaned == "" {
		if err := os.Remove(path); err != nil {
			return 0, fmt.Errorf("failed to remove %s: %w", path, err)
		}
		fmt.Fprintf(os.Stderr, "removed empty %s\n", claudeMdFile)
		return len(matches), nil
	}

	if err := fsext.AtomicWriteFile(path, []byte(cleaned+"\n"), 0o644); err != nil {
		return 0, fmt.Errorf("failed to write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "stripped %d crush-claude-init block(s) from %s\n", len(matches), claudeMdFile)
	return len(matches), nil
}

func init() {
	claudeDelCmd.Flags().Bool("global", false, "Remove from ~/.claude/commands/. Default when neither --global nor --local is given.")
	claudeDelCmd.Flags().Bool("local", false, "Remove from the current project's .claude/commands/ instead of ~/.claude/commands/.")
	rootCmd.AddCommand(claudeDelCmd)
}
