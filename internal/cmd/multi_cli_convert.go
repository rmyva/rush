// Fork addition: shared conversion + filesystem helpers used by the
// `<tool>-init`/`<tool>-del` command family (codex-init/codex-del,
// gemini-init/gemini-del, grok-init/grok-del, qwen-init/qwen-del,
// `opencode-init`/`opencode-del` and `omp-init`/`omp-del`. Each CLI has
// its own on-disk convention for
// "custom command"/"skill" — Claude Code uses `.claude/commands/*.md`
// front-matter, Codex/Grok use Skills-style `<name>/SKILL.md`, Gemini uses
// TOML, Qwen uses a different front-matter placeholder. Rather than
// duplicate source-of-truth prose, we load one embedded Markdown or YAML
// source per command and assemble it for each target CLI here.
package cmd

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed *.md *.yaml
var skillSourceFiles embed.FS

type skillTarget string

const (
	skillTargetCommon   skillTarget = "common"
	skillTargetClaude   skillTarget = "claude"
	skillTargetCodex    skillTarget = "codex"
	skillTargetGemini   skillTarget = "gemini"
	skillTargetGrok     skillTarget = "grok"
	skillTargetQwen     skillTarget = "qwen"
	skillTargetOpenCode skillTarget = "opencode"
	skillTargetOmp      skillTarget = "omp"
)

type structuredSkillSource struct {
	Description string             `yaml:"description"`
	Blocks      []structuredBlocks `yaml:"blocks"`
}

type structuredBlocks struct {
	Common   string `yaml:"common"`
	Claude   string `yaml:"claude"`
	Codex    string `yaml:"codex"`
	Gemini   string `yaml:"gemini"`
	Grok     string `yaml:"grok"`
	Qwen     string `yaml:"qwen"`
	Opencode string `yaml:"opencode"`
	Omp      string `yaml:"omp"`
}

// loadSkillSource selects a source by extension and assembles its target
// variant. A source may be either a legacy Markdown file or structured YAML.
func loadSkillSource(name string, target skillTarget) (description, body string, err error) {
	return loadSkillSourceFrom(skillSourceFiles, name, target)
}

func loadSkillSourceFrom(sourceFS fs.FS, name string, target skillTarget) (description, body string, err error) {
	md, mdErr := fs.ReadFile(sourceFS, name+".md")
	yamlSource, yamlErr := fs.ReadFile(sourceFS, name+".yaml")
	if mdErr != nil && !errorsIsNotExist(mdErr) {
		return "", "", fmt.Errorf("read %s.md source: %w", name, mdErr)
	}
	if yamlErr != nil && !errorsIsNotExist(yamlErr) {
		return "", "", fmt.Errorf("read %s.yaml source: %w", name, yamlErr)
	}
	return parseSkillSource(name, string(md), mdErr == nil, string(yamlSource), yamlErr == nil, target)
}

func validateClaudeInitSources(sourceFS fs.FS) error {
	if _, _, err := loadSkillSourceFrom(sourceFS, "claude_slash_command", skillTargetClaude); err != nil {
		return fmt.Errorf("rush slash-command source: %w", err)
	}
	if _, _, err := loadSkillSourceFrom(sourceFS, "claude_crush_fallback_command", skillTargetClaude); err != nil {
		return fmt.Errorf("rush-fallback slash-command source: %w", err)
	}
	return nil
}

func parseSkillSource(name, markdown string, hasMarkdown bool, structured string, hasStructured bool, target skillTarget) (string, string, error) {
	if hasMarkdown && hasStructured {
		return "", "", fmt.Errorf("skill %q has both .md and .yaml sources", name)
	}
	if hasMarkdown {
		return parseSlashCommandSource(markdown)
	}
	if hasStructured {
		return parseStructuredSkillSource(structured, target)
	}
	return "", "", fmt.Errorf("skill %q has no embedded .md or .yaml source", name)
}

func errorsIsNotExist(err error) bool {
	return err != nil && errors.Is(err, fs.ErrNotExist)
}

