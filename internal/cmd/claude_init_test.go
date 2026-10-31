package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// captureStderr runs f while capturing os.Stderr output.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = w
	defer func() { os.Stderr = old }()

	var buf bytes.Buffer
	done := make(chan struct{})
	go func() {
		_, _ = buf.ReadFrom(r)
		close(done)
	}()

	f()
	_ = w.Close()
	<-done
	return buf.String()
}

func runClaudeInitInDir(t *testing.T, dir string) {
	t.Helper()
	orig, err := os.Getwd()
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := os.Chdir(orig); err != nil {
			t.Logf("restore cwd: %v", err)
		}
	})
	cmd := &cobra.Command{}
	cmd.Flags().StringP("cwd", "c", "", "")
	require.NoError(t, cmd.ParseFlags([]string{"--cwd", dir}))
	require.NoError(t, claudeInitCmd.RunE(cmd, nil))
}

func runClaudeDelInDir(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, runClaudeDel(dir))
}

// ---------------------------------------------------------------------------
// claude-init tests — new behaviour (batch 22):
//   * Never writes a block into CLAUDE.md.
//   * Strips any legacy crush-claude-init block on invocation.
//   * Removes CLAUDE.md if stripping leaves it empty.
//   * Always installs/refreshes the slash-command file.
// ---------------------------------------------------------------------------

func TestClaudeInit_NoCLAUDEMd_StillInstallsSlashCommand(t *testing.T) {
	dir := t.TempDir()

	runClaudeInitInDir(t, dir)

	// CLAUDE.md is NOT created.
	_, err := os.Stat(filepath.Join(dir, claudeMdFile))
	assert.True(t, os.IsNotExist(err), "claude-init must not create CLAUDE.md when it didn't exist")

	// Slash command IS installed.
	slashPath := filepath.Join(dir, ".claude", "commands", "rush.md")
	bts, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "rush run")
	assert.Contains(t, got, "--role smart")
	assert.Contains(t, got, "run_in_background: true")
	assert.NotContains(t, got, "--codex-thread-id")
	assert.NotContains(t, got, "codex queue")
}

func TestClaudeInit_StripsLegacyBlockByReplacingInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)
	alias := filepath.Join(dir, "hardlink")
	content := "# Project\n\n<!-- crush-claude-init:v8 -->\nlegacy\n<!-- /crush-claude-init -->\n\n# Keep\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	if err := os.Link(path, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}

	err := stripLegacyBlockFromCLAUDEMd(dir)
	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	aliased, err := os.ReadFile(alias)
	require.NoError(t, err)
	assert.NotEqual(t, string(got), string(aliased), "replacement should leave the hard link pointing at the old file")
	assert.Contains(t, string(aliased), "legacy")
}

func TestClaudeInit_StripsLegacyBlock_KeepsRestOfFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	content := "# My project\n\n" +
		"<!-- crush-claude-init:v8 -->\nold delegation block\n<!-- /crush-claude-init -->\n\n" +
		"## Other notes\n\nlive content\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeInitInDir(t, dir)
	})

	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(bts)
	assert.NotContains(t, got, "crush-claude-init", "legacy block must be stripped")
	assert.NotContains(t, got, "old delegation block")
	assert.Contains(t, got, "# My project", "user content above the block must survive")
	assert.Contains(t, got, "## Other notes", "user content below the block must survive")
	assert.Contains(t, got, "live content")
	assert.Contains(t, stderr, "stripped 1 legacy crush-claude-init block")
}

func TestClaudeInit_StripsLegacyBlock_DeletesEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	// CLAUDE.md contains ONLY our legacy block.
	content := "<!-- crush-claude-init:v8 -->\nold delegation block\n<!-- /crush-claude-init -->\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeInitInDir(t, dir)
	})

	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "file should be deleted when only our legacy block was present")
	assert.Contains(t, stderr, "removed now-empty")
}

func TestClaudeInit_NoLegacyBlock_LeavesFileAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	original := "# My project\n\nNo crush block here.\n"
	require.NoError(t, os.WriteFile(path, []byte(original), 0o644))

	runClaudeInitInDir(t, dir)

	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, string(bts), "CLAUDE.md without our block must not be touched")
}

func TestClaudeInit_StripsMultipleLegacyVersions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	content := "# Project\n\n" +
		"<!-- crush-claude-init:v6 -->\nv6 block\n<!-- /crush-claude-init -->\n\n" +
		"middle text\n\n" +
		"<!-- crush-claude-init:v8 -->\nv8 block\n<!-- /crush-claude-init -->\n\n" +
		"tail text\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeInitInDir(t, dir)
	})

	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(bts)
	assert.NotContains(t, got, "crush-claude-init")
	assert.NotContains(t, got, "v6 block")
	assert.NotContains(t, got, "v8 block")
	assert.Contains(t, got, "# Project")
	assert.Contains(t, got, "middle text")
	assert.Contains(t, got, "tail text")
	assert.Contains(t, stderr, "stripped 2 legacy")
}

