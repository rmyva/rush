package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/PHPCraftdream/rush/internal/session"
)

// The `rush run` loop follows the coordinator's launch decision (docs/reviews/
// 2026-09-30-async-phase4-round2-attempts-design.md sec.1.6): CLIScope answers
// what to do next -- run a Drain (Owed), wait for a paced retry, exit on
// deferred/stuck debt -- and the loop never accounts an attempt itself: the
// turn loop that ran the Drain does (agent/drain_attempt.go).
var (
	// cliDBRetryPause bounds how fast the loop re-polls after a transient
	// scope-read failure (doc sec.3.5: "a DB read error is a retry with a
	// pause, never a silent skip").
	cliDBRetryPause = 500 * time.Millisecond
	// cliLockBusyRetryOverallLimit bounds how long a run of consecutive Drain
	// REFUSALS (session lock held by another process, an unwritable lock dir,
	// a provider that is not configured, peak hours, ...) or setup failures is
	// retried before the run gives up with that error. A refusal is paced by
	// the launch gate (drainRefusalPauseLoop), a setup failure by
	// cliSetupRetryPause; neither by anything else in this loop.
	cliLockBusyRetryOverallLimit = 30 * time.Second
	// cliQueuedDrainPause is the wait after a Drain that queued behind another
	// owner (it ran no turn of its own and cannot be accounted here).
	cliQueuedDrainPause = 500 * time.Millisecond
	// cliSetupRetryPause paces the retry of a Drain that failed in setup before
	// any turn (no launch gate stands behind such a failure; the streak is
	// bounded by cliLockBusyRetryOverallLimit).
	cliSetupRetryPause = 500 * time.Millisecond
)

// cliLoopTurnDoneSeam is a test-only hook called right after each loop turn's
// ExecuteRun returns, before its result is classified. Lets a test land a
// cancellation exactly between a finished turn and the loop's next decision.
// nil in every production path.
var cliLoopTurnDoneSeam func()

// cliLoopBeforeDrainSeam is a test-only hook called after the pre-launch
// checks passed and right before a Drain's ExecuteRun starts: the window in
// which an operator's `sessions cancel` can land after the check. nil in every
// production path.
var cliLoopBeforeDrainSeam func()

// flushLoopExit renders final's envelope through output exactly the way the
// loop's normal "scope closed" exit always did: every exit from the loop --
// refusal give-up, ctx cancellation, a wait error, a stuck debt, or the
// ordinary scope-closed end -- flushes a REAL prior turn's terse text or JSON
// envelope through the SAME path. final == nil is a no-op.
func flushLoopExit(output io.Writer, mode RunMode, final *RunResult, lastBuffered *bytes.Buffer) error {
	if final == nil {
		return nil
	}
	switch mode {
	case RunModeTerse:
		if _, err := io.Copy(output, lastBuffered); err != nil {
			return err
		}
	case RunModeJSON:
		if err := json.NewEncoder(output).Encode(final); err != nil {
			return fmt.Errorf("failed to encode JSON result: %w", err)
		}
	}
	return nil
}

// sleepOrCtxDone sleeps for d, or returns false early if ctx is done first.
func sleepOrCtxDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// loopTotals accumulates what EVERY real turn of one invocation did, so any
// exit path reports the whole run and not only its last turn: usage, tool
// calls, warnings and sub-agent outputs (a cancelled or failed Drain adds its
// usage without replacing the last completed answer). cost is the sum of the
// turns' own cost deltas: only the fallback for the run's cost, which is the
// session's cost window (cliLoop.applyTotals).
type loopTotals struct {
	tokens int64
	cost   float64
	counts map[string]int
	turns  []loopTurn
	// subAgentOutputs holds each child once (by session id, in first-seen
	// order) with its latest text: every turn lists ALL of the parent's
	// sub-sessions, so concatenating turns would repeat them (R3C-7).
	subAgentOutputs []SubAgentOutput
	subAgentIndex   map[string]int
	// extraWarnings carries run-level diagnostics that belong to no turn
	// (the reaction chain guard, #1113).
	extraWarnings []string
}

// loopTurn is one real turn's warnings, split so the ones that describe its
// final_text can be dropped once another turn supplies the answer (R3C-7).
type loopTurn struct {
	result       *RunResult
	warnings     []string
	textWarnings []string
}

func (t *loopTotals) add(r *RunResult) {
	if r == nil {
		return
	}
	if t.counts == nil {
		t.counts = make(map[string]int)
	}
	t.tokens += r.Usage.DeltaTokens
	t.cost += r.Usage.DeltaCostUSD
	t.turns = append(t.turns, loopTurn{
		result:       r,
		warnings:     slices.Clone(r.Warnings),
		textWarnings: slices.Clone(r.finalTextWarnings),
	})
	for _, out := range r.SubAgentOutputs {
		if i, ok := t.subAgentIndex[out.SessionID]; ok {
			t.subAgentOutputs[i] = out
			continue
		}
		if t.subAgentIndex == nil {
			t.subAgentIndex = make(map[string]int)
		}
		t.subAgentIndex[out.SessionID] = len(t.subAgentOutputs)
		t.subAgentOutputs = append(t.subAgentOutputs, out)
	}
	for _, stat := range r.ToolCalls {
		t.counts[stat.Name] += stat.Count
	}
}

