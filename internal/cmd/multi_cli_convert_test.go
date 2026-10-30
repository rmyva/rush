package cmd

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// parseSlashCommandSource
// ---------------------------------------------------------------------------

func TestParseSlashCommandSource_Basic(t *testing.T) {
	src := "---\n" +
		"description: Do the thing\n" +
		"---\n" +
		"\n" +
		"Body line 1\n" +
		"Body line 2 with $ARGUMENTS\n"

	desc, body, err := parseSlashCommandSource(src)
	require.NoError(t, err)
	assert.Equal(t, "Do the thing", desc)
	assert.Equal(t, "Body line 1\nBody line 2 with $ARGUMENTS\n", body)
}

func TestParseSlashCommandSource_DescriptionWithColon(t *testing.T) {
	src := "---\n" +
		"description: Do the thing: carefully, and with colons: like this\n" +
		"---\n" +
		"\n" +
		"Body\n"

	desc, body, err := parseSlashCommandSource(src)
	require.NoError(t, err)
	assert.Equal(t, "Do the thing: carefully, and with colons: like this", desc)
	assert.Equal(t, "Body\n", body)
}

func TestParseSlashCommandSource_TrimsLeadingBlankLines(t *testing.T) {
	src := "---\n" +
		"description: X\n" +
		"---\n" +
		"\n\n\n" +
		"Body starts here\n"

	_, body, err := parseSlashCommandSource(src)
	require.NoError(t, err)
	assert.Equal(t, "Body starts here\n", body)
}

func TestParseSlashCommandSource_RealTemplates(t *testing.T) {
	desc1, body1, err := loadSkillSource("claude_slash_command", skillTargetClaude)
	require.NoError(t, err)
	assert.NotEmpty(t, desc1)
	assert.Contains(t, body1, "$ARGUMENTS")

	desc2, body2, err := loadSkillSource("claude_crush_fallback_command", skillTargetClaude)
	require.NoError(t, err)
	assert.NotEmpty(t, desc2)
	assert.Contains(t, body2, "$ARGUMENTS")

	desc3, body3, err := parseSlashCommandSource(claudeWrushCommandTemplate)
	require.NoError(t, err)
	assert.NotEmpty(t, desc3)
	assert.Contains(t, body3, "$ARGUMENTS")

	desc4, body4, err := parseSlashCommandSource(claudeWcrushCommandTemplate)
	require.NoError(t, err)
	assert.NotEmpty(t, desc4)
	assert.Contains(t, body4, "$ARGUMENTS")
}

func TestParseSlashCommandSource_MissingOpeningDelimiter(t *testing.T) {
	_, _, err := parseSlashCommandSource("no front matter here\n")
	assert.Error(t, err)
}

func TestParseSlashCommandSource_MissingDescriptionLine(t *testing.T) {
	src := "---\n" +
		"notdescription: X\n" +
		"---\n" +
		"Body\n"
	_, _, err := parseSlashCommandSource(src)
	assert.Error(t, err)
}

func TestParseSlashCommandSource_MissingClosingDelimiter(t *testing.T) {
	src := "---\n" +
		"description: X\n" +
		"Body without closing delimiter\n"
	_, _, err := parseSlashCommandSource(src)
	assert.Error(t, err)
}

func TestParseStructuredSkillSource_AssemblesTargetBlocksInOrder(t *testing.T) {
	const src = `description: "Do it: carefully"
blocks:
  - common: |-
      Shared 🕎 paragraph with "quotes".
      ~~~go
      fmt.Println("hello")
      ~~~
  - common: ""
    claude: |-
      Claude launch guidance.
    codex: |-
      Codex launch guidance.
  - common: Final shared paragraph.
    claude: ""
`
	desc, body, err := parseStructuredSkillSource(src, skillTargetCodex)
	require.NoError(t, err)
	assert.Equal(t, "Do it: carefully", desc)
	assert.Equal(t, "Shared 🕎 paragraph with \"quotes\".\n~~~go\nfmt.Println(\"hello\")\n~~~\n\nCodex launch guidance.\n\nFinal shared paragraph.", body)

	_, claude, err := parseStructuredSkillSource(src, skillTargetClaude)
	require.NoError(t, err)
	assert.Contains(t, claude, "Claude launch guidance.")
	assert.NotContains(t, claude, "Codex launch guidance")
	assert.NotContains(t, body, "Claude launch guidance")
}