func TestClaudeInit_CreatesSlashCommand(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir)

	slashPath := filepath.Join(dir, ".claude", "commands", "rush.md")
	bts, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "rush run")
	assert.Contains(t, got, "--role smart")
}

// Content-of-the-slash-command pin tests intentionally removed (2026-05):
// asserting that specific marketing-style phrases ("opt-in only",
// "zero trust", "never echo … verbatim") appear in the installed
// markdown is testing the documentation, not the code. The slash
// command is prose meant to instruct another LLM — its wording will
// drift as we learn what works, and brittle string asserts only mean
// every refinement also has to update a test file. The behavioural
// contract that DOES matter — file gets written, our sentinel marker
// is present (so claude-del can recognise it), $ARGUMENTS placeholder
// is present (so Claude Code's slash-command machinery can substitute
// the user's prompt) — is already covered by
// TestClaudeInit_CreatesSlashCommand /
// TestClaudeInit_SlashCommandOverwritesWithSentinel /
// TestClaudeInit_SlashCommandSkipsWithoutSentinel above. The
// claude_slash_command.md content review happens at code-review
// time, not in CI.

func TestClaudeInit_SlashCommandOverwritesWithSentinel(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir)
	slashPath := filepath.Join(dir, ".claude", "commands", "rush.md")
	first, err := os.ReadFile(slashPath)
	require.NoError(t, err)

	runClaudeInitInDir(t, dir)
	second, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
}

func TestClaudeInit_SlashCommandSkipsWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	slashPath := filepath.Join(dir, ".claude", "commands", "rush.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(slashPath), 0o755))
	require.NoError(t, os.WriteFile(slashPath, []byte("someone else's file"), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeInitInDir(t, dir)
	})

	assert.Contains(t, stderr, "does not contain our sentinel")
	bts, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	assert.Equal(t, "someone else's file", string(bts))
}

// ---------------------------------------------------------------------------
// claude-del tests (unchanged — claude_del.go logic is unchanged in batch 22)
// ---------------------------------------------------------------------------

func TestClaudeDel_RemovesBlockAndKeepsRestOfFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	content := "# Project notes\n\n" +
		"<!-- crush-claude-init:v8 -->\nsome block content\n<!-- /crush-claude-init -->\n\n" +
		"## User section\n\nsome text\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	runClaudeDelInDir(t, dir)

	bts, err := os.ReadFile(path)
	require.NoError(t, err)
	got := string(bts)
	assert.NotContains(t, got, "crush-claude-init")
	assert.NotContains(t, got, "some block content")
	assert.Contains(t, got, "# Project notes")
	assert.Contains(t, got, "## User section")
	assert.Contains(t, got, "some text")
}

func TestClaudeDel_RemovesEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	content := "<!-- crush-claude-init:v8 -->\nsome block content\n<!-- /crush-claude-init -->\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeDelInDir(t, dir)
	})

	_, err := os.Stat(path)
	assert.True(t, os.IsNotExist(err), "file should be deleted when only our block was present")
	assert.Contains(t, stderr, "removed empty CLAUDE.md")
}

func TestClaudeInit_CreatesFallbackCommand(t *testing.T) {
	dir := t.TempDir()
	original, err := os.ReadFile("testdata/claude_crush_fallback_command.original.md")
	require.NoError(t, err)
	runClaudeInitInDir(t, dir)

	fallbackPath := filepath.Join(dir, ".claude", "commands", "rush-fallback.md")
	bts, err := os.ReadFile(fallbackPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Equal(t, claudeSlashCommandSentinel+"\n"+string(original), got)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "CronCreate")
	assert.Contains(t, got, "TaskCreate")
}

func TestClaudeInit_FallbackCommandOverwritesWithSentinel(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir)
	fallbackPath := filepath.Join(dir, ".claude", "commands", "rush-fallback.md")
	first, err := os.ReadFile(fallbackPath)
	require.NoError(t, err)

	runClaudeInitInDir(t, dir)
	second, err := os.ReadFile(fallbackPath)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
}

func TestClaudeInit_FallbackCommandSkipsWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	fallbackPath := filepath.Join(dir, ".claude", "commands", "rush-fallback.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(fallbackPath), 0o755))
	require.NoError(t, os.WriteFile(fallbackPath, []byte("someone else's file"), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeInitInDir(t, dir)
	})

	assert.Contains(t, stderr, "does not contain our sentinel")
	bts, err := os.ReadFile(fallbackPath)
	require.NoError(t, err)
	assert.Equal(t, "someone else's file", string(bts))
}

