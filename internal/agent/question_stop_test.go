package agent

import (
	"errors"
	"strings"
	"testing"
)

func TestAwaitingAnswerGuidance(t *testing.T) {
	t.Run("without options", func(t *testing.T) {
		guidance := AwaitingAnswerGuidance("Which env should I deploy to?", nil, "sess-123", "smart")

		if !strings.Contains(guidance, "QUESTION: Which env should I deploy to?") {
			t.Errorf("guidance %q must echo the question verbatim under a QUESTION: label", guidance)
		}
		if !strings.Contains(guidance, "rush run --role smart --session sess-123") {
			t.Errorf("guidance %q must contain a ready-to-run resume command with the session id substituted in", guidance)
		}
		if !strings.Contains(strings.ToLower(guidance), "not a crash") {
			t.Errorf("guidance %q must clarify this is an intentional stop, not a crash", guidance)
		}
		if !strings.Contains(guidance, "awaiting_answer") {
			t.Errorf("guidance %q must call out exit_reason \"awaiting_answer\" so it isn't mistaken for a failure", guidance)
		}
		if strings.Contains(guidance, "Suggested options") {
			t.Errorf("guidance %q must not print a Suggested options line when no options were given", guidance)
		}
	})

	t.Run("with options", func(t *testing.T) {
		guidance := AwaitingAnswerGuidance("Proceed with the migration?", []string{"yes", "no", "dry-run only"}, "sess-456", "smart")

		if !strings.Contains(guidance, "QUESTION: Proceed with the migration?") {
			t.Errorf("guidance %q must echo the question verbatim", guidance)
		}
		if !strings.Contains(guidance, "Suggested options: yes | no | dry-run only") {
			t.Errorf("guidance %q must list the suggested options", guidance)
		}
		if !strings.Contains(guidance, "rush run --role smart --session sess-456") {
			t.Errorf("guidance %q must contain a ready-to-run resume command with the session id substituted in", guidance)
		}
	})
}

func TestAwaitingAnswerGuidanceRoles(t *testing.T) {
	for _, role := range []string{"smart", "fast", "worker", "reviewer"} {
		guidance := AwaitingAnswerGuidance("Q?", nil, "s1", role)
		if !strings.Contains(guidance, "rush run --role "+role+" --session s1") {
			t.Errorf("role %q missing from resume guidance: %q", role, guidance)
		}
	}
	unknown := AwaitingAnswerGuidance("Q?", nil, "s1", "custom")
	if strings.Contains(unknown, "rush run ") || !strings.Contains(unknown, `role "custom" is unknown`) {
		t.Errorf("unknown role must be explained without a resume command: %q", unknown)
	}
}

func TestAwaitingAnswerStoppedFinishText(t *testing.T) {
	t.Run("generic error falls back gracefully", func(t *testing.T) {
		underlying := errors.New("agent asked a question and is awaiting an answer (session sess-789): what now?")
		msg, details := awaitingAnswerStoppedFinishText(underlying, "smart")

		if msg == "" {
			t.Fatal("message must be non-empty — empty message looks identical to a voluntary finish")
		}
		if details == "" {
			t.Fatal("details must be non-empty")
		}
		if !strings.Contains(details, underlying.Error()) {
			t.Errorf("details %q must include the underlying error verbatim", details)
		}
		if !strings.Contains(strings.ToLower(details), "not a crash") {
			t.Errorf("details %q should clarify this is an intentional stop, not a crash", details)
		}
	})

	t.Run("AwaitingAnswerError yields question, options and a resume command", func(t *testing.T) {
		err := &AwaitingAnswerError{
			Question:  "Which env should I deploy to?",
			Options:   []string{"staging", "prod"},
			SessionID: "sess-abc",
		}
		msg, details := awaitingAnswerStoppedFinishText(err, "smart")

		if msg == "" {
			t.Fatal("message must be non-empty")
		}
		if !strings.Contains(details, "QUESTION: Which env should I deploy to?") {
			t.Errorf("details %q must contain the question", details)
		}
		if !strings.Contains(details, "Suggested options: staging | prod") {
			t.Errorf("details %q must contain the suggested options", details)
		}
		if !strings.Contains(details, "rush run --role smart --session sess-abc") {
			t.Errorf("details %q must contain an explicit resume command an orchestrator can act on", details)
		}
	})
}

func TestAwaitingAnswerErrorClassification(t *testing.T) {
	err := &AwaitingAnswerError{Question: "q", SessionID: "s"}

	if !errors.Is(err, errAwaitingAnswer) {
		t.Error("errors.Is(err, errAwaitingAnswer) must hold via Unwrap, mirroring PeakHoursError/errProviderPeakHours")
	}

	wrapped := errors.Join(errors.New("context"), err)
	var ae *AwaitingAnswerError
	if !errors.As(wrapped, &ae) {
		t.Fatal("errors.As must find the AwaitingAnswerError through wrapping")
	}
	if ae.Question != "q" || ae.SessionID != "s" {
		t.Errorf("unwrapped AwaitingAnswerError lost its fields: %+v", ae)
	}
}
