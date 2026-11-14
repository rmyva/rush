// One STATUS vocabulary for the session surfaces: `sessions list`'s STATUS
// column, `sessions why`'s status line and `sessions status`'s phase mapping
// all render the shared session-activity classifier's verdict through this
// file, so the commands cannot drift apart on how a recorded ended_reason is
// presented (task FIX-STATUS (a)/(c): one source of truth).

package cmd

import (
	"github.com/PHPCraftdream/rush/internal/session"
)

// endedReasonPeakHours is the ended_reason a provider peak-hours pause
// records when a run stops because the provider is inside its peak-hours
// window. It is a pause, not a completion, so every surface renders it as
// paused rather than done.
const endedReasonPeakHours = "peak_hours"

// sessionStatusLabel renders the classifier's verdict into the STATUS word
// the sessions surfaces print ("running" / "delegating" / "crashed" / "done"
// / "paused: provider peak hours" / an unknown ended_reason verbatim; "" =
// at rest). recordedEnd is the session row's ended_reason read alongside the
// verdict: while live work keeps the session open the classifier
// deliberately ignores a recorded end (classifier decision 4), and without
// this parameter that recorded fact would be invisible in the status -- the
// exact "sessions list said running while sessions status reported a
// recorded exit" mismatch this file exists to close. Passing "" (the old
// verdict-only shape) keeps the plain word.
func sessionStatusLabel(v session.ActivityVerdict, recordedEnd string) string {
	switch v.Kind {
	case session.ActivityInTurn, session.ActivityBetweenTurns:
		return annotateRecordedEnd("running", recordedEnd)
	case session.ActivityDelegating:
		return annotateRecordedEnd("delegating", recordedEnd)
	case session.ActivityCrashed:
		return "crashed"
	case session.ActivityEnded:
		reason := v.EndedReason
		if reason == "" {
			reason = recordedEnd
		}
		return endedStatusLabel(reason)
	}
	return ""
}

// annotateRecordedEnd appends the recorded exit to a live-work status word:
// a session that is live despite a recorded ended_reason is exactly the
// anomaly an operator must see ("running (exit timeout)"), and it is the
// same fact `sessions status` reports as exit_reason.
func annotateRecordedEnd(status, recordedEnd string) string {
	if recordedEnd == "" {
		return status
	}
	return status + " (exit " + recordedEnd + ")"
}

// endedStatusLabel renders one recorded ended_reason. The known vocabulary
// maps to "done" (the historical whole-column behavior); the peak-hours
// pause maps to its paused label; anything else -- a reason a newer rush
// wrote that this binary does not know -- renders verbatim, never as
// "done".
func endedStatusLabel(reason string) string {
	switch reason {
	case "":
		return ""
	case endedReasonPeakHours:
		return "paused: provider peak hours"
	}
	if knownEndedReason(reason) {
		return "done"
	}
	return reason
}

// knownEndedReason reports whether reason is in the exit_reason vocabulary
// this codebase writes (message.FinishReason* plus the synthetic reasons the
// run result, the loop exits and the batch fault path record).
func knownEndedReason(reason string) bool {
	switch reason {
	case "awaiting_answer", "batch_fault", "canceled", "done", "end_turn",
		"error", "max_cost", "max_tokens", "queued", "stalled", "stop",
		"timeout", "tool_use", "unknown":
		return true
	}
	return false
}
