package app

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/session"
)

// cliTodoNudgeLimit bounds how many unfinished-todos reminders the loop may
// fire in a row without the todos changing (#A18), by analogy with the
// reaction chain guard's fuse (#1113): a model that answers every reminder
// with another announcement must not be prompted forever. The budget resets
// whenever the todos' (content,status) fingerprint changes.
const cliTodoNudgeLimit = 2

// todoNudgePrompt is the fixed user prompt of the unfinished-todos reminder
// turn (#A18). Like the reviewer pass's prompt it must steer to a concrete
// outcome, never to more announcements.
const todoNudgePrompt = `Your todo list still has unfinished items (pending or in_progress). Continue working on them now, or mark each finished item completed and remove each dropped item from the list, then end your turn with a summary of what is done and what remains. Do not end the turn while actionable work remains.`

// openTodos reads the session's todos and returns how many are pending or
// in_progress plus a fingerprint of their (content,status) pairs; a read
// failure is (0, "", err) and never fires a nudge.
func (l *cliLoop) openTodos() (count int, fingerprint string, err error) {
	sess, err := l.app.Sessions.Get(l.ctx, l.sessionID)
	if err != nil {
		return 0, "", err
	}
	var sb strings.Builder
	for _, td := range sess.Todos {
		if td.Status == session.TodoStatusCompleted {
			continue
		}
		count++
		sb.WriteString(td.Content)
		sb.WriteByte(0)
		sb.WriteString(string(td.Status))
		sb.WriteByte(0)
	}
	return count, sb.String(), nil
}

// todoNudgeDue decides whether the close path must first fire one
// unfinished-todos reminder turn (#A18). The budget resets when the todos
// changed since the previous nudge; once the budget is spent without
// progress the run is allowed to end, with a warning recorded at the exit.
func (l *cliLoop) todoNudgeDue() bool {
	// A reminder turn already failed and was dropped (C9-4): no further
	// reminders fire.
	if l.nudgeFailed {
		return false
	}
	// The last turn ended on a question for the caller: the run exits with
	// awaiting_answer, and a reminder turn would bury the question under a
	// prompt the caller never sent.
	var asked *agent.AwaitingAnswerError
	if errors.As(l.runErr, &asked) {
		return false
	}
	// A peak-hours pause must not fire a reminder turn: the same refusal
	// would kill it and surface as a "todo reminder turn failed" error.
	// The reminder is skipped with a stderr note; the todos stay for the
	// resumed run.
	var peak *agent.PeakHoursError
	if errors.As(l.runErr, &peak) {
		fmt.Fprintf(l.errOut(), "rush run: session %q: todo reminder skipped — the provider is in its peak-hours window (%v); the todos stay for the resumed run\n", l.sessionID, peak)
		return false
	}
	n, fp, err := l.openTodos()
	if err != nil || n == 0 {
		return false
	}
	if fp != l.todoFingerprint {
		l.todoFingerprint = fp
		l.todoNudges = 0
	}
	if l.todoNudges >= cliTodoNudgeLimit {
		if !l.todosGaveUp {
			l.todosGaveUp = true
			fmt.Fprintf(l.errOut(), "rush run: session %q still has %d unfinished todo(s) after %d reminder(s); ending the run (see the envelope warning)\n", l.sessionID, n, l.todoNudges)
		}
		return false
	}
	l.todoNudges++
	return true
}

// nudgePhase runs one unfinished-todos reminder turn (#A18): a plain
// prompted turn on the executor's model, then back to decide -- the closed
// scope is re-read there, and the nudge fires again only if todos are still
// open and the budget allows. A reminder that failed for its own reason is
// dropped, never the run's outcome (C9-4); cancellation, stop, a queued
// reminder and a question keep their own paths.
func (l *cliLoop) nudgePhase() cliStepResult {
	if err := l.stopError(); err != nil {
		final, exitErr := l.exitPrecheck(err)
		return cliStepResult{ev: evNudgeEnded, final: final, err: exitErr}
	}
	usageBefore := l.sessionUsage()
	l.flushQueuedUsage(usageBefore)
	result, buffered, err := l.runTodoNudgeTurn()
	if l.ctx.Err() != nil {
		final, exitErr := l.turnCanceled(result, buffered, err, usageBefore)
		return cliStepResult{ev: evNudgeEnded, final: final, err: exitErr}
	}
	// C9-4: an optional reminder that failed for its own reason is dropped
	// -- the executor's answer stands and no further reminders fire.
	var awaiting *agent.AwaitingAnswerError
	if err != nil && !errors.Is(err, ErrRunQueued) && !errors.As(err, &awaiting) {
		l.tot.add(result)
		if result == nil {
			l.tot.addSince(usageBefore, l.sessionUsage())
		}
		// A pending operator cancel or a crossed cap beats the drop: the run ends
		// through the normal stop exit, never as a success.
		if stopErr := l.stopError(); stopErr != nil {
			final, exitErr := l.exitPrecheck(stopErr)
			return cliStepResult{ev: evNudgeEnded, final: final, err: exitErr}
		}
		l.nudgeFailed = true
		if l.final != nil {
			l.final.Warnings = append(l.final.Warnings, fmt.Sprintf("todo reminder turn failed: %v", err))
		}
		fmt.Fprintf(l.errOut(), "rush run: todo reminder turn failed: %v; the run keeps its last answer\n", err)
		l.streak = drainStreak{}
		return cliStepResult{ev: evNudgeAgain}
	}
	if done, exitErr := l.afterDrain(result, err, buffered, usageBefore); done {
		final, exitFinal := l.exit(exitErr, incompleteExitReason(exitErr))
		return cliStepResult{ev: evNudgeEnded, final: final, err: exitFinal}
	}
	return cliStepResult{ev: evNudgeAgain}
}

// runTodoNudgeTurn runs the reminder as an ordinary prompted turn (not a
// Drain: the reminder is a real user message, on the executor's model
// unless the reviewer pass already ran).
func (l *cliLoop) runTodoNudgeTurn() (*RunResult, *bytes.Buffer, error) {
	buffered, turnOutput := l.turnSink()
	result, err := l.app.ExecuteRun(l.ctx, RunRequest{
		Prompt: todoNudgePrompt, Overrides: l.turnOverrides, Mode: l.mode,
		ContinueSessionID: l.sessionID,
		Origin:            l.overrides.Origin, Stdout: turnOutput, Stderr: l.errOut(),
		HideSpinner:       l.hideSpinner,
		captureResult:     true,
		loopTurn:          true,
		nudgeTurn:         true,
		deferReviewer:     true,
		reviewerConfig:    l.reviewerDone,
		onSessionResolved: l.claim,
	})
	if cliLoopTurnDoneSeam != nil {
		cliLoopTurnDoneSeam()
	}
	return result, buffered, err
}
