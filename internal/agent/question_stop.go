package agent

import (
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// awaitingAnswerStoppedTitle is the finish title of a question-tool stop;
// awaitingQuestionPrefix and awaitingGuidanceMarker frame the question (and
// its options) inside the finish details, so a sub-agent's question can be
// read back from history (subAgentQuestionFromFinish).
const (
	awaitingAnswerStoppedTitle = session.AwaitingAnswerStoppedTitle
	awaitingQuestionPrefix     = session.AwaitingQuestionPrefix
	awaitingGuidanceMarker     = session.AwaitingGuidanceMarker
)

// AwaitingAnswerGuidance returns the orchestrator-facing instruction
// paragraph for a question-tool stop. Unknown roles intentionally omit the
// resume command rather than print a command rejected by `rush run`.
func AwaitingAnswerGuidance(question string, options []string, sessionID string, role string) string {
	return awaitingAnswerGuidance(question, options, sessionID, role, runtime.GOOS == "windows")
}

func awaitingAnswerGuidance(question string, options []string, sessionID string, role string, windowsShell bool) string {
	optsLine := ""
	if len(options) > 0 {
		optsLine = "\n\nSuggested options: " + strings.Join(options, " | ")
	}
	resume := ""
	switch role {
	case "smart", "fast", "worker", "reviewer":
		quotedID, ok := quoteResumeSessionID(sessionID, windowsShell)
		if !ok {
			resume = fmt.Sprintf("cannot safely render a resume command for session %q in Windows cmd; resume it manually with `rush run --role %s --session <session-id> \"<your answer>\"`", sessionID, role)
		} else {
			resume = fmt.Sprintf("resume with:\n\n  rush run --role %s --session %s \"<your answer>\"", role, quotedID)
		}
	default:
		resume = fmt.Sprintf("do not resume with a command: this run's model role %q is unknown and may not be accepted by `rush run`. Choose a supported role (smart, fast, worker, or reviewer) before resuming session %s.", role, sessionID)
	}
	return fmt.Sprintf(
		awaitingQuestionPrefix+"%s%s"+awaitingGuidanceMarker+" — rush is intentionally stopping this turn because "+
			"the agent asked a question and needs an answer before it can continue. "+
			"rush is exiting now; it will not retry or re-ask on its own.\n\n"+
			"If an orchestrating agent is driving this session: decide the answer "+
			"yourself (or ask the human operator if it's genuinely their call), then "+
			"%s\n\n"+
			"This is a normal continuation, not a retry — do not treat exit_reason "+
			"\"awaiting_answer\" as a failure.",
		question, optsLine, resume,
	)
}

// quoteResumeSessionID returns shell syntax for a session ID. Windows cmd.exe
// expands percent variables even inside double quotes, and its quote/backslash
// rules are not POSIX rules; only emit IDs from a conservative alphabet there.
func quoteResumeSessionID(sessionID string, windowsShell bool) (string, bool) {
	if windowsShell {
		if sessionID == "" {
			return `""`, true
		}
		for _, r := range sessionID {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./: \\", r) {
				continue
			}
			return "", false
		}
		if !strings.ContainsRune(sessionID, ' ') {
			return sessionID, true
		}
		// Match the standard Windows command-line quoting convention: backslashes
		// immediately before the closing quote are doubled so the argument retains
		// them. Embedded quotes remain rejected above as cmd expansion is unsafe.
		trailingBackslashes := len(sessionID) - len(strings.TrimRight(sessionID, `\`))
		return `"` + sessionID + strings.Repeat(`\`, trailingBackslashes) + `"`, true
	}
	if sessionID == "" {
		return `''`, true
	}
	for _, r := range sessionID {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:", r) {
			continue
		}
		return "'" + strings.ReplaceAll(sessionID, "'", `'\''`) + "'", true
	}
	return sessionID, true
}

// subAgentQuestionText builds the tool-result text runSubAgent returns when
// a sub-agent (the `agent` tool) stops on AwaitingAnswerError instead of
// finishing normally. Sibling to AwaitingAnswerGuidance above: that function
// is for the top-level session stopping rush run itself (nobody left to
// answer, process must exit); this one is for a sub-agent stopping while its
// orchestrator is still alive and mid-turn — the orchestrator can decide the
// answer itself and keep going without its own turn ending. Wording matters
// here: this is read by a model, and the whole point of the "return and
// resume" design (see docs/plans/2026-07-26-orchestrator-worker-e2e.md,
// section 3) is that a generic-looking error would make the parent think the
// sub-agent crashed and redo the work itself instead of answering — so this
// must read unambiguously as "the sub-agent is fine and paused, not failed".
func subAgentQuestionText(childSessionID string, ae *AwaitingAnswerError) string {
	optsLine := ""
	if len(ae.Options) > 0 {
		optsLine = "\nSuggested options: " + strings.Join(ae.Options, " | ")
	}
	return subAgentQuestionFrame(childSessionID, ae.Question+optsLine)
}

// subAgentQuestionFrame wraps a question block (the question and its
// suggested options) in the text the parent model reads.
func subAgentQuestionFrame(childSessionID, questionBlock string) string {
	return fmt.Sprintf(
		"SUB-AGENT QUESTION (session %s): %s\n"+
			"The sub-agent is paused with its context intact. To answer, call the "+
			"`agent` tool again with resume_session_id=\"%s\" and your answer as the "+
			"prompt. To abandon it instead, just continue on your own.",
		childSessionID, questionBlock, childSessionID,
	)
}

// subAgentQuestionWithPreamble puts the words the child wrote before it asked
// in front of the question frame, so the parent reads the context the question
// refers to. Only the preamble is truncated: the frame (question, options,
// resume guidance) always survives.
func subAgentQuestionWithPreamble(preamble, frame string) string {
	preamble = strings.TrimSpace(preamble)
	if preamble == "" {
		return frame
	}
	return tools.TruncateOutput(preamble) + "\n\n" + frame
}

// subAgentQuestionFromFinish reads a question-tool stop back from the finish
// part it was recorded as (awaitingAnswerStoppedFinishText) and returns the
// text the parent should get for a child that asked it in a Drain turn --
// there is no live AwaitingAnswerError left at that point.
func subAgentQuestionFromFinish(childSessionID string, fp *message.Finish) (string, bool) {
	block, ok := subAgentQuestionBlockFromFinish(fp)
	if !ok {
		return "", false
	}
	return subAgentQuestionFrame(childSessionID, block), true
}

// subAgentQuestionBlockFromFinish extracts the raw question block (question
// plus suggested options) from a question-tool stop's finish part -- the
// unwrapped input of subAgentQuestionFrame, for readers that compose their
// own frame around it (delegation_question.go's notice and supervision
// texts).
func subAgentQuestionBlockFromFinish(fp *message.Finish) (string, bool) {
	if fp == nil || fp.Reason != message.FinishReasonError || fp.Message != awaitingAnswerStoppedTitle {
		return "", false
	}
	_, rest, ok := strings.Cut(fp.Details, awaitingQuestionPrefix)
	if !ok {
		return "", false
	}
	block, _, found := strings.Cut(rest, awaitingGuidanceMarker)
	if !found {
		return "", false
	}
	return block, true
}

// awaitingAnswerStoppedFinishText builds the (msg, details) pair recorded as
// the Finish part when a session is force-stopped because the agent called
// the question tool. Mirrors peakHoursStoppedFinishText in
// peak_hours_stop.go: msg is a short, unambiguous headline (never empty —
// an empty message looks identical to a voluntary finish), details carries
// the underlying error verbatim plus the full orchestrator guidance.
func awaitingAnswerStoppedFinishText(err error, role string) (msg, details string) {
	msg = awaitingAnswerStoppedTitle
	var ae *AwaitingAnswerError
	question := ""
	var options []string
	sessionID := ""
	if errors.As(err, &ae) {
		question = ae.Question
		options = ae.Options
		sessionID = ae.SessionID
	}
	details = fmt.Sprintf("%s\n\n%s", err.Error(), AwaitingAnswerGuidance(question, options, sessionID, role))
	return msg, details
}
