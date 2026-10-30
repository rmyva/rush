// The wire-stable JSON envelope for `rush run --json`: RunResult
// and its usage/sub-agent/partial types, the builders that assemble
// it (buildRunResult, buildSessionUsageInfo), and their small text
// helpers (tailN, synthesiseEmptyFinalSummary).

package app

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
)

// RunResult is the JSON shape emitted by `rush run --json`. Wire-stable:
// fields here are part of the public contract for wrapper scripts.
type RunResult struct {
	SessionID string `json:"session_id"`
	// ExitReason vocabulary:
	//   "stop","end_turn","tool_use","max_tokens","unknown"  — model-level
	//   "provider_limit"                                    — own hard quota
	//   "error"                                              — generic
	//   "canceled"                                           — caller-cancel
	//   "timeout"                                            — owned run deadline
	//   "stalled" (fork-only)                                — turn-stall abort
	//   "invalid_json" (fork-only)                           — --json /
	//       --format json was active and stripped output failed
	//       json.Valid; orchestrators that pipe final_text into jq
	//       SHOULD branch on this instead of treating exit_reason=stop
	//       as proof the content is valid JSON.
	ExitReason string `json:"exit_reason"`
	FinalText  string `json:"final_text"`
	// Review carries the automatic reviewer pass's verdict when one ran
	// after the executor's turn; final_text stays the executor's own last
	// completed answer. Empty when no reviewer pass ran (the default).
	Review string `json:"review,omitempty"`
	// ReviewVerdict is the reviewer pass's parsed verdict: "pass",
	// "pass_with_notes", "fail", "unverified" (the reviewer passed the run
	// without a single read-tool call of its own), "unparsed" (no verdict
	// line) or "error" (the review turn itself failed while the run's own
	// answer was kept). Empty when no reviewer pass ran. It is an opinion
	// of a second model and never changes the exit code.
	ReviewVerdict string `json:"review_verdict,omitempty"`
	// Fork patch (orchestrator UX): when --json or --format json
	// triggered the fence/preamble stripper and the model HAD wrapped
	// its answer in prose or a markdown fence, the unstripped original
	// is preserved here so the orchestrator can audit what the model
	// actually said. Empty when no stripping was applied or when the
	// model returned clean JSON already. When ExitReason="invalid_json",
	// AssistantNotes carries the strip attempt's (invalid) candidate
	// for side-by-side comparison.
	AssistantNotes string `json:"assistant_notes,omitempty"`
	// StrippedBytes is how many bytes the stripper removed from
	// final_text (0 when no strip happened or when validation failed
	// and we restored the original). Surfaces observability for the
	// "model keeps writing a preamble" failure mode — orchestrators
	// can graph this over time.
	StrippedBytes int `json:"stripped_bytes,omitempty"`
	// SubAgentOutputs is populated only when --aggregation=attach was
	// passed. Each entry is one sub-session that the parent's `agent`
	// tool dispatched during this run; FinalText is the sub-agent's
	// last assistant message. Lets the orchestrator recover detail
	// the parent over-summarised away.
	SubAgentOutputs []SubAgentOutput `json:"sub_agent_outputs,omitempty"`
	Error           string           `json:"error,omitempty"`
	// Warnings are non-fatal observations about the run that an
	// orchestrator should know about even when exit_reason looks happy.
	// Examples: agent fan-out finished with empty final_text (model
	// dispatched sub-agents but never composed a final reply, so
	// orchestrators expecting structured output get nothing); write tool
	// hit a stdout-redirect target. Wrappers can ignore the field if
	// they don't care.
	Warnings   []string       `json:"warnings,omitempty"`
	ToolCalls  []ToolCallStat `json:"tool_calls"`
	Usage      UsageInfo      `json:"usage"`
	DurationMs int64          `json:"duration_ms"`
	// RecoveredPartial is set when the session had an orphan assistant
	// message from a previous interrupted run (detected by IsPartial()
	// on the latest unfinished assistant row). Contains the partial text
	// so the orchestrator can salvage it. Fork patch: batch 8.
	RecoveredPartial *RecoveredPartial `json:"recovered_partial,omitempty"`
	// Set for exit_reason provider_limit or timeout.
	ResumeCommand         string                 `json:"resume_command,omitempty"`
	QuotaResetAt          string                 `json:"quota_reset_at,omitempty"`
	FinishedWork          []FinishedWork         `json:"finished_work,omitempty"`
	PendingChildQuestions []PendingChildQuestion `json:"pending_child_questions,omitempty"`

	// finalTextWarnings is the subset of Warnings that describes THIS turn's
	// final_text (empty, truncated, reduced). Not on the wire: the `rush run`
	// loop drops it for a turn another turn superseded as the answer.
	finalTextWarnings []string
}