func TestClaudeInit_CreatesWrushCommand(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir)

	wrushPath := filepath.Join(dir, ".claude", "commands", "wrush.md")
	bts, err := os.ReadFile(wrushPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "/wrush <task>` is opt-in")
	assert.Contains(t, got, "dedicated git worktree—even for solo, trivial, or self-performed work")
	assert.Contains(t, got, "git worktree add -b <task-slug>")
	assert.Contains(t, got, "Commit the transferred changes in the primary branch")
}

func TestClaudeInit_WrushCommandOverwritesWithSentinel(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir)
	wrushPath := filepath.Join(dir, ".claude", "commands", "wrush.md")
	first, err := os.ReadFile(wrushPath)
	require.NoError(t, err)

	runClaudeInitInDir(t, dir)
	second, err := os.ReadFile(wrushPath)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
}

func TestClaudeInit_WrushCommandSkipsWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	wrushPath := filepath.Join(dir, ".claude", "commands", "wrush.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(wrushPath), 0o755))
	require.NoError(t, os.WriteFile(wrushPath, []byte("someone else's file"), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeInitInDir(t, dir)
	})

	assert.Contains(t, stderr, "does not contain our sentinel")
	bts, err := os.ReadFile(wrushPath)
	require.NoError(t, err)
	assert.Equal(t, "someone else's file", string(bts))
}

func TestClaudeInit_CreatesWcrushCommand(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir)

	wcrushPath := filepath.Join(dir, ".claude", "commands", "wcrush.md")
	bts, err := os.ReadFile(wcrushPath)
	require.NoError(t, err)
	got := string(bts)
	assert.Contains(t, got, claudeSlashCommandSentinel)
	assert.Contains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "/wcrush")
	assert.Contains(t, got, "/wcrush <task>` is opt-in")
	assert.Contains(t, got, "## Mandatory: two phases")
	assert.Contains(t, got, "`/wrush`'s dedicated git worktree, no-sub-agent-commit/push, orchestrator transfer/commit, and worktree removal rules remain in force")
	assert.Contains(t, got, "commits only those changes before removing the worktree")
	assert.Contains(t, got, "read the `wrush.md` file in this")
	assert.NotContains(t, got, "NAME PROVISIONAL")
}

func TestClaudeInit_WcrushCommandOverwritesWithSentinel(t *testing.T) {
	dir := t.TempDir()
	runClaudeInitInDir(t, dir)
	wcrushPath := filepath.Join(dir, ".claude", "commands", "wcrush.md")
	first, err := os.ReadFile(wcrushPath)
	require.NoError(t, err)

	runClaudeInitInDir(t, dir)
	second, err := os.ReadFile(wcrushPath)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second))
}

func TestClaudeInit_WcrushCommandSkipsWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	wcrushPath := filepath.Join(dir, ".claude", "commands", "wcrush.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(wcrushPath), 0o755))
	require.NoError(t, os.WriteFile(wcrushPath, []byte("someone else's file"), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeInitInDir(t, dir)
	})

	assert.Contains(t, stderr, "does not contain our sentinel")
	bts, err := os.ReadFile(wcrushPath)
	require.NoError(t, err)
	assert.Equal(t, "someone else's file", string(bts))
}

func TestClaudeInit_GlobalScopeInstallsAllSlashCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	cmd := &cobra.Command{}
	cmd.Flags().Bool("global", false, "")
	cmd.Flags().Bool("local", false, "")
	cmd.Flags().StringP("cwd", "c", "", "")
	require.NoError(t, cmd.ParseFlags([]string{"--global"}))
	require.NoError(t, claudeInitCmd.RunE(cmd, nil))

	for _, name := range []string{"rush.md", "rush-fallback.md", "wrush.md", "wcrush.md"} {
		_, err := os.Stat(filepath.Join(home, ".claude", "commands", name))
		require.NoError(t, err, "global %s should be installed", name)
	}
}