func parseStructuredSkillSource(raw string, target skillTarget) (string, string, error) {
	var source structuredSkillSource
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&source); err != nil {
		return "", "", fmt.Errorf("parse structured skill source: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return "", "", fmt.Errorf("parse structured skill source: multiple YAML documents are not allowed")
		}
		return "", "", fmt.Errorf("parse structured skill source: %w", err)
	}
	if strings.TrimSpace(source.Description) == "" {
		return "", "", fmt.Errorf("structured skill source requires a description")
	}
	if len(source.Blocks) == 0 {
		return "", "", fmt.Errorf("structured skill source requires at least one block")
	}
	parts := make([]string, 0, len(source.Blocks)*2)
	for i, block := range source.Blocks {
		allFields := []string{block.Common, block.Claude, block.Codex, block.Gemini, block.Grok, block.Qwen, block.Opencode, block.Omp}
		present := false
		for _, part := range allFields {
			if strings.TrimSpace(part) != "" {
				present = true
			}
		}
		if !present {
			return "", "", fmt.Errorf("structured skill source block %d is empty", i+1)
		}
		for _, part := range []string{block.Common, block.forTarget(target)} {
			if strings.TrimSpace(part) != "" {
				parts = append(parts, strings.TrimSpace(part))
			}
		}
	}
	body := strings.Join(parts, "\n\n")
	if strings.TrimSpace(body) == "" {
		return "", "", fmt.Errorf("structured skill source has empty output for target %q", target)
	}
	if regexp.MustCompile(`\{\{RUSH_[A-Z0-9_]+\}\}`).MatchString(body) {
		return "", "", fmt.Errorf("structured skill source contains an unresolved build marker")
	}
	return source.Description, body, nil
}

func (b structuredBlocks) forTarget(target skillTarget) string {
	switch target {
	case skillTargetClaude:
		return b.Claude
	case skillTargetCodex:
		return b.Codex
	case skillTargetGemini:
		return b.Gemini
	case skillTargetGrok:
		return b.Grok
	case skillTargetQwen:
		return b.Qwen
	case skillTargetOpenCode:
		return b.Opencode
	case skillTargetOmp:
		return b.Omp
	default:
		return ""
	}
}

// parseSlashCommandSource splits a canonical slash-command .md source
// (YAML front-matter with a single `description:` field, then a Markdown
// body) into its description and body parts. The format is trivial and
// fixed by convention (see claude_slash_command.md / claude_rush_fallback_command.md),
// so this is deliberately plain string parsing rather than a YAML library.
//
// Expected shape:
//
//	---
//	description: <text>
//	---
//
//	<body...>
func parseSlashCommandSource(raw string) (description, body string, err error) {
	lines := strings.Split(raw, "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", fmt.Errorf("parseSlashCommandSource: expected source to start with a %q front-matter delimiter", "---")
	}

	const descPrefix = "description: "
	if !strings.HasPrefix(lines[1], descPrefix) {
		return "", "", fmt.Errorf("parseSlashCommandSource: expected line 2 to start with %q, got %q", descPrefix, lines[1])
	}
	description = strings.TrimPrefix(lines[1], descPrefix)

	if len(lines) < 3 || strings.TrimSpace(lines[2]) != "---" {
		return "", "", fmt.Errorf("parseSlashCommandSource: expected a closing %q front-matter delimiter on line 3, got %q", "---", lines[2])
	}

	rest := strings.Join(lines[3:], "\n")
	body = strings.TrimLeft(rest, "\n")
	return description, body, nil
}

// renderFrontMatterMD renders a front-matter-style .md file: a sentinel
// comment, a `description:` front-matter block, then the body with
// `$ARGUMENTS` rewritten to the given placeholder. Used by tools (e.g.
// qwen-init) whose custom-command convention still uses `description:`
// front-matter but a different in-body argument placeholder than Claude
// Code's `$ARGUMENTS`.
func renderFrontMatterMD(sentinelComment, description, body, placeholder string) string {
	return sentinelComment + "\n---\ndescription: " + description + "\n---\n\n" + strings.ReplaceAll(body, "$ARGUMENTS", placeholder)
}

// toGeminiTOML converts a description/body pair into Gemini CLI's custom
// command TOML format. Returns an error if body contains a literal `"""`,
// since that would break the TOML triple-quoted string we emit it into —
// better to fail loudly here than silently corrupt the generated file.
func toGeminiTOML(description, body string) (string, error) {
	if strings.Contains(body, `"""`) {
		return "", fmt.Errorf("toGeminiTOML: body contains a literal %q, which would break the TOML triple-quoted prompt string", `"""`)
	}
	escapedDesc := strings.ReplaceAll(description, `"`, `\"`)
	prompt := strings.ReplaceAll(body, "$ARGUMENTS", "{{args}}")
	return "# rush-slash-command:v1\n" +
		"description = \"" + escapedDesc + "\"\n" +
		"prompt = \"\"\"\n" +
		prompt +
		"\"\"\"\n", nil
}