// RecoveredPartial describes an orphaned partial assistant message found
// during session recovery. Fork patch: batch 8.
type RecoveredPartial struct {
	MessageID   string `json:"message_id"`
	Chars       int    `json:"chars"`
	LastFlushAt int64  `json:"last_flush_at"`
	Text        string `json:"text,omitempty"`
}

type ToolCallStat struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// SubAgentOutput is one row of RunResult.SubAgentOutputs. Populated by
// the --aggregation=attach path. Title and ID are kept so the
// orchestrator can correlate with `rush sessions list`.
type SubAgentOutput struct {
	SessionID string `json:"session_id"`
	Title     string `json:"title,omitempty"`
	FinalText string `json:"final_text"`
	// CharCount is convenience for the orchestrator — saves a
	// json.length call when deciding which sub-output to show first.
	CharCount int `json:"char_count"`
}

type UsageInfo struct {
	DeltaTokens  int64   `json:"delta_tokens"`
	DeltaCostUSD float64 `json:"delta_cost_usd"`
	// Session is the per-message token accounting for the WHOLE session,
	// including prompt-cache efficiency. Nil when nothing was recorded.
	//
	// Kept in its own object rather than folded in beside delta_tokens
	// because the two are NOT comparable: delta_tokens is a difference of the
	// session's last-snapshot counters (which are overwritten each turn, so
	// the "delta" reflects the final turn's prompt size, not everything the
	// run consumed), while these figures are real sums over the message rows.
	// Presenting them as adjacent fields of one flat object would invite
	// exactly the arithmetic that mixes them.
	Session *SessionUsageInfo `json:"session,omitempty"`
}