func TestClaudeDel_RemovesSlashCommandWithSentinel(t *testing.T) {
	dir := t.TempDir()
	slashPath := filepath.Join(dir, ".claude", "commands", "rush.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(slashPath), 0o755))
	require.NoError(t, os.WriteFile(slashPath, []byte("<!-- rush-slash-command:v1 -->\nsome content\n"), 0o644))

	claudeMdPath := filepath.Join(dir, claudeMdFile)
	require.NoError(t, os.WriteFile(claudeMdPath, []byte("# Notes\n"), 0o644))

	runClaudeDelInDir(t, dir)

	_, err := os.Stat(slashPath)
	assert.True(t, os.IsNotExist(err), "slash command file should be removed when it has our sentinel")
}

func TestClaudeDel_RefusesSlashCommandWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	slashPath := filepath.Join(dir, ".claude", "commands", "rush.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(slashPath), 0o755))
	require.NoError(t, os.WriteFile(slashPath, []byte("not ours"), 0o644))

	claudeMdPath := filepath.Join(dir, claudeMdFile)
	require.NoError(t, os.WriteFile(claudeMdPath, []byte("# Notes\n"), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeDelInDir(t, dir)
	})

	assert.Contains(t, stderr, "refusing to delete")
	assert.Contains(t, stderr, "missing sentinel")

	bts, err := os.ReadFile(slashPath)
	require.NoError(t, err)
	assert.Equal(t, "not ours", string(bts))
}

func TestClaudeDel_RemovesFallbackCommandWithSentinel(t *testing.T) {
	dir := t.TempDir()
	fallbackPath := filepath.Join(dir, ".claude", "commands", "rush-fallback.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(fallbackPath), 0o755))
	require.NoError(t, os.WriteFile(fallbackPath, []byte("<!-- rush-slash-command:v1 -->\nsome content\n"), 0o644))

	claudeMdPath := filepath.Join(dir, claudeMdFile)
	require.NoError(t, os.WriteFile(claudeMdPath, []byte("# Notes\n"), 0o644))

	runClaudeDelInDir(t, dir)

	_, err := os.Stat(fallbackPath)
	assert.True(t, os.IsNotExist(err), "fallback command file should be removed when it has our sentinel")
}

func TestClaudeDel_RefusesFallbackCommandWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	fallbackPath := filepath.Join(dir, ".claude", "commands", "rush-fallback.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(fallbackPath), 0o755))
	require.NoError(t, os.WriteFile(fallbackPath, []byte("not ours"), 0o644))

	claudeMdPath := filepath.Join(dir, claudeMdFile)
	require.NoError(t, os.WriteFile(claudeMdPath, []byte("# Notes\n"), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeDelInDir(t, dir)
	})

	assert.Contains(t, stderr, "refusing to delete")
	assert.Contains(t, stderr, "missing sentinel")

	bts, err := os.ReadFile(fallbackPath)
	require.NoError(t, err)
	assert.Equal(t, "not ours", string(bts))
}

func TestClaudeDel_RemovesWrushCommandWithSentinel(t *testing.T) {
	dir := t.TempDir()
	wrushPath := filepath.Join(dir, ".claude", "commands", "wrush.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(wrushPath), 0o755))
	require.NoError(t, os.WriteFile(wrushPath, []byte("<!-- rush-slash-command:v1 -->\nsome content\n"), 0o644))

	claudeMdPath := filepath.Join(dir, claudeMdFile)
	require.NoError(t, os.WriteFile(claudeMdPath, []byte("# Notes\n"), 0o644))

	runClaudeDelInDir(t, dir)

	_, err := os.Stat(wrushPath)
	assert.True(t, os.IsNotExist(err), "wrush command file should be removed when it has our sentinel")
}

func TestClaudeDel_RefusesWrushCommandWithoutSentinel(t *testing.T) {
	dir := t.TempDir()
	wrushPath := filepath.Join(dir, ".claude", "commands", "wrush.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(wrushPath), 0o755))
	require.NoError(t, os.WriteFile(wrushPath, []byte("not ours"), 0o644))

	claudeMdPath := filepath.Join(dir, claudeMdFile)
	require.NoError(t, os.WriteFile(claudeMdPath, []byte("# Notes\n"), 0o644))

	stderr := captureStderr(t, func() {
		runClaudeDelInDir(t, dir)
	})

	assert.Contains(t, stderr, "refusing to delete")
	assert.Contains(t, stderr, "missing sentinel")

	bts, err := os.ReadFile(wrushPath)
	require.NoError(t, err)
	assert.Equal(t, "not ours", string(bts))
}

func TestClaudeDel_NoOpWhenNothingPresent(t *testing.T) {
	dir := t.TempDir()

	stderr := captureStderr(t, func() {
		runClaudeDelInDir(t, dir)
	})

	assert.Contains(t, stderr, "no CLAUDE.md found")
}

func TestClaudeDel_IdempotentOnSecondRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, claudeMdFile)

	content := "# Project notes\n\n" +
		"<!-- crush-claude-init:v8 -->\nsome block content\n<!-- /crush-claude-init -->\n"
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	runClaudeDelInDir(t, dir)
	first, err := os.ReadFile(path)
	require.NoError(t, err)

	stderr := captureStderr(t, func() {
		runClaudeDelInDir(t, dir)
	})

	second, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, string(first), string(second), "second run must not change the file")
	assert.Contains(t, stderr, "no crush-claude-init block found")
}

// Per-model command and agent tests removed: that functionality has been
// extracted to the cc-arch-hands repo (`cah install` / `cah uninstall`).
// rush's claude-init/claude-del now manage only the /rush slash-command
// and the legacy CLAUDE.md block strip.