// toSkillMD converts a description/body pair into Codex/Grok Skills-format
// SKILL.md content. Both tools share the identical Skills convention:
// `<skillsDir>/<name>/SKILL.md` with `name:`/`description:` front-matter.
// The opening delimiter must be the file's first bytes for strict parsers;
// the ownership sentinel therefore lives immediately after the closing
// delimiter, where it remains parser-safe and discoverable by write/remove.
func toSkillMD(name, description, body string) string {
	return "---\n" +
		"name: " + name + "\n" +
		"description: " + description + "\n" +
		"---\n" +
		claudeSlashCommandSentinel + "\n\n" +
		"Any text you type after invoking this skill is the task — treat it exactly as `$ARGUMENTS` below would have been substituted.\n\n" +
		body
}

func replaceCodexGuidance(body, claudeGuidance, codexGuidance string) (string, error) {
	if !strings.Contains(body, claudeGuidance) {
		return "", fmt.Errorf("codex variant is missing expected Claude-specific guidance %q", claudeGuidance)
	}
	return strings.Replace(body, claudeGuidance, codexGuidance, 1), nil
}

const (
	claudeWrushLaunchGuidance = "Launch `rush run` with cwd inside the worktree (`cd` in the same\n" +
		"   Bash call) — every edit, git op, and test the sub-agent runs stays\n" +
		"   inside that tree. Redirect `.rush/stdin/<task>.{out,err}` to the\n" +
		"   PRIMARY checkout so results survive the eventual worktree removal."
	codexWrushLaunchGuidance = "Launch `rush run` with Codex's `exec_command`, setting its workdir\n" +
		"   to the worktree — every edit, git op, and test the sub-agent runs\n" +
		"   stays inside that tree. If `exec_command` returns a session handle,\n" +
		"   retain it. Repeatedly call `write_stdin` with that handle and empty\n" +
		"   input, blocking each time, until `rush run` exits; then read the\n" +
		"   output files and verify the result. Redirect\n" +
		"   `.rush/stdin/<task>.{out,err}` to the\n" +
		"   PRIMARY checkout so results survive the eventual worktree removal."
	claudeWcrushBackgroundGuidance = "**OOM discipline belongs to phase 2** — the other half of the same\n" +
		"  bargain: `-parallel 2` for heavy packages, never two heavy runs at\n" +
		"  once, long runs backgrounded via the Bash tool's\n" +
		"  `run_in_background: true` parameter and never with a trailing `&`."
	codexWcrushBackgroundGuidance = "**OOM discipline belongs to phase 2** — the other half of the same\n" +
		"  bargain: `-parallel 2` for heavy packages, never two heavy runs at\n" +
		"  once. Run long phase-2 tests with Codex's `exec_command`. If it\n" +
		"  returns a session handle, retain it and repeatedly call blocking\n" +
		"  `write_stdin` with empty input until the test process exits. Then read\n" +
		"  the output and verify the result."
	claudeWcrushResumeBlock = "```\n" +
		"rush run --role smart --session <same id> \"<permission and what to run>\"\n" +
		"```"
	opencodeWrushLaunchGuidance = "Launch `rush run` with the bash tool, `workdir` set to the worktree —\n" +
		"   every edit, git op, and test the sub-agent runs stays inside that\n" +
		"   tree. Run in the FOREGROUND with a large `timeout` in MILLISECONDS\n" +
		"   (e.g. 36000000; the 120000 default kills a `rush run` minutes in),\n" +
		"   one bare command, never `&`. Redirect\n" +
		"   `.rush/stdin/<task>.{out,err}` to the PRIMARY checkout's ABSOLUTE\n" +
		"   path (a relative path lands inside the worktree, because `workdir`\n" +
		"   is the worktree) so results survive the eventual worktree removal."
	ompWrushLaunchGuidance = "Launch `rush run` with the bash tool's `\"async\": true`, `\"timeout\": 0`,\n" +
		"   `\"pty\": false` and `\"cwd\"` set to the worktree — every edit, git op,\n" +
		"   and test the sub-agent runs stays inside that tree. Retain the\n" +
		"   returned job id; completion auto-wakes this conversation. Never\n" +
		"   `&`, `nohup`, or `disown`. Redirect\n" +
		"   `.rush/stdin/<task>.{out,err}` to the PRIMARY checkout's ABSOLUTE\n" +
		"   path (a relative path lands inside the worktree, because `cwd`\n" +
		"   is the worktree) so results survive the eventual worktree removal."
	opencodeWcrushBackgroundGuidance = "**OOM discipline belongs to phase 2** — the other half of the same\n" +
		"  bargain: `-parallel 2` for heavy packages, never two heavy runs at\n" +
		"  once. Run long phase-2 tests in the FOREGROUND with a large\n" +
		"  `timeout` in MILLISECONDS (the 120000 default kills a long test\n" +
		"  run); the call returning is the completion signal. Never append\n" +
		"  `&` — it false-completes instantly and the child is untracked."
	ompWcrushBackgroundGuidance = "**OOM discipline belongs to phase 2** — the other half of the same\n" +
		"  bargain: `-parallel 2` for heavy packages, never two heavy runs at\n" +
		"  once. Run long phase-2 tests with the bash tool's `\"async\": true` and\n" +
		"  `\"timeout\": 0` (without `timeout: 0` the 300 s deadline applies even\n" +
		"  in background); the returned job id auto-wakes this conversation on\n" +
		"  completion — in print mode, keep calling `wait` until the tests\n" +
		"  finish. Never `&`, `nohup`, or `disown`."
)

