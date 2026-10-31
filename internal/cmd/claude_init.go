// Fork patch: `claude-init` installs ONLY the `/rush` slash-command and
// strips any legacy crush-claude-init block from CLAUDE.md. The per-model
// slash-commands, per-model sub-agents and model-registry / agent-clause /
// sentinel code have been extracted to a separate repo (`cc-arch-hands`);
// use `cah install` from there for the rest.
//
// On invocation we still STRIP any pre-existing crush-claude-init block
// from CLAUDE.md (any version) so users upgrading from an older rush
// get a clean workspace. If the strip leaves CLAUDE.md empty, the file
// is removed (mirrors `claude-del`'s behaviour).
package cmd

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/PHPCraftdream/rush/internal/fsext"
	"github.com/spf13/cobra"
)

// claudeWrushCommandTemplate is the canonical /wrush slash-command body minus
// the sentinel marker. /wrush is /rush with one added hard rule: every
// delegation runs inside a dedicated git worktree, never the primary
// checkout — see the template's own "Mandatory: an isolated worktree, every
// time" section. Same rationale as the other two templates: kept in a
// sibling .md file, reuses the shared claudeSlashCommandSentinel (ownership
// checks are scoped per filename, wrush.md here).
//
//go:embed claude_wrush_command.md
var claudeWrushCommandTemplate string

// claudeWcrushCommandTemplate is the canonical /wcrush slash-command body
// minus the sentinel marker. /wcrush is /wrush with one added hard rule:
// the sub-agent never runs the tests — phase 1 writes and statically
// verifies, phase 2 runs the suites on the orchestrator's explicit
// permission in the same session. Everything else is inherited by pointing
// at wrush.md in the same directory. Same rationale as the other
// templates: kept in a sibling .md file, reuses the shared
// claudeSlashCommandSentinel (ownership checks are scoped per filename,
// wcrush.md here).
//
//go:embed claude_wcrush_command.md
var claudeWcrushCommandTemplate string

// claudeInitBlockPattern matches any version of the legacy inserted block —
// `<!-- crush-claude-init:v1 --> … <!-- /crush-claude-init -->`.
// Kept around because `claude-init` still runs it on existing CLAUDE.md
// files to GC the block when an old install is upgraded.
var claudeInitBlockPattern = regexp.MustCompile(`(?s)<!-- crush-claude-init:v\d+ -->.*?<!-- /crush-claude-init -->\s*`)

const (
	claudeMdFile               = "CLAUDE.md"
	claudeSlashCommandPath     = ".claude/commands/rush.md"
	claudeSlashCommandSentinel = "<!-- rush-slash-command:v1 -->"
	claudeCommandsDir          = ".claude/commands"
	claudeGlobalCommandsDir    = ".claude/commands" // relative to $HOME
)

// resolveCommandsDir returns the directory where slash commands should be
// written. When global is true it returns ~/.claude/commands; otherwise it
// returns <cwd>/.claude/commands.
func resolveCommandsDir(cwd string, global bool) (string, error) {
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot determine home directory: %w", err)
		}
		return filepath.Join(home, claudeGlobalCommandsDir), nil
	}
	return filepath.Join(cwd, claudeCommandsDir), nil
}

