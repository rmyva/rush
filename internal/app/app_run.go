// The non-interactive run path: run modes and per-invocation overrides,
// session resolution, the panic-isolated agent-turn wrapper, and the
// ExecuteRun event loop that computes a run and streams output; the thin
// RunNonInteractive wrapper renders the final envelope.

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/permission"
	"github.com/PHPCraftdream/rush/internal/session"
	"github.com/charmbracelet/x/ansi"
)

// cleanupTimeout bounds the best-effort DB writes in RunNonInteractive's
// post-run defers (A-2, task #779). Chosen as a small multiple of ordinary
// SQLite lock contention (a single writer holding the DB for a query or two,
// typically single-digit milliseconds) while staying far short of the 30s
// busy_timeout configured in internal/db/connect.go -- long enough that a
// momentarily busy DB still gets the write, short enough that a genuinely
// stuck writer can't reproduce the "looks like a hang after output already
// printed" symptom this fixes.
const cleanupTimeout = 2 * time.Second

// messageEventsClosedSeam is a test-only hook fired once, the first time
// RunNonInteractive's event loop observes messageEvents closed (H-2, task
// #779). nil in production. Lets tests assert the closed-channel branch was
// actually taken exactly once (not spun on) rather than only inferring it
// from wall-clock loop termination.
var messageEventsClosedSeam func()

// executeRunBeforeTurnLaunchSeam is a test-only hook called after the live
// message subscription is installed and immediately before the turn starts.
// nil in production.
var executeRunBeforeTurnLaunchSeam func()

// executeRunDoneCaseSeam is a test-only hook invoked after the done result is
// selected and before queued message events are drained. nil in production.
var executeRunDoneCaseSeam func()