// warningsFor is the run's warning list: every turn's diagnostics, but the
// warnings about a turn's final_text only for the turn that is the answer.
func (t *loopTotals) warningsFor(final *RunResult) []string {
	var out []string
	out = append(out, t.extraWarnings...)
	for _, turn := range t.turns {
		for _, w := range turn.warnings {
			if turn.result != final && slices.Contains(turn.textWarnings, w) {
				continue
			}
			out = append(out, w)
		}
	}
	return out
}

func (t *loopTotals) applyTo(final *RunResult, started time.Time) {
	final.Usage.DeltaTokens = t.tokens
	final.Usage.DeltaCostUSD = t.cost
	// Keep warnings appended to final after its snapshot (the loop's reviewer
	// pass); skip ones the rebuild already has, so re-application stays idempotent.
	var late []string
	for i := range t.turns {
		if t.turns[i].result == final && len(final.Warnings) > len(t.turns[i].warnings) {
			late = final.Warnings[len(t.turns[i].warnings):]
			break
		}
	}
	out := t.warningsFor(final)
	for _, w := range late {
		if !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	final.Warnings = out
	final.SubAgentOutputs = t.subAgentOutputs
	final.DurationMs = time.Since(started).Milliseconds()
	final.ToolCalls = final.ToolCalls[:0]
	for name, count := range t.counts {
		final.ToolCalls = append(final.ToolCalls, ToolCallStat{Name: name, Count: count})
	}
	slices.SortFunc(final.ToolCalls, func(a, b ToolCallStat) int { return cmpName(a.Name, b.Name) })
}

// cliLoop is one `rush run` invocation that follows the coordinator's launch
// decisions: a first turn (the user's request), then Drain turns while the
// session owes a reaction and work is still running.
type cliLoop struct {
	app    *App
	source agent.ReactionDebtSource
	ctx    context.Context
	output io.Writer
	mode   RunMode

	hideSpinner       bool
	overrides         RunOverrides // the caller's, incl. OnFinishHook (run by the caller)
	turnOverrides     RunOverrides // per-turn: no OnFinishHook
	prompt            string
	continueSessionID string
	useLast           bool
	started           time.Time

	sessionID        string
	driverClaimed    bool
	claimedSessionID string
	// startMark is the session's usage when the driver was claimed: the run's
	// cost is what the session spent from here to the exit (applyTotals).
	startMark usageMark
	// reviewerDone: the reviewer pass ran (or was refused); it never runs again
	// in this invocation, and later Drains run on its call options and model.
	reviewerDone bool
	// reviewBasis is the evidence snapshot the loop's reviewer turn needs:
	// captured from the FIRST turn's ExecuteRun (which owns the user prompt)
	// and carried forward, since the loop's later calls do not repeat it.
	reviewBasis *reviewBasis

	// firstSubmitted: the user's own turn was launched (every setup step that
	// can fail before it had passed). A failure before that ends the run.
	firstSubmitted bool
	// refusedByOwner: the first turn was refused because another process holds
	// the session; the loop's exit then leaves the row and its cancel flag alone.
	refusedByOwner bool

	quota        *cliQuotaStop
	final        *RunResult    // the last COMPLETED turn's result (carries the answer)
	runErr       error         // the last real turn's outcome
	lastFailed   *RunResult    // the last real Drain's result, when it failed
	lastBuffered *bytes.Buffer // terse output of the last completed turn
	tot          loopTotals

	// streak is the retry state of the current run of Drains (see drainStreak):
	// replaced with a zero value every time the scope closes (R5C-2).
	streak drainStreak
	// queuedMark is the session's usage when the first not-yet-folded queued
	// iteration began (see afterDrain); flushQueuedUsage adds what the session
	// spent since.
	queuedMark *usageMark
	// lastOpenScopeNotice paces the wait heartbeat across the WHOLE run, not
	// per nextStep call (which restarts after every Drain).
	lastOpenScopeNotice time.Time
	// chainNoticePrinted: the reaction chain guard's (#1113) one stderr line
	// and one envelope warning were already emitted for this run.
	chainNoticePrinted bool
	// lastScope is the most recent CLIScopeState read in nextStep: the wait
	// heartbeat names its once schedule (stage 5a).
	lastScope agent.CLIScopeState
	// Unfinished-todos reminder state (#A18): how many reminders fired in a
	// row without the todos changing, their last (content,status)
	// fingerprint, whether the budget was spent without progress, and
	// whether a reminder turn failed and was dropped (C9-4).
	todoNudges      int
	todoFingerprint string
	todosGaveUp     bool
	nudgeFailed     bool
	stderr          io.Writer
}

// cliLoopStderr, when non-nil, replaces os.Stderr (read at each write, like
// the direct uses it replaced) as the loop's diagnostics sink; a test seam.
var cliLoopStderr io.Writer

func (l *cliLoop) errOut() io.Writer {
	if l.stderr != nil {
		return l.stderr
	}
	if cliLoopStderr != nil {
		return cliLoopStderr
	}
	return os.Stderr
}

func (app *App) runNonInteractiveWithAsyncResults(ctx context.Context, output io.Writer, prompt string, overrides RunOverrides, hideSpinner bool, mode RunMode, continueSessionID string, useLast bool) (final *RunResult, runErr error) {
	if output == nil {
		output = io.Discard
	}
	if err := app.resolveRunRole(&overrides); err != nil {
		return nil, err
	}
	if overrides.roleContract != nil {
		overrides.DisableSubAgents = true
		overrides.roleContract = overrides.roleContract.Clone()
	}
	if app.batchFaultWriter != nil && mode == RunModeJSON {
		output = app.batchFaultWriter
		app.batchFaultOutputMu.Lock()
		app.batchFaultOutput = func(fault error) {
			if err := app.batchFaultWriter.writeFault(continueSessionID, fault); err != nil {
				slog.Error("failed to encode batch-fault JSON result", "error", err)
			}
		}
		app.batchFaultOutputMu.Unlock()
	}
	source, isReactionSource := app.AgentCoordinator.(agent.ReactionDebtSource)
	if overrides.Origin == message.OriginCLI && !isReactionSource {
		// The loop cannot run without the coordinator's scope answers; running one
		// plain turn instead would drop the driver marker and every reaction
		// silently (R4C-5).
		return nil, fmt.Errorf("rush run: the agent coordinator (%T) is not an agent.ReactionDebtSource; the async reaction loop cannot run", app.AgentCoordinator)
	}
	if !isReactionSource || overrides.Origin != message.OriginCLI {
		final, runErr = app.ExecuteRun(ctx, RunRequest{
			Prompt: prompt, Overrides: overrides, Mode: mode,
			ContinueSessionID: continueSessionID, UseLast: useLast,
			Origin: overrides.Origin, Stdout: output, Stderr: os.Stderr,
			HideSpinner: hideSpinner,
		})
		if mode == RunModeJSON && final != nil {
			if err := json.NewEncoder(output).Encode(final); err != nil {
				return final, fmt.Errorf("failed to encode JSON result: %w", err)
			}
		}
		return final, runErr
	}

	// B12/C14 fix, part 3: run the dead-host sweep and retention purge once,
	// best-effort, per invocation (the 60s pass started by the driver claim
	// repeats them while the loop waits).
	source.RunMaintenanceSweep(ctx)
	// Resolve once for continue/useLast/new before orphan cleanup. Pin the
	// selected session so the first turn cannot resolve a different owner.
	sess, err := app.resolveSession(agent.WithCallOrigin(ctx, overrides.Origin), continueSessionID, useLast)
	if err != nil {
		return nil, err
	}
	continueSessionID, useLast = sess.ID, false
	if store := app.WakeScheduleStore(); store != nil {
		if _, err := store.CancelAwaitTasksMaxWait(ctx, sess.ID, time.Now()); err != nil {
			return nil, fmt.Errorf("run-start await_tasks cleanup: %w", err)
		}
		if n, ok := app.AgentCoordinator.(interface{ WakeSchedulerNotify() }); ok {
			n.WakeSchedulerNotify()
		}
	}

	started := time.Now()
	turnOverrides := overrides
	turnOverrides.OnFinishHook = ""
	defer func() {
		if overrides.OnFinishHook != "" && final != nil {
			runOnFinishHook(overrides.OnFinishHook, final.SessionID, final.ExitReason,
				final.Usage.DeltaCostUSD, final.Usage.DeltaTokens, time.Since(started))
		}
	}()
	l := &cliLoop{
		app: app, source: source, ctx: ctx, output: output, mode: mode,
		hideSpinner: hideSpinner, overrides: overrides, turnOverrides: turnOverrides,
		prompt: prompt, continueSessionID: continueSessionID, useLast: useLast,
		started: started, sessionID: continueSessionID, lastBuffered: &bytes.Buffer{},
		reviewBasis: app.captureReviewBasis(ctx, overrides.ModelRole, prompt, started),
	}
	// Doc sec.3.4: this loop IS the external driver for its session -- claimed
	// (durably, so no other process starts a reaction turn for it) as soon as
	// the session id resolves, released on every exit on a context detached
	// from ctx: Ctrl-C must still delete the marker.
	defer func() {
		if l.driverClaimed {
			relCtx, relCancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
			defer relCancel()
			source.ReleaseExternalDriver(relCtx, l.claimedSessionID)
		}
	}()
	return l.run()
}

// claim is ExecuteRun's onSessionResolved: it claims the driver marker once.
func (l *cliLoop) claim(resolved string) error {
	if !l.driverClaimed {
		if err := l.source.ClaimExternalDriver(l.ctx, resolved); err != nil {
			return err
		}
		l.driverClaimed = true
		l.claimedSessionID = resolved
		l.sessionID = resolved
		l.startMark = l.sessionUsage()
	}
	l.sessionID = resolved
	return nil
}

// runTurn runs one ExecuteRun: the first call carries the user's request, every
// later one is a Drain (empty prompt, mutation-free setup).
func (l *cliLoop) runTurn(first bool) (*RunResult, *bytes.Buffer, error) {
	buffered, turnOutput := l.turnSink()
	turnCtx := l.ctx
	if !first {
		// Doc sec.3.4: an empty-prompt Drain call: lifts ErrEmptyPrompt, skips
		// createUserMessage, reacts only to what its own turn-start pull moves
		// into history.
		turnCtx = agent.WithDrainCall(agent.WithBackgroundJobNotice(l.ctx))
	}
	result, err := l.app.ExecuteRun(turnCtx, RunRequest{
		Prompt: l.prompt, Overrides: l.turnOverrides, Mode: l.mode,
		ContinueSessionID: l.continueSessionID, UseLast: l.useLast,
		Origin: l.overrides.Origin, Stdout: turnOutput, Stderr: l.errOut(),
		HideSpinner:       l.hideSpinner,
		captureResult:     true,
		loopTurn:          true,
		drainTurn:         !first,
		deferReviewer:     true, // the loop runs the pass once, at its exit
		reviewerConfig:    l.reviewerDone,
		onSessionResolved: l.claim,
		onTurnSubmitted: func() {
			if first {
				l.firstSubmitted = true
			}
		},
	})
	if cliLoopTurnDoneSeam != nil {
		cliLoopTurnDoneSeam()
	}
	return result, buffered, err
}

// turnSink is where one turn's output goes: terse output is held (only the
// answer is printed, at exit), JSON is discarded (the envelope is printed at
// exit), stream flows through.
func (l *cliLoop) turnSink() (buffered *bytes.Buffer, out io.Writer) {
	buffered = &bytes.Buffer{}
	switch l.mode {
	case RunModeTerse:
		return buffered, buffered
	case RunModeJSON:
		return buffered, io.Discard
	}
	return buffered, l.output
}

// reviewerDue reports that the run ended clean and the automatic reviewer pass
// is configured and has not run yet: it runs at most once per invocation, as an
// ordinary turn, when the scope closed (never inside a Drain, R3C-1). Same
// conditions as the in-ExecuteRun pass of non-loop callers; the loop has no
// credentials.
func (l *cliLoop) reviewerDue() bool {
	return !l.reviewerDone && l.runErr == nil && l.final != nil && l.ctx.Err() == nil &&
		shouldRunReviewerPass(l.overrides.ModelRole, l.app.config.Config())
}

// runReviewerTurn runs the reviewer pass: a plain turn on the claimed session,
// no Drain marker on its ctx.
func (l *cliLoop) runReviewerTurn() (*RunResult, *bytes.Buffer, error) {
	buffered, turnOutput := l.turnSink()
	result, err := l.app.ExecuteRun(l.ctx, RunRequest{
		Overrides: l.turnOverrides, Mode: l.mode,
		ContinueSessionID: l.sessionID,
		Origin:            l.overrides.Origin, Stdout: turnOutput, Stderr: l.errOut(),
		HideSpinner:       l.hideSpinner,
		captureResult:     true,
		loopTurn:          true,
		reviewerTurn:      true,
		reviewBasis:       l.reviewBasis,
		onSessionResolved: l.claim,
	})
	if cliLoopTurnDoneSeam != nil {
		cliLoopTurnDoneSeam()
	}
	return result, buffered, err
}

// closePhase handles a closed scope: the reviewer pass first, when due. A
// clean review turn returns evCloseAgain: the turn keeps bash (every CLI bash
// is an async job), so the loop decides again -- running work waited on, Owed
// debt Drained on the reviewer's options. A pre-review Drain's result becomes
// the answer (R4C-1); a Drain after the review only settles usage and errors,
// never replacing the reviewed answer (C9-16, A10). Every other outcome ends
// the run: a failed review is
// the run's error, like the pass inside ExecuteRun (work it started is
// cancelled with the run, as at any error exit), a review that queued behind
// another owner or was refused by the session lock's holder did not run and
// the run keeps its answer (R6C-4), and a cancelled run keeps its last
// completed answer.
func (l *cliLoop) closePhase() cliStepResult {
	if !l.reviewerDue() {
		final, err := l.exit(l.runErr, "")
		return cliStepResult{ev: evCloseEnded, final: final, err: err}
	}
	l.reviewerDone = true
	if precheckErr := l.stopError(); precheckErr != nil {
		final, err := l.exitPrecheck(precheckErr)
		return cliStepResult{ev: evCloseEnded, final: final, err: err}
	}
	usageBefore := l.sessionUsage()
	l.flushQueuedUsage(usageBefore)
	result, buffered, turnErr := l.runReviewerTurn()
	switch {
	case l.ctx.Err() != nil:
		final, err := l.turnCanceled(result, buffered, turnErr, usageBefore)
		return cliStepResult{ev: evCloseEnded, final: final, err: err}
	case errors.Is(turnErr, ErrRunQueued):
		fmt.Fprintf(l.errOut(), "rush run: session %q: the reviewer pass queued behind another owner and did not run\n", l.sessionID)
		l.queuedMark = &usageBefore
		final, err := l.exit(l.runErr, "")
		return cliStepResult{ev: evCloseEnded, final: final, err: err}
	case turnRefusedByOwner(turnErr):
		// Another process holds the session lock (a human turn): the review ran
		// nothing, so the run keeps its last completed answer and ends clean.
		fmt.Fprintf(l.errOut(), "rush run: session %q: the reviewer pass did not run, another owner holds the session (%v); the run keeps its last answer\n", l.sessionID, turnErr)
		l.queuedMark = &usageBefore
		final, err := l.exit(l.runErr, "")
		return cliStepResult{ev: evCloseEnded, final: final, err: err}
	case result == nil:
		final, err := l.exit(turnErr, incompleteExitReason(turnErr))
		return cliStepResult{ev: evCloseEnded, final: final, err: err}
	default:
		l.tot.add(result)
		if turnErr == nil {
			// A10: the executor's answer stays the run's final_text (and its
			// terse output stays the printed answer); the reviewer's verdict
			// is attached as the additive review field.
			if l.final != nil {
				l.app.attachReview(l.ctx, l.final, l.sessionID, result.FinalText, l.errOut())
			}
			l.runErr, l.lastFailed = nil, nil
			return cliStepResult{ev: evCloseAgain}
		}
		if turnErr != nil && reviewFailureKeepsPrimary(l.ctx, turnErr) {
			// A review turn that failed for a reason of its own (a provider
			// error, its own timeout) keeps the executor's answer. A pending
			// operator cancel or a crossed cap beats keeping it (#1227).
			if stopErr := l.stopError(); stopErr != nil {
				final, err := l.exitPrecheck(stopErr)
				return cliStepResult{ev: evCloseEnded, final: final, err: err}
			}
			l.runErr, l.lastFailed = nil, nil
			recordReviewFailure(l.final, l.errOut(), turnErr)
			final, err := l.exit(nil, "")
			return cliStepResult{ev: evCloseEnded, final: final, err: err}
		}
		l.final, l.lastBuffered, l.runErr, l.lastFailed = result, buffered, turnErr, nil
		final, err := l.exit(turnErr, "")
		return cliStepResult{ev: evCloseEnded, final: final, err: err}
	}
}

// turnCanceled ends the run after a turn that ran while the run's ctx was
// cancelled (Ctrl-C, --timeout). Its usage is kept; a turn that had already
// committed cleanly (result, no error) keeps its answer as the run's final one
// (R3C-4); the run still exits canceled. A committed result whose clean error
// was only afterwards turned into a *RunTimeoutError by classifyRunTimeout
// (the owned deadline landed between the turn's commit and ExecuteRun's
// return) is adopted the same way, with the timeout error kept as the run's
// error so the exit still reports the timeout -- a failed or envelope-less
// turn is never adopted.
func (l *cliLoop) turnCanceled(result *RunResult, buffered *bytes.Buffer, err error, usageBefore usageMark) (*RunResult, error) {
	l.tot.add(result)
	if result == nil {
		// An interrupted turn returns no envelope; its usage is what the
		// session totals gained meanwhile.
		l.tot.addSince(usageBefore, l.sessionUsage())
	}
	if result != nil && (err == nil || runTimeoutCommitted(err)) {
		l.final, l.lastBuffered, l.runErr, l.lastFailed = result, buffered, err, nil
	}
	return l.exitCanceled()
}

// run drives the explicit state machine (app_run_async_fsm.go): each phase
// handler returns its event and, when the event ends the run, the result; the
// transition table picks the next phase. Entering phaseClose, or re-entering
// phaseDecide after it, recreates the Drain streak (R5C-2) -- stale streak
// state is unrepresentable across a scope close.
func (l *cliLoop) run() (*RunResult, error) {
	phase := phaseFirst
	res := cliStepResult{ev: evBegin}
	for phase != phaseExit {
		if res.ev == evScopeClosed || res.ev == evCloseAgain || res.ev == evNudgeAgain {
			l.streak = drainStreak{}
		}
		switch phase {
		case phaseDecide:
			res = l.decidePhase()
		case phaseDrain:
			res = l.drainPhase()
		case phaseNudge:
			res = l.nudgePhase()
		case phaseClose:
			res = l.closePhase()
		case phaseFirst:
			res = l.firstTurnPhase()
		}
		phase = transition(phase, res.ev)
	}
	return res.final, res.err
}

// firstTurnPhase runs the user's own turn and classifies how it ended.
func (l *cliLoop) firstTurnPhase() cliStepResult {
	result, buffered, err := l.runTurn(true)
	// The session never resolved, its driver claim was refused (another live
	// loop drives it), or a setup step failed before the user's turn was
	// launched (R3C-3): the request never reached the model, so there is
	// nothing of ours to wait on and no later Drain may turn the failure into
	// an exit 0 -- fail now with that error, having run nothing and without
	// the exit effects (no envelope, no ended_reason).
	if err != nil && (!l.driverClaimed || !l.firstSubmitted) {
		var timeoutErr *RunTimeoutError
		if errors.As(err, &timeoutErr) {
			if flushErr := flushLoopExit(l.output, l.mode, result, buffered); flushErr != nil {
				return cliStepResult{ev: evFirstDead, final: result, err: errors.Join(err, flushErr)}
			}
			return cliStepResult{ev: evFirstDead, final: result, err: err}
		}
		return cliStepResult{ev: evFirstDead, err: err}
	}
	l.final, l.runErr, l.lastBuffered = result, err, buffered
	l.tot.add(result)
	if result != nil {
		l.sessionID = result.SessionID
	}
	// Every later turn is a Drain: empty prompt, on the resolved session.
	l.prompt, l.continueSessionID, l.useLast = "", l.sessionID, false
	// Doc sec.3.8: a "session lock busy" refusal of the user's own turn fails
	// fast (pre-phase-4 contract, sessionBusyGuidance): the envelope is still
	// flushed and --on-finish still runs (R2C-2).
	var lockBusy *session.SessionLockBusyError
	if errors.As(err, &lockBusy) {
		// Another process owns the session: this run ran nothing, so it records
		// no ended_reason and honours no cancel request on it (R8A-1/R8A-2).
		l.refusedByOwner = true
		final, exitErr := l.exit(err, "")
		return cliStepResult{ev: evFirstLockBusy, final: final, err: exitErr}
	}
	if l.sessionID == "" || l.ctx.Err() != nil {
		final, exitErr := l.exitCanceled()
		return cliStepResult{ev: evFirstCanceled, final: final, err: exitErr}
	}
	l.latchQuota(err)
	return cliStepResult{ev: evFirstContinue}
}

// decidePhase asks nextStep what to do and maps its answer onto an event; the
// waits (paced retry, open work) block inside nextStep.
func (l *cliLoop) decidePhase() cliStepResult {
	if l.quota != nil {
		return l.quotaPhase()
	}
	step, why, waitErr := l.nextStep()
	switch {
	case step == stepCanceled:
		final, err := l.exitPrecheck(waitErr)
		return cliStepResult{ev: evScopeStop, final: final, err: err}
	case waitErr != nil:
		final, err := l.exitWait(waitErr)
		return cliStepResult{ev: evScopeWaitErr, final: final, err: err}
	case step == stepStuck:
		fmt.Fprintf(l.errOut(), "rush run: session %q has a notice the loop stopped reacting to after repeated failed attempts (%s); it stays pending -- see `rush sessions why %s`\n", l.sessionID, why, l.sessionID)
		final, err := l.exit(&runIncompleteError{reason: "error", detail: "a notice could not be reacted to: " + why}, "error")
		return cliStepResult{ev: evScopeStuck, final: final, err: err}
	case step == stepExit:
		// Stage 5a: the close is the one exit that cancels the session's loop
		// schedules -- before the exit flush, so the warning lands in the
		// envelope. Once schedules never reach here (WorkOpen).
		l.cancelLoopSchedulesAtClose()
		if l.todoNudgeDue() {
			return cliStepResult{ev: evTodosNudge}
		}
		return cliStepResult{ev: evScopeClosed}
	}
	return cliStepResult{ev: evScopeDrain}
}

// drainPhase runs one Drain iteration: the pre-launch checks, the turn, the
// classification of its outcome (afterDrain).
func (l *cliLoop) drainPhase() cliStepResult {
	if err := l.stopError(); err != nil {
		final, exitErr := l.exitPrecheck(err)
		return cliStepResult{ev: evScopeStop, final: final, err: exitErr}
	}
	usageBefore := l.sessionUsage()
	l.flushQueuedUsage(usageBefore)
	if cliLoopBeforeDrainSeam != nil {
		cliLoopBeforeDrainSeam()
	}
	result, buffered, err := l.runTurn(false)
	if l.ctx.Err() != nil {
		final, exitErr := l.turnCanceled(result, buffered, err, usageBefore)
		return cliStepResult{ev: evDrainCanceled, final: final, err: exitErr}
	}
	// A Drain that crossed the run's budget ends the run: its result is not the
	// answer, and no further paid turn follows.
	if !agent.IsDrainNotAttempted(err) && !errors.Is(err, ErrRunQueued) {
		if capErr := l.capError(); capErr != nil {
			l.tot.add(result)
			final, exitErr := l.exitPrecheck(capErr)
			return cliStepResult{ev: evDrainCapped, final: final, err: exitErr}
		}
	}
	if done, exitErr := l.afterDrain(result, err, buffered, usageBefore); done {
		final, exitFinal := l.exit(exitErr, incompleteExitReason(exitErr))
		return cliStepResult{ev: evDrainGaveUp, final: final, err: exitFinal}
	}
	return cliStepResult{ev: evDrainContinues}
}

// afterDrain classifies one finished Drain iteration. done reports that the
// loop must end now (the refusal budget ran out) with exitErr.
func (l *cliLoop) afterDrain(result *RunResult, err error, buffered *bytes.Buffer, usageBefore usageMark) (done bool, exitErr error) {
	switch verdict := classifyDrainOutcome(result, err); verdict {
	case drainQueued:
		// It queued behind another owner (or the Drain found nothing visible to
		// react to): no turn of ours to account for the debt -- the owner's loop
		// already did, and the owner's answer is not this run's. What the session
		// spends meanwhile is still the run's cost: it is folded in at the next
		// turn or the exit (queuedMark), never dropped.
		l.streak = drainStreak{}
		if l.queuedMark == nil {
			l.queuedMark = &usageBefore
		}
		l.source.WaitForHint(l.ctx, l.sessionID, time.Now().Add(cliQueuedDrainPause))
		return false, nil
	case drainRefused, drainSetupFailed:
		// A refusal is never counted or settled: the launch gate paces the retry
		// (0.5s for this loop's own session); the loop only bounds the streak.
		// A setup failure before any turn (no result: a model override that no
		// longer resolves, a run-queue read error, ...) has no gate behind it,
		// so it takes the same bounded/visible path and the loop paces it
		// itself; unbounded it would relaunch as fast as it fails.
		l.streak.failed = nil
		refused := verdict == drainRefused
		first, still := "was refused", "is still refused"
		if !refused {
			first, still = "could not be set up", "still cannot be set up"
		}
		if l.streak.since.IsZero() {
			l.streak.since = time.Now()
			fmt.Fprintf(l.errOut(), "rush run: session %q: the reaction turn %s (%v); retrying\n", l.sessionID, first, err)
		}
		if time.Since(l.streak.since) > cliLockBusyRetryOverallLimit {
			fmt.Fprintf(l.errOut(), "rush run: session %q: the reaction turn %s after %s (%v); giving up\n",
				l.sessionID, still, cliLockBusyRetryOverallLimit, err)
			return true, err
		}
		if !refused {
			sleepOrCtxDone(l.ctx, cliSetupRetryPause)
		}
		return false, nil
	}
	l.streak = drainStreak{}
	l.tot.add(result)
	switch classifyDrainOutcome(result, err) {
	case drainCompleted:
		// A completed turn (or a question -- an answer of its own kind) becomes
		// the run's answer -- until the reviewer pass has run (C9-16): a Drain
		// after the review only settles usage, never replacing the reviewed
		// answer (A10).
		if result != nil && !l.reviewerDone {
			l.final = result
			l.lastBuffered = buffered
		}
		l.runErr, l.lastFailed = err, nil
		l.streak.failed = nil
	default:
		// A failed Drain never replaces the last completed answer; the error
		// exit carries its classification.
		l.runErr, l.lastFailed = err, result
		l.streak.failed = err
		l.latchQuota(err)
	}
	return false, nil
}

// cliStep is what nextStep decided.
type cliStep int

const (
	stepExit cliStep = iota
	stepDrain
	stepStuck
	// stepCanceled: the run must stop while it waits -- `sessions cancel` was
	// seen (R7C-1) or --max-cost/--max-tokens was crossed (R8B-3); nextStep's
	// error is the precheck-style stop error.
	stepCanceled
)

// cliOpenScopeWaitNoticeInterval bounds how often the loop prints its "still
// waiting on open work" stderr heartbeat (C17 fix): the first line comes when
// the loop starts waiting, then at most one per interval over the WHOLE run
// (the cadence is not restarted by a Drain). An `Unknown` host-liveness
// verdict keeps a session's scope reported open for up to `sessions gc`'s 6h
// horizon with NO observable signal otherwise. A var so a test can shrink it.
var cliOpenScopeWaitNoticeInterval = 60 * time.Second

// cliDBErrorRetryOverallLimit bounds the DB-read-error retry loop (C17 fix): a
// persistent DB failure (disk issue, corruption) was previously retried
// forever with only a slog.Warn line. A var so a test can shrink it.
var cliDBErrorRetryOverallLimit = 30 * time.Second

// nextStep implements doc sec.3.5's CLI-loop decision through the
// coordinator's single CLIScope answer, waiting as long as it must: Owed --
// run a Drain (the pre-launch caps are checked by the caller); Paced -- wait
// until the gate reopens (a newer event reopens it early only after a refusal, never after a paid failure); running work --
// wait (heartbeat on stderr); Stuck with nothing running -- give up; Deferred
// or no debt with nothing running -- the scope is closed. A DB read error
// retries with a pause, bounded overall and visible on stderr. An operator
// cancel or a crossed cap is checked before each wait (stepCanceled, R7C-1,
// R8B-3).
func (l *cliLoop) nextStep() (step cliStep, why string, err error) {
	var dbErrorRetryStart time.Time
	for {
		if l.ctx.Err() != nil {
			return stepExit, "", l.ctx.Err()
		}
		if poller, ok := l.source.(agent.ExternalInterruptSource); ok {
			var stopErr error
			messageID, pollErr := poller.PollExternalInterrupt(l.ctx, l.sessionID, func() error {
				stopErr = l.stopError()
				return stopErr
			})
			if stopErr != nil {
				return stepCanceled, "", stopErr
			}
			if pollErr != nil {
				return stepExit, "", fmt.Errorf("rush run: pending interrupt: %w", pollErr)
			}
			if messageID != "" {
				if err := l.runWaitingInterrupt(messageID); err != nil {
					return stepExit, "", err
				}
				continue
			}
		}
		state, stateErr := l.source.CLIScope(l.ctx, l.sessionID)
		if stateErr != nil {
			if errors.Is(stateErr, agent.ErrBatchFactsSourceUnavailable) {
				fmt.Fprintf(l.errOut(), "rush run: session %q's batch fact source is unavailable; giving up: %v\n", l.sessionID, stateErr)
				return stepExit, "", fmt.Errorf("rush run: batch fact source unavailable: %w", stateErr)
			}
			slog.Warn("rush run: scope check failed; retrying", "session_id", l.sessionID, "err", stateErr)
			if dbErrorRetryStart.IsZero() {
				dbErrorRetryStart = time.Now()
			}
			if time.Since(dbErrorRetryStart) > cliDBErrorRetryOverallLimit {
				fmt.Fprintf(l.errOut(), "rush run: session %q's database has been unreadable for %s (%s); giving up\n",
					l.sessionID, cliDBErrorRetryOverallLimit, stateErr)
				return stepExit, "", stateErr
			}
			if !sleepOrCtxDone(l.ctx, cliDBRetryPause) {
				return stepExit, "", l.ctx.Err()
			}
			continue
		}
		dbErrorRetryStart = time.Time{}
		l.lastScope = state
		// The guard's line and warning belong to the MOMENT it fires (#1113):
		// even while the loop goes on waiting for open work, the operator must
		// see why the chain stopped. Printed once per run.
		if state.ChainGuard && !l.chainNoticePrinted {
			l.chainNoticePrinted = true
			chainText := fmt.Sprintf("reaction chain stopped: %d automatic turns only ran sleep/echo; their results stay for the next turn", agent.ReactionChainLimit)
			fmt.Fprintf(l.errOut(), "rush run: %s\n", chainText)
			l.tot.extraWarnings = append(l.tot.extraWarnings, chainText)
		}
		switch classifyScope(state) {
		case scopeActDrain:
			return stepDrain, "", nil
		case scopeActWaitPaced:
			if stopErr := l.stopError(); stopErr != nil {
				return stepCanceled, "", stopErr
			}
			l.noticePaced(state)
			l.source.WaitForHint(l.ctx, l.sessionID, state.RetryAt)
			continue
		case scopeActWaitOpen:
			if stopErr := l.stopError(); stopErr != nil {
				return stepCanceled, "", stopErr
			}
			if cliLoopInterruptWaitSeam != nil {
				cliLoopInterruptWaitSeam()
			}
			if now := time.Now(); l.lastOpenScopeNotice.IsZero() || now.Sub(l.lastOpenScopeNotice) >= cliOpenScopeWaitNoticeInterval {
				l.lastOpenScopeNotice = now
				fmt.Fprintf(l.errOut(), "rush run: session %q still has open work; waiting on %s\n", l.sessionID, l.describeOpenWork())
			}
			l.source.WaitForHint(l.ctx, l.sessionID, time.Time{})
			continue
		case scopeActStuck:
			return stepStuck, state.Reason, nil
		case scopeActExit:
			// Deferred: the notice stays for the next (human) turn; with
			// nothing else open the scope is closed.
			if state.Drain == agent.DrainDeferred {
				fmt.Fprintf(l.errOut(), "rush run: session %q has a notice no automatic turn is allowed for (%s); it stays for the next turn\n", l.sessionID, state.Reason)
			}
			return stepExit, "", nil
		}
	}
}

// noticePaced prints one stderr line per failed attempt, naming the error and
// when the retry comes (R3C-8): the wait can last up to two minutes. A refusal
// has its own line (afterDrain) and clears failedAttempt.
func (l *cliLoop) noticePaced(state agent.CLIScopeState) {
	if l.streak.failed == nil || state.RetryAt.IsZero() || state.RetryAt.Equal(l.streak.pacedNoticeAt) {
		return
	}
	l.streak.pacedNoticeAt = state.RetryAt
	fmt.Fprintf(l.errOut(), "rush run: session %q: the reaction turn failed (%v); retrying at %s\n",
		l.sessionID, l.streak.failed, state.RetryAt.Format(time.RFC3339))
}

func cmpName(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// flushQueuedUsage folds the TOKENS accrued since the first queued
// iteration into the run's totals. The run's cost needs no folding: the
// window is a subtree difference (startMark vs the exit reading), so spend
// between turns — whoever made it — is already inside (#1130).
func (l *cliLoop) flushQueuedUsage(now usageMark) {
	if l.queuedMark != nil {
		l.tot.addSinceTokens(*l.queuedMark, now)
		l.queuedMark = nil
	}
}

// applyTotals writes the run's totals into the envelope about to be flushed.
func (l *cliLoop) applyTotals(final *RunResult) {
	if l.queuedMark != nil {
		// Token folding for queued iterations: their spend needs no folding —
		// the subtree window below already contains it (#1130).
		l.flushQueuedUsage(l.sessionUsage())
	}
	l.tot.applyTo(final, l.started)
	// The run's cost is the SUBTREE spend from the claim to now (#1130):
	// sum of cost_self over the delegation subtree minus nothing else — the
	// difference covers what happened between turns (a delegated child's
	// spend, a human turn on the same session), which the turns' own deltas
	// miss. The per-turn sum stays as the fallback when a read failed.
	// Tokens are last-snapshot counters, summed per turn.
	if end := l.sessionUsage(); l.startMark.ok && end.ok {
		final.Usage.DeltaCostUSD = max(end.cost-l.startMark.cost, 0)
	}
}

// usageMark is the session's token and cost totals at one instant.
type usageMark struct {
	tokens int64
	cost   float64
	ok     bool
}

// sessionUsage reads the run's usage marks on a context that survives
// Ctrl-C: tokens are the root row's snapshot counters, cost is the SUBTREE
// spend (sum of cost_self over the delegation subtree, #1130).
func (l *cliLoop) sessionUsage() usageMark {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), cleanupTimeout)
	defer cancel()
	sess, err := l.app.Sessions.Get(ctx, l.sessionID)
	if err != nil {
		return usageMark{}
	}
	spent, err := l.app.Sessions.SubtreeSpent(ctx, l.sessionID)
	if err != nil {
		return usageMark{}
	}
	return usageMark{tokens: sess.PromptTokens + sess.CompletionTokens, cost: spent, ok: true}
}

// addSince adds what the session totals gained between two marks: the
// interrupted-turn path, whose own deltas never landed anywhere else.
func (t *loopTotals) addSince(before, after usageMark) {
	if !before.ok || !after.ok {
		return
	}
	t.tokens += max(after.tokens-before.tokens, 0)
	t.cost += max(after.cost-before.cost, 0)
}

// addSinceTokens folds only the token counters between two marks: the
// queued-iteration path (#1130 — the run's cost is the subtree window, so
// folding queued spend here would double-count it at the exit).
func (t *loopTotals) addSinceTokens(before, after usageMark) {
	if !before.ok || !after.ok {
		return
	}
	t.tokens += max(after.tokens-before.tokens, 0)
}