var claudeInitCmd = &cobra.Command{
	Use:   "claude-init",
	Args:  cobra.NoArgs,
	Short: "Install the /rush, /rush-fallback, /wrush and /wcrush slash-commands and strip legacy CLAUDE.md block",
	Long: `Set up rush's ` + "`/rush`" + `, ` + "`/rush-fallback`" + `, ` + "`/wrush`" + ` and ` + "`/wcrush`" + ` slash-commands in
Claude Code.

The slash-command files are written to ` + "`~/.claude/commands/`" + ` by default
(the GLOBAL scope, available in every project). Use --local (or --cwd,
which implies it) to scope them to the current project's
` + "`.claude/commands/`" + ` instead.

Concretely:

  1. Write ` + "`.claude/commands/rush.md`" + ` — the ` + "`/rush`" + ` delegation command.
  2. Write ` + "`.claude/commands/rush-fallback.md`" + ` — the ` + "`/rush-fallback`" + `
     hard-limit reroute command.
  3. Write ` + "`.claude/commands/wrush.md`" + ` — ` + "`/wrush`" + `, identical to
     ` + "`/rush`" + ` except it always runs the delegation inside a dedicated
     git worktree instead of the primary checkout, cleaning up (or
     merging back) once the work is verified.
  4. Write ` + "`.claude/commands/wcrush.md`" + ` — ` + "`/wcrush`" + `, identical to
     ` + "`/wrush`" + ` except the sub-agent never runs the tests: phase 1
     writes and statically verifies, phase 2 runs the suites on the
     orchestrator's explicit permission.

     Each file is skipped (with a warning) if it exists without our
     sentinel — we never overwrite a file we don't own.

  5. In local mode only: strip any pre-existing crush-claude-init block
     from ` + "`CLAUDE.md`" + ` (any version v1..vN). If the file becomes empty
     it is removed.

` + "`claude-init`" + ` no longer writes anything into ` + "`CLAUDE.md`" + `. Delegation is
explicit-only — invoke ` + "`/rush <task>`" + `, ` + "`/wrush <task>`" + ` or ` + "`/wcrush <task>`" + ` when you want it.
The installed slash command is Markdown; its embedded source may be Markdown
or structured YAML with Claude-specific blocks.

For per-model commands, agents and skills, use ` + "`cah install`" + ` from the
cc-arch-hands repo.`,
	Example: `
# Install / refresh the /rush, /rush-fallback, /wrush and /wcrush slash-commands globally — the default
rush claude-init

# Install into the current project instead
rush claude-init --local

# Scope to another project (implies --local)
rush claude-init --cwd /path/to/project
`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateClaudeInitSources(skillSourceFiles); err != nil {
			return err
		}
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
			// Strip any legacy crush-claude-init block from CLAUDE.md (local only).
			if err := stripLegacyBlockFromCLAUDEMd(cwd); err != nil {
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

		// Install / refresh the /rush slash-command.
		if err := writeSlashCommandToDir(cmdDir); err != nil {
			return fmt.Errorf("slash command: %w", err)
		}
		// Install / refresh the /rush-fallback slash-command.
		if err := writeFallbackCommandToDir(cmdDir); err != nil {
			return fmt.Errorf("fallback slash command: %w", err)
		}
		// Install / refresh the /wrush slash-command.
		if err := writeWrushCommandToDir(cmdDir); err != nil {
			return fmt.Errorf("wrush slash command: %w", err)
		}
		// Install / refresh the /wcrush slash-command.
		if err := writeWcrushCommandToDir(cmdDir); err != nil {
			return fmt.Errorf("wcrush slash command: %w", err)
		}
		return nil
	},
}

// stripLegacyBlockFromCLAUDEMd removes every crush-claude-init block from
// the workspace's CLAUDE.md. If the file becomes empty, it is deleted.
// Missing file is a no-op.
func stripLegacyBlockFromCLAUDEMd(cwd string) error {
	path := filepath.Join(cwd, claudeMdFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read %s: %w", path, err)
	}
	body := string(data)
	matches := claudeInitBlockPattern.FindAllString(body, -1)
	if len(matches) == 0 {
		return nil
	}
	stripped := strings.TrimRight(claudeInitBlockPattern.ReplaceAllString(body, ""), " \t\n")
	if stripped == "" {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("failed to remove now-empty %s: %w", path, err)
		}
		fmt.Fprintf(os.Stderr, "stripped %d legacy crush-claude-init block(s) and removed now-empty %s\n", len(matches), path)
		return nil
	}
	if err := fsext.AtomicWriteFile(path, []byte(stripped+"\n"), 0o644); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "stripped %d legacy crush-claude-init block(s) from %s\n", len(matches), path)
	return nil
}

func writeSlashCommand(cwd string) error {
	return writeSlashCommandToDir(filepath.Join(cwd, claudeCommandsDir))
}