func TestParseSkillSource_LegacyAndErrors(t *testing.T) {
	const legacy = "---\ndescription: Legacy source\n---\n\nBody stays unchanged.\n"
	desc, body, err := parseSkillSource("legacy", legacy, true, "", false, skillTargetCodex)
	require.NoError(t, err)
	wantDesc, wantBody, err := parseSlashCommandSource(legacy)
	require.NoError(t, err)
	assert.Equal(t, wantDesc, desc)
	assert.Equal(t, wantBody, body)

	_, _, err = parseSkillSource("both", legacy, true, "description: x\nblocks: []\n", true, skillTargetClaude)
	assert.ErrorContains(t, err, "both .md and .yaml")
	_, _, err = parseSkillSource("missing", "", false, "", false, skillTargetClaude)
	assert.ErrorContains(t, err, "no embedded")

	for _, tc := range []struct {
		name string
		src  string
		want string
	}{
		{name: "unknown key", src: "description: x\nunknown: y\nblocks:\n  - common: body\n", want: "field unknown not found"},
		{name: "empty object", src: "description: x\nblocks:\n  - {}\n", want: "block 1 is empty"},
		{name: "empty output", src: "description: x\nblocks:\n  - codex: only codex\n", want: "empty output"},
		{name: "unresolved marker", src: "description: x\nblocks:\n  - common: '{{RUSH_BUILD_MARKER}}'\n", want: "unresolved build marker"},
		{name: "multiple documents", src: "description: x\nblocks:\n  - common: body\n---\ndescription: y\n", want: "multiple YAML documents"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseStructuredSkillSource(tc.src, skillTargetClaude)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestValidateClaudeInitSourcesReturnsContextualYAMLErrors(t *testing.T) {
	valid := []byte("description: x\nblocks:\n  - common: body\n")
	tests := []struct {
		name     string
		files    fstest.MapFS
		contains string
	}{
		{
			name: "rush source",
			files: fstest.MapFS{
				"claude_slash_command.yaml":          {Data: []byte("description: x\nunknown: y\nblocks:\n  - common: body\n")},
				"claude_crush_fallback_command.yaml": {Data: valid},
			},
			contains: "rush slash-command source",
		},
		{
			name: "fallback source",
			files: fstest.MapFS{
				"claude_slash_command.yaml":          {Data: valid},
				"claude_crush_fallback_command.yaml": {Data: []byte("description: x\nunknown: y\nblocks:\n  - common: body\n")},
			},
			contains: "rush-fallback slash-command source",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateClaudeInitSources(test.files)
			assert.ErrorContains(t, err, test.contains)
			assert.ErrorContains(t, err, "unknown")
		})
	}
}

func TestRushClaudeSourcePreservesOriginalBodyVerbatim(t *testing.T) {
	original, err := os.ReadFile("testdata/claude_slash_command.original.md")
	require.NoError(t, err)
	wantDescription, wantBody, err := parseSlashCommandSource(string(original))
	require.NoError(t, err)
	gotDescription, gotBody, err := loadSkillSource("claude_slash_command", skillTargetClaude)
	require.NoError(t, err)
	assert.Equal(t, wantDescription, gotDescription)
	assert.Equal(t, strings.TrimSpace(wantBody), gotBody)
	for _, distinctive := range []string{
		"## Fallback when `rush` hits rate limits",
		"## Never self-add `--allow-peak-hours`",
		"## Resuming after `exit_reason: \"awaiting_answer\"`",
		"## Orchestrator mode — and why a worker's question is NOT your problem",
		"## Scoping permissions for a delegation — `--restrict-run`",
		"## After the run finishes — you are responsible for verifying everything",
		"--idle-timeout` (default `15m`",
	} {
		assert.Contains(t, gotBody, distinctive)
	}
}

// ---------------------------------------------------------------------------
// renderFrontMatterMD
// ---------------------------------------------------------------------------

func TestRenderFrontMatterMD(t *testing.T) {
	got := renderFrontMatterMD("<!-- sentinel:v1 -->", "My desc", "Hello $ARGUMENTS world", "{{args}}")
	want := "<!-- sentinel:v1 -->\n---\ndescription: My desc\n---\n\nHello {{args}} world"
	assert.Equal(t, want, got)
}

// ---------------------------------------------------------------------------
// toGeminiTOML
// ---------------------------------------------------------------------------

func TestToGeminiTOML_Basic(t *testing.T) {
	got, err := toGeminiTOML(`Say "hi"`, "Hello $ARGUMENTS world")
	require.NoError(t, err)
	assert.Contains(t, got, "# rush-slash-command:v1")
	assert.Contains(t, got, `description = "Say \"hi\""`)
	assert.Contains(t, got, `prompt = """`)
	assert.Contains(t, got, "Hello {{args}} world")
	assert.NotContains(t, got, "$ARGUMENTS")
}

func TestToGeminiTOML_ErrorsOnTripleQuoteInBody(t *testing.T) {
	_, err := toGeminiTOML("desc", `body with """ triple quotes`)
	assert.Error(t, err)
}

// ---------------------------------------------------------------------------
// toSkillMD
// ---------------------------------------------------------------------------

func TestToSkillMD(t *testing.T) {
	got := toSkillMD("rush", "My description", "Body text with $ARGUMENTS placeholder")
	assert.True(t, strings.HasPrefix(got, "---\nname: rush\ndescription: My description\n---\n"+claudeSlashCommandSentinel+"\n\n"))
	assert.Contains(t, got, "name: rush\n")
	assert.Contains(t, got, "description: My description\n")
	assert.Contains(t, got, "$ARGUMENTS")
	assert.Contains(t, got, "Body text with $ARGUMENTS placeholder")
	// The $ARGUMENTS placeholder in the body itself must NOT be rewritten.
	assert.Contains(t, got, "treat it exactly as `$ARGUMENTS` below would have been substituted")
}

func TestToCodexWrushSkillMD_RewritesAllRushSkillReferences(t *testing.T) {
	description, body, err := parseSlashCommandSource(claudeWrushCommandTemplate)
	require.NoError(t, err)

	got, err := toCodexWrushSkillMD(description, body)
	require.NoError(t, err)

	assert.NotContains(t, got, "block on `write_stdin`")
	assert.NotContains(t, got, "session handle and wait with blocking")
	assert.Contains(t, got, "session handle")
	assert.Contains(t, got, "empty\n   input")
	assert.Contains(t, got, "blocking each time")
	assert.Contains(t, got, "until `rush run` exits")
	assert.Contains(t, got, "then read the")
	assert.NotContains(t, got, "rush_done:")
	assert.NotContains(t, got, "automated wake")
	assert.NotContains(t, got, "`rush.md` file in this same directory")
	assert.Contains(t, got, "sibling `../rush/SKILL.md` file")
	assert.Equal(t, strings.Count(body, "rush.md"), strings.Count(got, "../rush/SKILL.md"))
}

func TestToCodexWrushSkillMD_DoesNotRewriteWrushFilename(t *testing.T) {
	const body = "Read the `rush.md` file in this same directory. Keep wrush.md unchanged.\n" +
		claudeWrushLaunchGuidance

	got, err := toCodexWrushSkillMD("description", body)
	require.NoError(t, err)
	assert.Contains(t, got, "sibling `../rush/SKILL.md` file")
	assert.Contains(t, got, "wrush.md")
	assert.NotContains(t, got, "w../rush/SKILL.md")
}

func TestToCodexWrushSkillMD_RejectsMissingCanonicalReference(t *testing.T) {
	_, err := toCodexWrushSkillMD("description", "body without the expected reference")
	assert.ErrorContains(t, err, "expected canonical same-directory rush.md reference")
}

func TestToCodexWcrushSkillMD_RewritesAllWrushSkillReferences(t *testing.T) {
	description, body, err := parseSlashCommandSource(claudeWcrushCommandTemplate)
	require.NoError(t, err)

	got, err := toCodexWcrushSkillMD(description, body)
	require.NoError(t, err)

	assert.NotContains(t, got, "`wrush.md` file in this")
	assert.NotContains(t, got, "block on `write_stdin`")
	assert.NotContains(t, got, "session handle and wait with blocking")
	assert.Contains(t, got, "session handle")
	assert.Contains(t, got, "empty input")
	assert.Contains(t, got, "call blocking")
	assert.Contains(t, got, "until the test process exits")
	assert.Contains(t, got, "Then read")
	assert.Contains(t, got, "long phase-2 tests")
	assert.NotContains(t, got, "wrush.md")
	assert.Contains(t, got, "sibling `../wrush/SKILL.md` file")
	assert.Equal(t, strings.Count(body, "wrush.md"), strings.Count(got, "../wrush/SKILL.md"))
	assert.Contains(t, got, "../wrush/SKILL.md's checklist")
	assert.Contains(t, got, "name: wcrush")
	assert.NotContains(t, got, "--codex-thread-id")
}

func TestToCodexWcrushSkillMD_AcceptsLineWrappedCanonicalReference(t *testing.T) {
	const body = "Read the `wrush.md` file in this\nsame directory in full before starting.\n" +
		claudeWcrushBackgroundGuidance + "\n" + claudeWcrushResumeBlock

	got, err := toCodexWcrushSkillMD("description", body)
	require.NoError(t, err)
	assert.Contains(t, got, "sibling `../wrush/SKILL.md` file")
	assert.NotContains(t, got, "wrush.md")
}

func TestToCodexWcrushSkillMD_RejectsMissingCanonicalReference(t *testing.T) {
	_, err := toCodexWcrushSkillMD("description", "body without the expected reference")
	assert.ErrorContains(t, err, "expected canonical same-directory wrush.md reference")
}

func TestCodexGuidanceRewritesFailClosedOnTemplateDrift(t *testing.T) {
	_, err := replaceCodexGuidance("source without the expected fragment", "MISSING_CLAUDE_FRAGMENT", "Codex guidance")
	assert.ErrorContains(t, err, "MISSING_CLAUDE_FRAGMENT")

	_, err = toCodexWrushSkillMD("description", "Read the `rush.md` file in this same directory.")
	assert.ErrorContains(t, err, "Bash call")

	_, err = toCodexWcrushSkillMD("description", "Read the `wrush.md` file in this same directory.")
	assert.ErrorContains(t, err, "run_in_background: true")
}

// ---------------------------------------------------------------------------
// Regression guard: source templates must never contain literal triple
// quotes, since toGeminiTOML embeds bodies into a TOML triple-quoted
// string and would otherwise silently produce broken TOML.
// ---------------------------------------------------------------------------

func TestNoTripleQuotesInSource(t *testing.T) {
	_, rush, err := loadSkillSource("claude_slash_command", skillTargetClaude)
	require.NoError(t, err)
	_, fallback, err := loadSkillSource("claude_crush_fallback_command", skillTargetClaude)
	require.NoError(t, err)
	assert.NotContains(t, rush, `"""`)
	assert.NotContains(t, fallback, `"""`)
	assert.NotContains(t, claudeWrushCommandTemplate, `"""`)
	assert.NotContains(t, claudeWcrushCommandTemplate, `"""`)
}