// toCodexWrushSkillMD converts the canonical Claude /wrush body to Codex's
// nested Skills layout. Claude places rush.md and wrush.md in one directory,
// while Codex places them in sibling directories as rush/SKILL.md and
// wrush/SKILL.md, so every reference to the inherited base instructions must
// point one directory up.
func toCodexWrushSkillMD(description, body string) (string, error) {
	const (
		claudeSameDirReference = "`rush.md` file in this same directory"
		codexSiblingReference  = "sibling `../rush/SKILL.md` file"
	)
	if !strings.Contains(body, claudeSameDirReference) {
		return "", fmt.Errorf("toCodexWrushSkillMD: expected canonical same-directory rush.md reference")
	}
	var err error
	body, err = replaceCodexGuidance(body, claudeWrushLaunchGuidance, codexWrushLaunchGuidance)
	if err != nil {
		return "", err
	}

	body = strings.ReplaceAll(body, claudeSameDirReference, codexSiblingReference)
	body = regexp.MustCompile(`\brush\.md\b`).ReplaceAllString(body, "../rush/SKILL.md")
	return toSkillMD("wrush", description, body), nil
}

// toCodexWcrushSkillMD converts the canonical Claude /wcrush body to Codex's
// nested Skills layout. Claude places rush.md, wrush.md and wcrush.md in one
// directory, while Codex places them in sibling directories as rush/SKILL.md,
// wrush/SKILL.md and wcrush/SKILL.md, so every reference to the inherited
// base instructions must point one directory up. The canonical
// same-directory wrush.md reference may be line-wrapped in the Markdown
// source, so validation runs on whitespace-collapsed text and the readable
// phrase rewrite tolerates a line break inside it; the word-boundary
// rewrite below catches any remaining occurrences regardless of wrapping.
func toCodexWcrushSkillMD(description, body string) (string, error) {
	const (
		claudeSameDirReference = "`wrush.md` file in this same directory"
		codexSiblingReference  = "sibling `../wrush/SKILL.md` file"
	)
	collapsed := strings.Join(strings.Fields(body), " ")
	if !strings.Contains(collapsed, claudeSameDirReference) {
		return "", fmt.Errorf("toCodexWcrushSkillMD: expected canonical same-directory wrush.md reference")
	}
	var err error
	body, err = replaceCodexGuidance(body, claudeWcrushBackgroundGuidance, codexWcrushBackgroundGuidance)
	if err != nil {
		return "", err
	}

	wrappedPhrase := regexp.MustCompile(regexp.QuoteMeta("`wrush.md` file in this") + `\s+same directory`)
	body = wrappedPhrase.ReplaceAllString(body, codexSiblingReference)
	body = regexp.MustCompile(`\bwrush\.md\b`).ReplaceAllString(body, "../wrush/SKILL.md")
	return toSkillMD("wcrush", description, body), nil
}