func writeSlashCommandToDir(dir string) error {
	path := filepath.Join(dir, "rush.md")
	if data, err := os.ReadFile(path); err == nil {
		if !strings.Contains(string(data), claudeSlashCommandSentinel) {
			fmt.Fprintf(os.Stderr, "warning: %s exists but does not contain our sentinel — skipping (someone else owns that file)\n", path)
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	content, err := claudeSlashCommandContent()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
	return nil
}

// claudeSlashCommandContent returns the body of `.claude/commands/rush.md`.
// Sentinel marker is prepended so claude-del can recognise files we own
// without parsing content. Self-contained: holds the full delegation
// guidance inline, because there is no longer a long block in CLAUDE.md
// to refer the operator to. Triggered ONLY by an explicit `/rush <task>`
// from the operator.
func claudeSlashCommandContent() (string, error) {
	description, body, err := loadSkillSource("claude_slash_command", skillTargetClaude)
	if err != nil {
		return "", fmt.Errorf("load /rush source: %w", err)
	}
	return renderFrontMatterMD(claudeSlashCommandSentinel, description, body, "$ARGUMENTS") + "\n", nil
}

func writeFallbackCommandToDir(dir string) error {
	path := filepath.Join(dir, "rush-fallback.md")
	if data, err := os.ReadFile(path); err == nil {
		if !strings.Contains(string(data), claudeSlashCommandSentinel) {
			fmt.Fprintf(os.Stderr, "warning: %s exists but does not contain our sentinel — skipping (someone else owns that file)\n", path)
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	content, err := claudeFallbackCommandContent()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
	return nil
}

// claudeFallbackCommandContent returns the body of
// `.claude/commands/rush-fallback.md`. Sentinel marker is prepended so
// claude-del can recognise files we own without parsing content. Triggered
// ONLY by an explicit `/rush-fallback <agent>` from the operator, right
// after a peak-hours refusal.
func claudeFallbackCommandContent() (string, error) {
	description, body, err := loadSkillSource("claude_crush_fallback_command", skillTargetClaude)
	if err != nil {
		return "", fmt.Errorf("load /rush-fallback source: %w", err)
	}
	return renderFrontMatterMD(claudeSlashCommandSentinel, description, body, "$ARGUMENTS") + "\n", nil
}

func writeWrushCommandToDir(dir string) error {
	path := filepath.Join(dir, "wrush.md")
	if data, err := os.ReadFile(path); err == nil {
		if !strings.Contains(string(data), claudeSlashCommandSentinel) {
			fmt.Fprintf(os.Stderr, "warning: %s exists but does not contain our sentinel — skipping (someone else owns that file)\n", path)
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err := os.WriteFile(path, []byte(claudeWrushCommandContent()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
	return nil
}

// claudeWrushCommandContent returns the body of `.claude/commands/wrush.md`.
// Sentinel marker is prepended so claude-del can recognise files we own
// without parsing content. Triggered ONLY by an explicit `/wrush <task>`
// from the operator — same trigger discipline as /rush, plus the mandatory
// git-worktree isolation described in the template itself.
func claudeWrushCommandContent() string {
	return claudeSlashCommandSentinel + "\n" + claudeWrushCommandTemplate
}

func writeWcrushCommandToDir(dir string) error {
	path := filepath.Join(dir, "wcrush.md")
	if data, err := os.ReadFile(path); err == nil {
		if !strings.Contains(string(data), claudeSlashCommandSentinel) {
			fmt.Fprintf(os.Stderr, "warning: %s exists but does not contain our sentinel — skipping (someone else owns that file)\n", path)
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	if err := os.WriteFile(path, []byte(claudeWcrushCommandContent()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
	return nil
}

// claudeWcrushCommandContent returns the body of `.claude/commands/wcrush.md`.
// Sentinel marker is prepended so claude-del can recognise files we own
// without parsing content. Triggered ONLY by an explicit `/wcrush <task>`
// from the operator — same trigger discipline as /wrush, plus the
// mandatory two-phase (write now, verify later) contract described in
// the template itself.
func claudeWcrushCommandContent() string {
	return claudeSlashCommandSentinel + "\n" + claudeWcrushCommandTemplate
}

func init() {
	claudeInitCmd.Flags().Bool("global", false, "Install into ~/.claude/commands/ (available in every project). Default when neither --global nor --local is given.")
	claudeInitCmd.Flags().Bool("local", false, "Install into the current project's .claude/commands/ instead of ~/.claude/commands/.")
	rootCmd.AddCommand(claudeInitCmd)
}