// SessionUsageInfo is the cache/token breakdown an orchestrator needs to judge
// prompt-cache efficiency without opening the session database.
//
// Token classes are DISJOINT, so PromptTokens is their sum: InputTokens
// (fresh, full price), CacheReadTokens (served from cache) and
// CacheCreationTokens (written into it).
type SessionUsageInfo struct {
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	ReasoningTokens     int64   `json:"reasoning_tokens,omitempty"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	PromptTokens        int64   `json:"prompt_tokens"`
	TotalTokens         int64   `json:"total_tokens"`
	CostUSD             float64 `json:"cost_usd"`
	// CacheHitRatio is null when it cannot be stated: the provider does not
	// report caching, or there was no prompt. Consumers MUST branch on null
	// rather than reading 0 — a fabricated zero is indistinguishable from a
	// genuine cache miss.
	CacheHitRatio *float64 `json:"cache_hit_ratio"`
	CacheSupport  string   `json:"cache_support,omitempty"`
	// MessagesRecorded / MessagesMissingUsage state what the figures were
	// computed over. Messages written before per-message tracking existed
	// carry no usage and are EXCLUDED from the sums, not counted as zero, so
	// a low ratio on a long-lived session may simply mean thin coverage.
	MessagesRecorded     int64 `json:"messages_recorded"`
	MessagesMissingUsage int64 `json:"messages_missing_usage"`
	// Estimated is true when any contributing message's usage was synthesized
	// from message lengths because the provider sent none.
	Estimated bool `json:"estimated,omitempty"`
}

// buildSessionUsageInfo converts a message-layer usage report into the
// envelope shape. Returns nil when the session has no recorded usage at all,
// so `usage.session` is absent rather than a misleading all-zero object.
func buildSessionUsageInfo(report message.UsageReport) *SessionUsageInfo {
	if len(report.ByModel) == 0 {
		return nil
	}
	total := report.Total()
	out := &SessionUsageInfo{
		InputTokens:          total.InputTokens,
		OutputTokens:         total.OutputTokens,
		ReasoningTokens:      total.ReasoningTokens,
		CacheReadTokens:      total.CacheReadTokens,
		CacheCreationTokens:  total.CacheCreationTokens,
		PromptTokens:         total.PromptTokens(),
		TotalTokens:          total.TotalTokens,
		CostUSD:              total.CostUSD,
		CacheSupport:         string(total.CacheSupport),
		MessagesRecorded:     report.Messages(),
		MessagesMissingUsage: report.MissingUsage,
		Estimated:            total.Estimated,
	}
	if ratio, ok := total.CacheHitRatio(); ok {
		out.CacheHitRatio = &ratio
	}
	return out
}

// buildRunResult assembles RunResult from the bits collected during the
// run. exit_reason follows the same vocabulary the WUI uses (see
// message.FinishReason*) plus a synthetic "canceled" / "error" when the
// agent never finalised a message.
//
// finalErrTitle and finalErrDetails come from the assistant message's
// Finish part when Reason=error (e.g. "Stream stalled" /
// "Provider X stopped sending streaming data for over 3m0s..."). They
// surface into RunResult.Error so orchestrators see WHY a turn errored,
// not just THAT it did.
// Fork patch (orchestrator UX): assistantNotes added. Carries the
// stripped prose/fence content when --json or --format json triggered
// stripJSONEnvelope and the model had wrapped the JSON; "" otherwise.
//
// strippedBytes / stripErrMsg / stripErrReason are populated by the
// JSON validation step (stripAndExtractJSON). stripErrReason, when
// non-empty, OVERRIDES the model's finalReason so the envelope tells
// the orchestrator "you asked for JSON, it didn't validate" instead of
// the model's optimistic "stop"/"end_turn".
func buildRunResult(sessionID, finalText, assistantNotes, finalReason string, err error, canceled bool, toolCounts map[string]int, deltaTokens int64, deltaCost float64, duration time.Duration, finalErrTitle, finalErrDetails string, strippedBytes int, stripErrMsg, stripErrReason string, subAgentOutputs []SubAgentOutput, reductionWarning string) RunResult {
	reason := finalReason
	unconfirmedCanceledTerminal := canceled && err != nil && reason == string(message.FinishReasonEndTurn)
	if errors.Is(err, ErrRunQueued) {
		reason = "queued"
	} else if unconfirmedCanceledTerminal {
		// A live end_turn is provisional until the durable drain completes.
		reason = "canceled"
	} else if reason == "" {
		switch {
		case canceled:
			reason = "canceled"
		case err != nil:
			reason = "error"
		default:
			reason = "unknown"
		}
	}
	// Fork patch (orchestrator UX): the ask_question tool force-finishes
	// the turn with message.FinishReasonError (see AddFinish in agent.go's
	// awaitingErr branch) — there is no separate FinishReason for it, so
	// finalReason/reason land on the generic "error" above just like a
	// provider hiccup would. That is misleading: an orchestrator scripting
	// against exit_reason needs to tell "the agent is waiting on you" apart
	// from "the run genuinely broke" so it doesn't retry a question as if
	// it were a transient failure — same rationale that could justify a
	// dedicated "peak_hours" value, which does not exist yet either; this
	// is intentionally the first such carve-out. Re-derive it from the
	// underlying err (not from parsing the Finish text) so this stays
	// correct even if the wording in question_stop.go changes later.
	isAwaitingAnswer := false
	var awaitingErr *agent.AwaitingAnswerError
	if errors.As(err, &awaitingErr) {
		reason = "awaiting_answer"
		isAwaitingAnswer = true
	}
	calls := make([]ToolCallStat, 0, len(toolCounts))
	for name, count := range toolCounts {
		calls = append(calls, ToolCallStat{Name: name, Count: count})
	}
	// Stable ordering so the JSON diffs cleanly across runs.
	sort.Slice(calls, func(i, j int) bool { return calls[i].Name < calls[j].Name })

	// Warnings: non-fatal observations the orchestrator should see.
	var warnings, textWarnings []string
	if reason == "queued" {
		warnings = append(warnings, "prompt was queued behind an active session run; this invocation did not execute it")
	}
	// Fan-out without composition: model dispatched at least one sub-agent
	// (`agent`/`agentic_fetch`) but the turn ended with no final text. The
	// orchestrator asked for a structured answer and got an empty string,
	// which usually means the model expected the sub-agents to "be the
	// answer" — but `rush run` returns ONLY the top-level final_text, so
	// the actual content sits in the sub-session DB rows the orchestrator
	// can't easily see. Telling them to either prompt for a wrap-up
	// summary or fetch the sub-session data explicitly.
	if reason != "error" && reason != "canceled" && reason != "awaiting_answer" && reason != "queued" && strings.TrimSpace(finalText) == "" {
		fanoutCalls := toolCounts["agent"] + toolCounts["agentic_fetch"]
		if fanoutCalls > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"final_text is empty after %d sub-agent fan-out call(s). The model dispatched sub-agents but did not compose a top-level reply — query the sub-session DB rows directly, or prompt the model to summarise into final_text.",
				fanoutCalls,
			))
			textWarnings = append(textWarnings, warnings[len(warnings)-1])
		} else {
			// Fork patch (orchestrator UX): the model ended the turn on a
			// tool_call without composing a final assistant text. The
			// orchestrator now has no human-readable summary. Synthesise a
			// one-liner from the tool counts so they can at least decide
			// whether to look at `git status --short` or re-prompt for a
			// proper summary.
			if synth := synthesiseEmptyFinalSummary(toolCounts); synth != "" {
				warnings = append(warnings, "final_text is empty (model ended on a tool_call without composing a reply). "+synth+" Inspect `git status --short` or `rush sessions last <id>` for context, or re-prompt asking for a summary.")
			} else {
				warnings = append(warnings, "final_text is empty and no tools were called this turn. The model produced nothing actionable.")
			}
			textWarnings = append(textWarnings, warnings[len(warnings)-1])
		}
	}
	// The in-turn progress guard (#1149) ends a turn that made no progress:
	// the operator must see it even when exit_reason looks happy ("end_turn",
	// no error). This is NOT a finalTextWarnings entry on purpose -- loopTotals
	// keeps a turn's plain warnings even after another turn supersedes its
	// answer, so a Drain that replaces the stopped turn still reports why it
	// stopped. Same precedent as the reduction-loss warning below.
	if agent.IsInTurnGuardStop(finalErrTitle) {
		warnings = append(warnings, "turn stopped by the in-turn progress guard: "+finalErrDetails)
	}
	errMsg := ""
	switch {
	case isAwaitingAnswer:
		// finalErrTitle/finalErrDetails are exactly what agent.go's
		// awaitingErr branch already wrote onto the Finish part via
		// awaitingAnswerStoppedFinishText — title is the short headline,
		// details is err.Error() plus the full AwaitingAnswerGuidance
		// (question, options, and the ready-to-run `rush run --role <role> --session
		// <id> "<answer>"` resume command). Reuse it verbatim instead of
		// falling through to the bare err.Error() the next case would use,
		// so the orchestrator sees the resume command without having to
		// re-query the session.
		switch {
		case finalErrTitle != "" && finalErrDetails != "":
			errMsg = finalErrTitle + ": " + finalErrDetails
		case finalErrTitle != "":
			errMsg = finalErrTitle
		case finalErrDetails != "":
			errMsg = finalErrDetails
		default:
			errMsg = err.Error()
		}
	case err != nil && !canceled:
		errMsg = err.Error()
	case unconfirmedCanceledTerminal:
		errMsg = err.Error()
	case reason == "error":
		// In-band error: the agent finished its turn but the model's
		// Finish part says reason=error (e.g. watchdog stall, provider
		// error, empty stream). Surface title + details so wrappers
		// don't have to re-query the DB.
		switch {
		case finalErrTitle != "" && finalErrDetails != "":
			errMsg = finalErrTitle + ": " + finalErrDetails
		case finalErrTitle != "":
			errMsg = finalErrTitle
		case finalErrDetails != "":
			errMsg = finalErrDetails
		}
	}
	// Fork patch (orchestrator UX): bug 4 from the 2026-05-17 audit
	// feedback — when reason=="error" but the model's Finish part
	// carried no Message/Details (some providers emit a bare error
	// finish), errMsg stayed empty and the orchestrator had no clue
	// WHY the turn died. Provide an informative fallback that names
	// the most likely causes so the operator at least knows where to
	// start looking. Also flag a truncation-hint when final_text
	// looks unfinished (ends mid-sentence or with a leading-in
	// punctuation like ":") so the operator sees "model was about to
	// continue".
	// Peak-hours refusal (pre-flight or mid-turn stop): the envelope must
	// carry the same guidance as stderr/DB, incl. the operator's message.
	var peakErr *agent.PeakHoursError
	if errors.As(err, &peakErr) {
		if guidance := agent.PeakHoursGuidance(peakErr); !strings.Contains(errMsg, guidance) {
			if errMsg != "" {
				errMsg += "\n\n"
			}
			errMsg += guidance
		}
	}
	if reason == "error" && errMsg == "" {
		errMsg = "unknown error (provider returned an error finish without a message — likely causes: provider HTTP error, stream stall before watchdog fired, OOM-kill, context-window overflow). Re-run with --verbose for stderr detail."
	}
	if reason == "error" {
		trimmed := strings.TrimRight(strings.TrimSpace(finalText), " \t")
		if n := len(trimmed); n > 0 {
			last := trimmed[n-1]
			if last == ':' || last == ',' || last == '-' {
				warnings = append(warnings, fmt.Sprintf(
					"final_text appears truncated (ends with %q) — model was likely composing more output when the error fired. Last 80 chars: %q",
					string(last), tailN(trimmed, 80),
				))
				textWarnings = append(textWarnings, warnings[len(warnings)-1])
			}
		}
	}
	// Fork patch (orchestrator UX): strip-validation overrides reason
	// + error when the operator asked for JSON and the stripped output
	// did not parse. We DO want to clobber the model's optimistic
	// "end_turn" / "stop" here because from the orchestrator's point
	// of view this run failed its contract.
	if stripErrReason != "" {
		reason = stripErrReason
		if errMsg == "" {
			errMsg = stripErrMsg
		} else {
			errMsg = errMsg + "; " + stripErrMsg
		}
	}
	// Fork patch (orchestrator UX): the reduction-loss warning is an
	// always-on observation about sub-agent fan-out. Appended last so
	// the more critical fan-out-empty + truncation warnings stay
	// first in the array.
	if reductionWarning != "" {
		warnings = append(warnings, reductionWarning)
		textWarnings = append(textWarnings, reductionWarning)
	}
	return RunResult{
		SessionID:       sessionID,
		ExitReason:      reason,
		FinalText:       finalText,
		AssistantNotes:  assistantNotes,
		StrippedBytes:   strippedBytes,
		SubAgentOutputs: subAgentOutputs,
		Error:           errMsg,
		Warnings:        warnings,
		ToolCalls:       calls,
		Usage: UsageInfo{
			DeltaTokens:  deltaTokens,
			DeltaCostUSD: deltaCost,
		},
		DurationMs: duration.Milliseconds(),

		finalTextWarnings: textWarnings,
	}
}