// ExecuteRun computes a single agent turn and returns the result envelope
// without rendering it (see RunMode for the output shapes that shape the
// streaming behaviour). Streaming output goes to req.Stdout, diagnostics to
// req.Stderr; nil falls back to io.Discard. For RunModeJSON the caller is
// responsible for encoding the returned *RunResult.
func (app *App) ExecuteRun(ctx context.Context, req RunRequest) (result *RunResult, runErr error) {
	runCtx := ctx
	sessionID := req.ContinueSessionID
	defer func() {
		result, runErr = classifyRunTimeout(runCtx, result, runErr, sessionID, runRoleName(req.Overrides))
	}()
	mode := req.Mode
	continueSessionID := req.ContinueSessionID
	useLast := req.UseLast
	overrides := req.Overrides
	systemPrompt := overrides.SystemPrompt
	prompt := req.Prompt
	slog.Info("Running in non-interactive mode")

	ctx, cancel, setup, err := app.prepareExecuteRun(ctx, req)
	if err != nil {
		return nil, err
	}
	defer cancel()
	// R12-H2 §1.2: the OUTER release of the SDK invocation's Call owner —
	// released when the invocation finishes (setup failures already rolled
	// back inside prepareExecuteRun). Only this owner releases; root and
	// execution owners are independent.
	defer func() {
		if setup.callBindingRelease != nil {
			setup.callBindingRelease()
		}
	}()
	stdout := setup.stdout
	stderr := setup.stderr
	credsRunner := setup.credsRunner
	smartOverride := setup.smartOverride
	fastOverride := setup.fastOverride
	modelOverrideRequested := setup.modelOverrideRequested
	stopSpinner := setup.stopSpinner
	stderrTTY := setup.stderrTTY
	progress := setup.progress
	runSpec := setup.runSpec
	folderScopeSpec := setup.folderScopeSpec
	failIfSessionBusy := setup.failIfSessionBusy

	defer stopSpinner()

	sess, err := app.resolveRunSession(ctx, req.mutationFree(), continueSessionID, useLast)
	if err != nil {
		return nil, fmt.Errorf("failed to create session for non-interactive mode: %w", err)
	}
	sessionID = sess.ID
	if setup.callOpts == nil {
		return nil, errors.New("agent depth call options unavailable")
	}
	depth, depthErr := agent.PersistSessionAgentDepth(ctx, app.Sessions, sess.ID, setup.callOpts.MaxAgentDepth)
	if depthErr != nil {
		return nil, fmt.Errorf("resolve session agent depth: %w", depthErr)
	}
	setup.callOpts.MaxAgentDepth = depth
	setup.callOpts.AgentDepthSet = true
	ctx = agent.WithCallOptions(ctx, setup.callOpts)
	if req.onSessionResolved != nil {
		if err := req.onSessionResolved(sess.ID); err != nil {
			return nil, err
		}
	}

	// Durable work accepted earlier for this session runs FIRST (FIFO), in
	// this call, before our own turn. Otherwise this process's own pump
	// admits it concurrently, our turn queues behind it and exits "queued",
	// and that exit cancels the pump's turn — a self-perpetuating hang
	// (2026-09-23, five sessions after a --timeout). Fail-fast callers keep
	// their immediate-busy contract.
	if !failIfSessionBusy {
		if err := app.drainPendingBeforeRun(ctx, sess.ID); err != nil {
			return nil, err
		}
	}

	// FailIfSessionBusy (sdk.Client.Run/RunWithCredentials): reject the
	// request when the session already has an in-process owner, instead
	// of silently queueing behind it. Opt-in on purpose: `rush run` and
	// the web server keep their intentional queueing behaviour.
	//
	// This pre-check is only the FAST path — it avoids spawning a turn
	// goroutine in the common already-busy case. It cannot close the
	// check-then-act window by itself: for FailIfSessionBusy callers the
	// atomic mailbox reservation immediately below both decides admission
	// and carries the ownership era into the turn; for queueing callers the
	// contract is still enforced at the session's mailbox reservation inside
	// sessionAgent.Run (mailbox.submit), whose single check-and-set returns
	// without queueing for such a call, so sessionAgent.Run reports
	// ErrSessionBusy (R1-4).
	if failIfSessionBusy && app.AgentCoordinator.IsSessionBusy(sess.ID) {
		slog.Warn("Run rejected: session already has an in-process owner", "session_id", sess.ID)
		return nil, fmt.Errorf("session %q is already processing another request: %w", sess.ID, agent.ErrSessionBusy)
	}

	// R2-1/R2-3 (round-2 SDK review): for fail-fast callers claim the
	// session's mailbox ownership ATOMICALLY right here — before ANY
	// per-run mutation below (system prompt, reasoning effort, auto-approve,
	// the per-session permission policy, cancel flag, budget, ended_reason,
	// title). The old shape mutated shared session state first and let the
	// real admission decision happen much later, deep inside mailbox.submit
	// (sessionAgent.Run): two simultaneous FailIfSessionBusy callers on one
	// idle session both passed the fast-path check above, both mutated
	// shared state, and the eventual loser's deferred
	// ClearSessionRunAllowlist then deleted the WINNER's armed policy
	// (R2-1), while a caller that was about to be rejected could rewrite
	// the running session's prompt, budget or title before learning it had
	// lost (R2-3). ReserveExclusive is the same atomic mbIdle->mbOwned
	// check-and-set mailbox.submit performs, reused per the review's
	// recommendation ("Move atomic run admission ahead of side effects.
	// This can also provide the owner token needed to solve R2-1
	// cleanly").
	//
	// FailIfSessionBusy == false (`rush run`, the web server) keeps the
	// intentional queueing contract untouched (R1-4): no reservation is
	// attempted, the mutations below run at queue time exactly as before,
	// and mailbox.submit decides ownership/queueing when the turn
	// goroutine reaches it.
	var (
		reservedEpoch   uint64
		reservedCancel  context.CancelFunc
		reservedHandoff atomic.Bool
		// reservedHold is the holdCtx returned by ReserveExclusive
		// (context.WithCancel-derived); Cancel(sessionID)/CancelAll landing
		// during the hold cancels it via the session mailbox.
		reservedHold context.Context
	)
	if failIfSessionBusy {
		holdCtx, epoch, cancel, ok := app.AgentCoordinator.ReserveExclusive(ctx, sess.ID)
		if !ok {
			slog.Warn("Run rejected: session already has an in-process owner (atomic reservation)", "session_id", sess.ID)
			return nil, fmt.Errorf("session %q is already processing another request: %w", sess.ID, agent.ErrSessionBusy)
		}
		reservedEpoch = epoch
		reservedCancel = cancel
		reservedHold = holdCtx
		// Ownership continues into the turn: the token makes sessionAgent.Run
		// CONTINUE this era (the same rebindDispatcher mechanism /compact and
		// rerun already use) instead of racing a fresh submit() that would
		// find the mailbox owned by this very caller. onHandoff disarms the
		// bail-out release below exactly when the turn takes over:
		// ExecuteRun can return on cancellation while the turn goroutine is
		// still running, so a blind release defer would end a live era under
		// a running turn. Every error return BETWEEN here and the handoff
		// (UpdateSystemPrompt and friends) releases via this defer instead.
		ctx = agent.WithReservedOwnership(ctx, sess.ID, reservedEpoch, reservedCancel, func() {
			reservedHandoff.Store(true)
		})
		defer func() {
			if !reservedHandoff.Load() {
				app.AgentCoordinator.ReleaseExclusive(sess.ID, reservedEpoch, reservedCancel)
			}
		}()
	}

	// R3-3: mutCtx governs every pre-handoff session mutation below. On
	// the reserved path it is reservedHold, the context ReserveExclusive
	// derives via context.WithCancel and wires into the mailbox as the
	// era's cancel target, so a Cancel(sessionID)/CancelAll landing during
	// the hold window cancels it while caller-ctx cancellation still
	// propagates (it derives from ctx). Previously ExecuteRun discarded
	// holdCtx, so mailbox-directed cancellation in this window fired a
	// placeholder nobody observed and the mutations and turn proceeded
	// anyway. After the handoff holdCtx is intentionally dead
	// (RunWithReservedOwnership / the pre-reserved-ownership path fires it
	// once the turn's own cancel is live), so the turn goroutine and
	// everything downstream keeps the original ctx.
	mutCtx := ctx
	if reservedHold != nil {
		mutCtx = reservedHold
	}

	// checkHoldCanceled is checked before every pre-handoff mutation block
	// and immediately before the handoff, so a canceled hold bails out
	// through the armed ReleaseExclusive defer without mutating session
	// state or starting a turn.
	//
	// F6 (2026-09-01 SDK review): admissionAborted records that a
	// bail-out actually fired. The ended_reason and on-finish-hook
	// defers below are registered before the LAST gate and write
	// through Background-derived contexts on purpose (a started run's
	// cleanup must complete even when its own ctx is already gone), so
	// ctx cancellation cannot stop them in the window where the whole
	// run never started; they check this flag instead. A plain bool is
	// enough: every checkHoldCanceled call site and both defers run on
	// ExecuteRun's goroutine.
	admissionAborted := false
	checkHoldCanceled := func() error {
		if reservedHold == nil {
			return nil
		}
		err := reservedHold.Err()
		if err == nil {
			return nil
		}
		admissionAborted = true
		slog.Warn("Run abandoned: cancellation landed during admission hold", "session_id", sess.ID, "err", err)
		return fmt.Errorf("session %q canceled during run admission: %w", sess.ID, err)
	}

	if continueSessionID != "" || useLast {
		slog.Info("Continuing session for non-interactive run", "session_id", sess.ID)
	} else {
		slog.Info("Created session for non-interactive run", "session_id", sess.ID)
	}

	// Persist the requested system prompt for this session. Coordinator's
	// resolveSessionSystemPrompt will pick it up on the next Run(); leaving
	// systemPrompt empty preserves whatever was previously stored (or causes
	// the default prompt to be built and stored on first run).
	if err := checkHoldCanceled(); err != nil {
		return nil, err
	}
	// A follow-up turn (req.mutationFree: a Drain, the reviewer pass) is
	// mutation-free: the invocation's own setup -- system prompt, reasoning
	// effort, model slots, cancel flag, budget, ended_reason, title -- ran once
	// with the first turn (R2C-7).
	if systemPrompt != "" && !req.mutationFree() {
		if err := app.Sessions.UpdateSystemPrompt(mutCtx, sess.ID, systemPrompt); err != nil {
			return nil, fmt.Errorf("failed to set system prompt for session: %w", err)
		}
	}

	// Automatically approve all permission requests for this non-interactive
	// session.
	// checkHoldCanceled guards both calls below: they have no ctx parameter
	// to carry cancellation, so the hold check is the only gate.
	if err := checkHoldCanceled(); err != nil {
		return nil, err
	}
	app.Permissions.AutoApproveSession(sess.ID)

	// Fork patch (run allowlist): the spec merge moved above the
	// per-call CallOptions (T10 — the folder scope's KeepCommandTools
	// decision and the fs_* AllowTools append derive from it). What
	// remains here is the compile and the session/ctx wiring below.
	// Only affects the auto-approve path exercised above; interactive
	// sessions never run this code. R2-1's epoch binding has been
	// superseded by the per-call binding (R3-4), which covers the
	// reserved path too: the reserved call also carries its policy and
	// arms it at turn start.
	compiled, allowErr := permission.BuildRunAllowlist(runSpec)
	if allowErr != nil {
		slog.Warn("Restricted-run allowlist has invalid patterns (skipping them)", "err", allowErr)
	}
	// F2: pin THIS run's compiled policy as the session baseline. It was
	// once the mechanism that kept a durably restarted turn restricted, but
	// it is now a DEMOTED legacy-row fallback: the durable run queue
	// persists each call's own policy spec (WithRunAllowlistSpec below) and
	// the pump recompiles and re-arms it per call, keyed by LogicalCallID —
	// Request consults the per-call entry first — so the baseline no longer
	// governs any NEW row. It now only judges turns rebuilt from rows
	// persisted before the spec field existed (no spec in their JSON), for
	// which nothing better can be reconstructed in-process. Residual: after
	// a real process restart even the in-memory baseline is gone, and a
	// legacy row then falls to the unrestricted process-wide gate — an
	// accepted, documented migration-window behavior that any re-run
	// through ExecuteRun heals. Never cleared on run end — a later run on
	// the same session re-arms it.
	if mgr, ok := app.Permissions.(permission.SessionRunAllowlistBaselineManager); ok {
		mgr.SetSessionRunAllowlistBaseline(sess.ID, compiled)
	}
	// R2-1: the unconditional process-wide SetRunAllowlist write is GONE
	// from this path.
	//
	// R3-4: the compiled policy is no longer armed here at call time —
	// that write raced the mailbox: a queued (FailIfSessionBusy=false)
	// call overwrote the ACTIVE owner's entry at queue time and its
	// front-end defer cleared the only entry before the queued turn ever
	// ran, and even the reserved path's entry armed before the turn
	// goroutine had been admitted by the mailbox. The policy now travels
	// with the call (agent.WithRunAllowlist below, stamped onto
	// SessionAgentCall by buildCall/runInternal) and the turn loop arms it
	// — bound to the call's LogicalCallID — only when the call actually
	// becomes the active turn, for BOTH the reserved and the legacy
	// queueing path, and clears it with that same call id at loop end.
	// FailIfSessionBusy==false keeps the queueing contract (R1-4)
	// untouched: queueing no longer has ANY global side effect at call
	// time.
	ctx = agent.WithRunAllowlist(ctx, &compiled)
	// R4-1/R4-2/R4-3: the UNCOMPILED spec travels with the call too, so it
	// is serialized onto any durable run-queue row this call produces
	// (ToSessionAgentCallData). The pump rebuilds the call, recompiles the
	// spec, and arms the restarted turn's OWN policy keyed by its
	// LogicalCallID — replacing the per-session baseline below as the
	// mechanism that keeps a durable restart restricted.
	ctx = agent.WithRunAllowlistSpec(ctx, &runSpec)

	// T12: the folder-scope spec travels with the call too, so it is
	// serialized onto any durable run-queue row this call produces
	// (ToSessionAgentCallData). The pump rebuilds the call, recompiles
	// the spec, and rebinds the restarted turn's scoped filesystem
	// toolset — without this, a durably-restarted scoped call would
	// silently come back with the shared unscoped toolset.
	if folderScopeSpec != nil {
		ctx = agent.WithFolderScopeSpec(ctx, folderScopeSpec)
	}

	// Fork patch: batch 8/30 + peak-hours bypass (R1-1). This run's
	// timeout-extension policy, cost/token caps and peak-hours bypass now
	// travel in callOpts (WithCallOptions above) and are consumed per call
	// by runInternal/agent_turn — the former SetAgentTimeoutOptions /
	// SetRunLimits / SetAllowPeakHours calls wrote coordinator-wide state
	// that a concurrent run could overwrite before this run's turn read
	// it.

	// Fork patch: batch 30 — clear stale cancel flag.
	if err := checkHoldCanceled(); err != nil {
		return nil, err
	}
	// #1101: the invocation's preparatory session writes (the one-shot
	// cancel flag, the operator-visible budget, ended_reason) run at
	// ADMISSION, not before it: runOwned invokes the callback once the
	// mailbox and the inter-process session lock are ours, so a first turn
	// refused by another process's lock leaves the owner's unread cancel,
	// budget and ended_reason untouched. A follow-up turn (mutationFree)
	// is setup-free, as before.
	if !req.mutationFree() {
		sessID := sess.ID
		providerIntent := setup.providerIntent
		customFenceEnabled := setup.callBindingRelease != nil
		var requirement *session.SessionExecutionRequirement
		if setup.callOpts != nil && setup.callOpts.Requirement != nil {
			requirementCopy := *setup.callOpts.Requirement
			requirement = &requirementCopy
		}
		ctx = agent.WithExecutionProviderIntent(ctx, sessID, agent.ExecutionProviderIntent(providerIntent))
		ctx = agent.WithAdmissionSetup(ctx, func(actx context.Context) error {
			if err := persistSessionExecutionIntent(actx, app, sessID, providerIntent, requirement, customFenceEnabled); err != nil {
				return err
			}
			app.prepareAdmittedRun(actx, sessID, overrides)
			return nil
		})
	}

	// Fork patch (operator UX): auto-title from first user prompt. If the
	// session title is empty or "Untitled Session", set it to the first 60
	// chars of the user prompt. Makes `sessions list` immediately useful
	// without requiring the orchestrator to pass a title.
	if prompt != "" && (sess.Title == "" || sess.Title == "Untitled Session" || sess.Title == sess.ID) {
		autoTitle := prompt
		if len(autoTitle) > 60 {
			autoTitle = autoTitle[:60] + "…"
		}
		// Strip newlines so it fits in one line in `sessions list`.
		autoTitle = strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' {
				return ' '
			}
			return r
		}, autoTitle)
		if err := app.Sessions.Rename(mutCtx, sess.ID, autoTitle); err != nil {
			slog.Warn("Failed to auto-title session", "session_id", sess.ID, "err", err)
		}
	}

	// Fork patch: batch 24 — on-finish hook support. Captures run
	// metadata as it becomes available and executes the hook on return.
	var (
		hookExitReason string
		hookCost       float64
		hookTokens     int64
	)
	runStart := time.Now()
	tokensBefore := sess.PromptTokens + sess.CompletionTokens
	// #1130: the window is the SUBTREE spend (delegation children included),
	// not the root row's own ledger. A failed read makes the window UNKNOWN
	// (costBeforeKnown): the hook and the envelope then report ZERO instead
	// of the whole history — still under-reporting, not inventing a number.
	costBefore, costBeforeErr := app.Sessions.SubtreeSpent(ctx, sess.ID)
	if costBeforeErr != nil {
		costBefore = 0
	}
	costBeforeKnown := costBeforeErr == nil

	// Fork patch (operator UX): persist ended_reason when the run finishes.
	// hookExitReason is always set before return, so this defer fires after it.
	//
	// A-2 (task #779): these defers deliberately use context.Background()
	// rather than the run's own ctx -- by the time they fire, ctx is
	// already cancelled (cancel() above, or a --timeout/interrupt), so
	// reusing it would make the write fail instantly every time, silently
	// dropping ended_reason/usage bookkeeping on every cancelled run. But
	// an unbounded Background() context can block for the full SQLite
	// busy_timeout (30s, see internal/db/connect.go's pragma map) waiting
	// on a lock -- to the user, who already has their result printed, that
	// looks like a hang. cleanupTimeout gives these best-effort writes a
	// short budget of their own: long enough to clear an ordinary,
	// momentary busy_timeout contention window, short enough that a
	// genuinely stuck writer can't reproduce the 30s freeze this fixes.
	defer func() {
		if admissionAborted {
			slog.Info("Skipping ended_reason write: the run never started (canceled during admission)", "session_id", sess.ID)
			return
		}
		if req.loopTurn {
			// The `rush run` loop's exit is the only writer for its turns: the
			// column stays empty while the run is in progress (R8A-1).
			return
		}
		if req.mutationFree() && (agent.IsDrainNotAttempted(runErr) || errors.Is(runErr, ErrRunQueued) || turnRefusedByOwner(runErr)) {
			// A follow-up turn that ran nothing (a Drain refused, or queued
			// behind another owner; a reviewer turn refused by the lock holder)
			// must not rewrite how the session's run ended.
			return
		}
		reason := hookExitReason
		if reason == "" {
			reason = "done"
		}
		// Same retry contract as the loop's persistEndedReason: a one-shot
		// 2s budget was observed dropping the write under concurrent exits.
		if setErr := writeEndedReasonWithRetry(context.Background(), func(ctx context.Context) error {
			return app.Sessions.SetEndedReason(ctx, sess.ID, reason)
		}); setErr != nil {
			slog.Warn("Failed to persist ended_reason", "session_id", sess.ID, "reason", reason, "err", setErr)
		}
	}()
	if overrides.OnFinishHook != "" {
		defer func() {
			if admissionAborted {
				slog.Info("Skipping on-finish hook: the run never started (canceled during admission)", "session_id", sess.ID)
				return
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), cleanupTimeout)
			defer cleanupCancel()
			if freshSess, err := app.Sessions.Get(cleanupCtx, sess.ID); err == nil {
				hookTokens = freshSess.PromptTokens + freshSess.CompletionTokens - tokensBefore
				spentNow, spentErr := app.Sessions.SubtreeSpent(cleanupCtx, sess.ID)
				if spentErr != nil {
					slog.Warn("Failed to refresh session budget for on-finish hook usage", "session_id", sess.ID, "err", spentErr)
				}
				// A failed before- or after-read leaves the window unknown:
				// hookCost stays 0 instead of carrying the whole history (or
				// going negative) (#1130).
				if costBeforeKnown && spentErr == nil {
					hookCost = max(spentNow-costBefore, 0)
				}
			} else {
				slog.Warn("Failed to refresh session for on-finish hook usage", "session_id", sess.ID, "err", err)
			}
			duration := time.Since(runStart)
			runOnFinishHook(overrides.OnFinishHook, sess.ID, hookExitReason, hookCost, hookTokens, duration)
		}()
	}

	defer func() {
		result, runErr = classifyRunTimeout(runCtx, result, runErr, sess.ID, runRoleName(overrides))
		var timeoutErr *RunTimeoutError
		if errors.As(runErr, &timeoutErr) {
			hookExitReason = "timeout"
		}
	}()

	runFn := func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
		if reservedHold != nil && modelOverrideRequested {
			return app.AgentCoordinator.RunWithReservedOwnership(ctx, sessionID, prompt,
				reservedEpoch, reservedCancel, func() { reservedHandoff.Store(true) }, smartOverride, fastOverride)
		}
		if modelOverrideRequested {
			return app.AgentCoordinator.RunWithOverrides(ctx, sessionID, prompt, smartOverride, fastOverride)
		}
		return app.AgentCoordinator.Run(ctx, sessionID, prompt)
	}
	if credsRunner != nil {
		creds := req.Credentials
		runFn = func(ctx context.Context, sessionID, prompt string) (*fantasy.AgentResult, error) {
			return credsRunner.RunWithCredentials(ctx, sessionID, prompt, creds)
		}
	}
	// Last gate before the turn goroutine launches. A cancel landing in
	// the irreducible window between this check and the turn's mailbox
	// rebind is the accepted design race (identical to the rerun
	// handler's): the turn must not run under holdCtx, since the handoff
	// deliberately retires it.
	if err := checkHoldCanceled(); err != nil {
		hookExitReason = "cancelled"
		return nil, err
	}
	if req.reviewerConfig {
		// A Drain after the loop's reviewer turn reacts to that turn's own async
		// work, so it runs as that turn did: reviewer model, sub-agents off.
		runFn, ctx = app.buildReviewerPassTurn(ctx, setup.callOpts)
	}
	// The event loop now lives on executeRunLoop (app_run_reviewer.go) so
	// it can run twice: once for the primary turn, and — only when the
	// reviewer pass fires — once more for the review turn, whose finish()
	// result becomes this function's return value. The progress-bar/
	// trailing-newline defer below stays registered here (before the
	// first phase) so its LIFO position among this function's defers is
	// unchanged.
	basis := req.reviewBasis
	if basis == nil && !req.reviewerTurn && !req.drainTurn && !req.deferReviewer {
		basis = app.captureReviewBasis(ctx, overrides.ModelRole, prompt, runStart)
	}
	loop := &executeRunLoop{
		app:             app,
		sess:            sess,
		ctx:             ctx,
		mode:            mode,
		captureResult:   req.captureResult,
		overrides:       overrides,
		stdout:          stdout,
		stderr:          stderr,
		stderrTTY:       stderrTTY,
		progress:        progress,
		stopSpinner:     stopSpinner,
		runStart:        runStart,
		tokensBefore:    tokensBefore,
		costBefore:      costBefore,
		costBeforeKnown: costBeforeKnown,
		hookExitReason:  &hookExitReason,
	}
	defer func() {
		if progress && stderrTTY {
			_, _ = fmt.Fprintf(stderr, ansi.ResetProgressBar)
		}

		// JSON mode emits its own trailing newline via json.Encoder; the
		// terse/stream modes need a bare \n so a follow-up shell prompt
		// doesn't overwrite the last token.
		if mode != RunModeJSON {
			_, _ = fmt.Fprintln(stdout)
		}
	}()

	if req.reviewerTurn {
		// The loop's own reviewer pass: only the review phase runs, as an
		// ordinary turn (the ctx carries no Drain marker).
		reviewRunFn, reviewCtx := app.buildReviewerPassTurn(ctx, setup.callOpts)
		loop.resetForReviewerPass(reviewCtx)
		if req.onTurnSubmitted != nil {
			req.onTurnSubmitted()
		}
		result, resultErr := loop.runTurnPhase(app.reviewerTurnPrompt(reviewCtx, sess.ID, basis), reviewRunFn)
		loop.flushTerseOutput()
		return result, resultErr
	}
	if req.onTurnSubmitted != nil {
		req.onTurnSubmitted()
	}
	result, resultErr := loop.runTurnPhase(prompt, runFn)
	// Fork patch (reviewer pass): a CLEAN primary phase (no error — which
	// already excludes failed, canceled, timed-out, max-cost/max-tokens
	// and queued outcomes, since finish() maps every one of those to a
	// non-nil error) on a --role smart run with a Reviewer model
	// configured continues the SAME session with one more turn on the
	// Reviewer model. That turn's own finish() result — envelope,
	// hookExitReason, ended_reason — becomes ExecuteRun's return value;
	// no retry/recovery is attempted if the review turn itself fails.
	//
	// F2 (2026-09-21 weekly audit): a CREDENTIALED run (req.Credentials,
	// sdk.Client.RunWithCredentials) never auto-follows with the review
	// turn. The review turn goes through RunWithOverrides — the GLOBAL
	// config path — so a tenant whose own credential set has no reviewer
	// slot would silently send its session transcript to the operator's
	// globally-configured reviewer provider. Until the reviewer pass
	// participates in the same per-call credential isolation, it is off
	// entirely for credentialed runs.
	// R2-4: the gate refuses to open a new phase for a run the parent
	// canceled — either finish() recorded a committed success that
	// suppressed a cancellation (canceledAfterCommit), or the cancel
	// landed in the window between finish() returning and this check.
	// In both cases the committed primary result is the run's final
	// answer; a review turn would run a new phase under a dead context.
	// Root terminality is transitive here too: a reviewer pass is a NEW
	// phase on the root session, which must not open while any descendant
	// sub-agent or async command it owns is still live at any depth, or
	// while the root itself still owes (or is retrying) a reaction turn. The
	// gate reads the SAME CLIScope answer the CLI loop's own exit condition
	// uses: running work (a delegation armed for sess.ID stays 'running' until
	// the child's scope drains), an owed/paced/stuck reaction all block the
	// pass; debt the policy defers (bg-shell with auto-resume off, a pending
	// question) does not -- nothing will ever act on it, so waiting on it would
	// silently skip the reviewer forever. Every skip says so on stderr.
	//
	// C16 fix: the cheap, in-memory checks below run FIRST, and the scope read
	// is only consulted once they all already pass (it runs recovery writes and
	// exclusive lock probes), so runs with no reviewer configured never pay for
	// it; reviewerPassBlocked retries a DB read error with a pause.
	//
	// R3C-1: never inside a Drain iteration (its ctx is a Drain call: the review
	// turn would inherit it, find nothing to pull and come back as a "queued"
	// no-turn, dropping the run's real answer) and never when the `rush run`
	// loop runs the pass itself, once, at its scope-closed exit.
	reviewerCandidate := resultErr == nil && req.Credentials == nil &&
		!req.drainTurn && !req.deferReviewer &&
		!loop.canceledAfterCommit && loop.ctx.Err() == nil &&
		shouldRunReviewerPass(overrides.ModelRole, app.config.Config())
	if reviewerCandidate {
		if source, ok := app.AgentCoordinator.(agent.ReactionDebtSource); ok {
			if app.reviewerPassBlocked(ctx, source, sess.ID, stderr) {
				reviewerCandidate = false
			}
		}
	}
	if reviewerCandidate {
		primaryResult := result
		primaryFinalText := loop.finalText
		// Reviewer pass stall handling (app_run_reviewer_stall.go): each
		// attempt rebuilds the review turn fresh; a stalled attempt restarts
		// the pass, and an always-stalling reviewer ends on the review-failed
		// path with ErrReviewerUnresponsive.
		attempt := func() (*RunResult, error) {
			reviewRunFn, reviewCtx := app.buildReviewerPassTurn(ctx, setup.callOpts)
			loop.resetForReviewerPass(reviewCtx)
			return loop.runTurnPhase(app.reviewerTurnPrompt(reviewCtx, sess.ID, basis), reviewRunFn)
		}
		reviewResult, reviewErr := retryStalledReviewerPass(attempt)
		switch {
		case reviewErr == nil:
			// A10/C9-21: the executor's answer stays the run's final text
			// in every mode; the reviewer's verdict is additive.
			if primaryResult != nil && reviewResult != nil {
				app.attachReview(ctx, primaryResult, sess.ID, reviewResult.FinalText, stderr)
			} else if primaryResult == nil {
				// No envelope in terse/stream without captureResult: surface
				// the verdict's stderr line from a throwaway result. The
				// reviewer's own text is still loop.finalText here.
				reviewText := loop.finalText
				if reviewResult != nil {
					reviewText = reviewResult.FinalText
				}
				app.attachReview(ctx, &RunResult{}, sess.ID, reviewText, stderr)
			}
			loop.finalText = primaryFinalText
			result, resultErr = primaryResult, nil
		case errors.Is(reviewErr, ErrReviewerUnresponsive):
			// The reviewer stalled through every attempt: keep the
			// executor's answer and take the existing review-failed path.
			recordReviewFailure(primaryResult, stderr, reviewErr)
			loop.finalText = primaryFinalText
			result, resultErr = primaryResult, nil
		case reviewFailureKeepsPrimary(ctx, reviewErr):
			// A review turn that failed for a reason of its own (a provider
			// error, a timeout of the review turn) keeps the executor's
			// answer as the run's outcome; only a canceled run, a
			// fail-fast busy refusal or a queued review replace it.
			slog.Warn("reviewer pass failed", "session", sess.ID, "err", reviewErr)
			// primaryResult is nil in terse/stream mode without captureResult.
			recordReviewFailure(primaryResult, stderr, reviewErr)
			loop.finalText = primaryFinalText
			result, resultErr = primaryResult, nil
		default:
			result, resultErr = reviewResult, reviewErr
		}
	}
	// R5-3: terse output is published only now — after the gate has
	// decided which phase's result is the run's single final answer.
	loop.flushTerseOutput()
	return result, resultErr
}

// RunNonInteractive runs a single agent turn and writes its result to
// `output`. See RunMode for the available output shapes. It is a thin
// wrapper over ExecuteRun: it supplies the process streams as defaults
// and renders the JSON envelope for RunModeJSON.
func (app *App) RunNonInteractive(ctx context.Context, output io.Writer, prompt string, overrides RunOverrides, hideSpinner bool, mode RunMode, continueSessionID string, useLast bool) error {
	_, err := app.RunNonInteractiveWithResult(ctx, output, prompt, overrides, hideSpinner, mode, continueSessionID, useLast)
	return err
}

// RunNonInteractiveWithResult runs one agent turn, writes its output, and
// returns the structured result when one is available.
func (app *App) RunNonInteractiveWithResult(ctx context.Context, output io.Writer, prompt string, overrides RunOverrides, hideSpinner bool, mode RunMode, continueSessionID string, useLast bool) (*RunResult, error) {
	return app.runNonInteractiveWithAsyncResults(ctx, output, prompt, overrides, hideSpinner, mode, continueSessionID, useLast)
}
