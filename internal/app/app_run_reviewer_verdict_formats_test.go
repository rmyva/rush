package app

// Format variants of the reviewer verdict line, each pinned to the wire value
// RunResult.ReviewVerdict records. The Russian rows are the real shapes the
// 2026-11-13 fix-audit found in "reviewer verdict line missing" runs: a
// review whose first line was "ПРОВЕРКА: ПРОЙДЕНО С ЗАМЕЧАНИЯМИ (Зачёт с
// замечаниями)" and one with no verdict line at all. The fail-closed rule is
// unchanged everywhere: a text without a verdict line is still "" — the
// caller records "unparsed" and warns; nothing here turns silence into a
// pass or invents a fail.

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseReviewVerdictFormatVariants walks every accepted variant plus the
// look-alikes that must stay unparsed.
//
// Revert-check: reverting reviewVerdictPattern to the pre-fix strict form
// (case-sensitive, English-only, ^\W*VERDICT:) turns every russian- and
// case-rows below red with "" instead of the mapped verdict; loosening the
// end-of-token anchor to nothing turns the "PASSED"/"ЗАЧЁТЫ"/"ЗАВЕРШЕНА"
// rows red with a verdict where "" is required.
func TestParseReviewVerdictFormatVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want string
	}{
		// The real 2026-11-13 final-l3 first line.
		{
			name: "russian prefix, pass with notes",
			text: "ПРОВЕРКА: ПРОЙДЕНО С ЗАМЕЧАНИЯМИ (Зачёт с замечаниями)\n\n## Запрос против результата\nСделано.\n",
			want: "pass_with_notes",
		},
		{
			name: "russian prefix lowercased",
			text: "проверка: пройдено с замечаниями",
			want: "pass_with_notes",
		},
		{
			name: "ВЕРДИКТ: ОТКАЗ is fail",
			text: "ВЕРДИКТ: ОТКАЗ\n\n## Findings\nP1: the build is broken\n",
			want: "fail",
		},
		{
			name: "ПРОВАЛ is fail",
			text: "ПРОВЕРКА: ПРОВАЛ",
			want: "fail",
		},
		{
			name: "ЗАЧЁТ is pass",
			text: "ПРОВЕРКА: ЗАЧЁТ\n\n## Checked myself\n",
			want: "pass",
		},
		{
			// The label exists in both ё and е spelling.
			name: "ЗАЧЕТ С ЗАМЕЧАНИЯМИ (е spelling) is pass_with_notes",
			text: "ПРОВЕРКА: ЗАЧЕТ С ЗАМЕЧАНИЯМИ",
			want: "pass_with_notes",
		},
		{
			name: "lowercase english pass_with_notes",
			text: "verdict: pass_with_notes\n\n## Findings\n",
			want: "pass_with_notes",
		},
		{
			name: "mixed-case fail with trailing text",
			text: "Verdict: Fail - see the findings below\n",
			want: "fail",
		},
		{
			name: "markdown heading",
			text: "## VERDICT: PASS\n\n## Request vs result\n",
			want: "pass",
		},
		{
			name: "markdown heading with bold fail",
			text: "### **VERDICT: FAIL**\n",
			want: "fail",
		},
		{
			name: "bold russian verdict",
			text: "**ПРОВЕРКА: ПРОЙДЕНО С ЗАМЕЧАНИЯМИ**",
			want: "pass_with_notes",
		},
		{
			name: "extra text after the token",
			text: "VERDICT: PASS_WITH_NOTES - two P2 findings below\n",
			want: "pass_with_notes",
		},
		{
			name: "trailing punctuation",
			text: "VERDICT: FAIL.",
			want: "fail",
		},
		// Look-alikes that must stay unparsed.
		{
			name: "PASSED is not PASS",
			text: "VERDICT: PASSED",
			want: "",
		},
		{
			name: "russian prefix with an unknown token",
			text: "ПРОВЕРКА: ЗАВЕРШЕНА",
			want: "",
		},
		{
			name: "a prose line without a label word",
			text: "ЗАЧЁТЫ за такую работу не выдают\n",
			want: "",
		},
		{
			// The 2026-11-13 final-l4 shape: a full report whose every
			// section heading exists but no verdict line anywhere.
			name: "a report without a verdict line stays unparsed",
			text: "## Request vs result\n\nDone as asked.\n\n## Findings\n\nP2: unrun tests.\n\n## Final answer and process\n",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, tt.want, parseReviewVerdict(tt.text))
		})
	}

	// The first line still wins over a quoted token deeper in the body,
	// in both languages.
	require.Equal(t, "fail", parseReviewVerdict("VERDICT: FAIL\n\n(quoting the prompt: \"VERDICT: PASS\")\n"))
	require.Equal(t, "fail", parseReviewVerdict("ПРОВЕРКА: ОТКАЗ\n\nниже цитата: VERDICT: PASS\n"))
}