// tailN returns the last n runes of s (or the whole s if shorter). Used
// to put a small "what was the model writing when it died" snippet into
// the truncation warning without dumping kilobytes into the envelope.
func tailN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// synthesiseEmptyFinalSummary builds a one-line summary of what tools were
// used this turn so the empty-final_text warning gives the orchestrator
// SOMETHING to act on. Returns "" if no tools were called.
//
// Fork patch (orchestrator UX): a model that finishes on a tool_call
// without composing assistant text leaves final_text="". Wrappers
// reading just final_text get a silent success and have to fall back to
// `git status` or `sessions last`. The synthesised summary names the
// most-likely meaningful tools (edit/write/multiedit/bash) and counts
// the rest as "other tools".
func synthesiseEmptyFinalSummary(toolCounts map[string]int) string {
	if len(toolCounts) == 0 {
		return ""
	}
	// Group writes (edit / write / multiedit count as "files changed").
	writeTools := []string{"edit", "write", "multiedit"}
	writes := 0
	for _, t := range writeTools {
		writes += toolCounts[t]
	}
	bashes := toolCounts["bash"]
	others := 0
	for name, n := range toolCounts {
		if name == "bash" {
			continue
		}
		isWrite := false
		for _, w := range writeTools {
			if w == name {
				isWrite = true
				break
			}
		}
		if !isWrite {
			others += n
		}
	}

	parts := []string{}
	if writes > 0 {
		parts = append(parts, fmt.Sprintf("%d file edit(s)", writes))
	}
	if bashes > 0 {
		parts = append(parts, fmt.Sprintf("%d bash call(s)", bashes))
	}
	if others > 0 {
		parts = append(parts, fmt.Sprintf("%d other tool call(s)", others))
	}
	if len(parts) == 0 {
		return ""
	}
	return "Tools used: " + strings.Join(parts, ", ") + "."
}