// Guidance swapped per Markdown-command target: [from, to] pairs keyed by
// target.
var wrushLaunchGuidanceByTarget = map[skillTarget][2]string{
	skillTargetOpenCode: {claudeWrushLaunchGuidance, opencodeWrushLaunchGuidance},
	skillTargetOmp:      {claudeWrushLaunchGuidance, ompWrushLaunchGuidance},
}

var wcrushBackgroundGuidanceByTarget = map[skillTarget][2]string{
	skillTargetOpenCode: {claudeWcrushBackgroundGuidance, opencodeWcrushBackgroundGuidance},
	skillTargetOmp:      {claudeWcrushBackgroundGuidance, ompWcrushBackgroundGuidance},
}

// toMarkdownWrushCommandMD converts the canonical Claude /wrush body for a
// Markdown-command target (OpenCode, omp). Only the harness-specific launch
// guidance is swapped — all four files live in one commands directory, so
// the same-directory rush.md reference stays as written.
func toMarkdownWrushCommandMD(target skillTarget, description, body string) (string, error) {
	repl, ok := wrushLaunchGuidanceByTarget[target]
	if !ok {
		return "", fmt.Errorf("toMarkdownWrushCommandMD: unsupported target %q", target)
	}
	swapped, err := replaceCodexGuidance(body, repl[0], repl[1])
	if err != nil {
		return "", err
	}
	return renderFrontMatterMD(claudeSlashCommandSentinel, description, swapped, "$ARGUMENTS") + "\n", nil
}

// toMarkdownWcrushCommandMD is the /wcrush counterpart of
// toMarkdownWrushCommandMD, swapping the harness-specific background
// guidance instead.
func toMarkdownWcrushCommandMD(target skillTarget, description, body string) (string, error) {
	repl, ok := wcrushBackgroundGuidanceByTarget[target]
	if !ok {
		return "", fmt.Errorf("toMarkdownWcrushCommandMD: unsupported target %q", target)
	}
	swapped, err := replaceCodexGuidance(body, repl[0], repl[1])
	if err != nil {
		return "", err
	}
	return renderFrontMatterMD(claudeSlashCommandSentinel, description, swapped, "$ARGUMENTS") + "\n", nil
}

// writeSentinelledFile writes content to path, refusing to overwrite a file
// that already exists but doesn't carry our sentinel substring (someone
// else's file with the same name). Creates parent directories as needed.
func writeSentinelledFile(path, sentinelSubstring, content string) error {
	if data, err := os.ReadFile(path); err == nil {
		if !strings.Contains(string(data), sentinelSubstring) {
			fmt.Fprintf(os.Stderr, "warning: %s exists but does not contain our sentinel — skipping (someone else owns that file)\n", path)
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", path)
	return nil
}

// removeSentinelledFile removes path, refusing to delete a file that exists
// but doesn't carry our sentinel substring. Missing file is a no-op.
func removeSentinelledFile(path, sentinelSubstring string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	if !strings.Contains(string(data), sentinelSubstring) {
		fmt.Fprintf(os.Stderr, "refusing to delete %s — does not look like ours (missing sentinel)\n", path)
		return nil
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("failed to remove %s: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "removed %s\n", path)
	return nil
}

// writeSentinelledSkillDir writes content to <skillsDir>/<name>/SKILL.md,
// following the same sentinel-ownership rule as writeSentinelledFile.
func writeSentinelledSkillDir(skillsDir, name, sentinelSubstring, content string) error {
	return writeSentinelledFile(filepath.Join(skillsDir, name, "SKILL.md"), sentinelSubstring, content)
}

// removeSentinelledSkillDir removes <skillsDir>/<name>/SKILL.md (following
// the same sentinel-ownership rule as removeSentinelledFile), then attempts
// to remove the now-presumably-empty <skillsDir>/<name>/ directory. The
// directory removal error is deliberately ignored: os.Remove on a
// non-empty directory fails and leaves it untouched, which is exactly the
// safe behaviour we want if something else is still using that directory.
func removeSentinelledSkillDir(skillsDir, name, sentinelSubstring string) error {
	dir := filepath.Join(skillsDir, name)
	if err := removeSentinelledFile(filepath.Join(dir, "SKILL.md"), sentinelSubstring); err != nil {
		return err
	}
	_ = os.Remove(dir)
	return nil
}
