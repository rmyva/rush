package app

// End-to-end regression tests for agent finalization (plan
// docs/plans/2026-11-13-finalization-s4-s7.md §5.5 and §10 W1/W2): the
// flag-ON tests pin the TARGET contract — a parent's answer stays
// provisional until descendant work finished, reaction debts settled and
// the candidate sealed — and are intentionally RED today because no
// production path binds the owner's finalization record for a fresh run
// (BindAgentFinalization has zero production call sites). That red is the
// P0 evidence for the W1 wiring; the flag-OFF test characterizes the
// legacy byte-for-byte behavior that must not change.

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// finalizationE2EOwnerRecord returns how many session_agent_finalizations
// rows the session owns and, when at least one exists, the phase of the
// first row. Queries run on application.DB() outside any transaction and
// only after the run finished: the database has a single open connection,
// so a transaction held while the app runs would deadlock it.
func finalizationE2EOwnerRecord(t *testing.T, application *App, sessionID string) (int, string) {
	t.Helper()
	var rows int
	require.NoError(t, application.DB().QueryRowContext(context.Background(),
		`SELECT count(*) FROM session_agent_finalizations WHERE session_id = ?`,
		sessionID).Scan(&rows))
	if rows == 0 {
		return 0, ""
	}
	var phase string
	require.NoError(t, application.DB().QueryRowContext(context.Background(),
		`SELECT phase FROM session_agent_finalizations WHERE session_id = ?`,
		sessionID).Scan(&phase))
	return rows, phase
}

// finalizationE2EDebtCount runs the two production debt predicates from
// internal/session/finalization_debt.go verbatim (the session_notices half
// and the batch_nodes half, parameterized by the owner session id) and
// returns their sum. The predicates are copied character-for-character so
// this assertion tracks exactly the debt the seal's scope reader sees:
// editing one copy but not the other must break this test, not hide the
// drift.
func finalizationE2EDebtCount(t *testing.T, application *App, sessionID string) int {
	t.Helper()
	predicates := []string{
		`SELECT count(*) FROM session_notices
WHERE owner = ? AND wake = 1 AND reacted = 0 AND delivery IN ('pending', 'done')
  AND delivery != 'void'`,
		`SELECT count(*) FROM batch_nodes n JOIN batch_nodes r ON r.node_id = n.root_id
WHERE n.owner_session_id = ? AND n.notice_selected = 1 AND r.announced = 1
  AND r.delivery != 'void'
  AND n.wake = 1 AND n.reacted = 0 AND n.delivery IN ('pending', 'done')
  AND n.delivery != 'void'`,
	}
	total := 0
	for _, predicate := range predicates {
		var n int
		require.NoError(t, application.DB().QueryRowContext(context.Background(),
			predicate, sessionID).Scan(&n), "debt query failed: %s", predicate)
		total += n
	}
	return total
}

// TestFinalizationE2EFlagOnHoldsRunUntilChildResultThenSeals pins plan §10
// W1 acceptance for the flag-ON in-process e2e: with
// options.finalization.enabled the parent's answer stays provisional while
// the child job runs, and the run may only finish after the child
// finished, the reaction debt settled and the recomposed candidate sealed
// as "accepted" — the terminal result is not delivered before the seal.
// It currently demonstrates the P0 binding gap: no production path binds
// the owner's finalization record for a fresh run, so the finalization
// layers are inert and this test is RED until that wiring lands. That red
// is the P0 evidence; the assertions below encode the target contract,
// never today's inert behavior. Reverting the finalization hold loop
// reproduces "run completed while the child job was still running", and
// the assertions on provisional-before-done, the single "accepted" seal
// row and the zero debt count fail.
func TestFinalizationE2EFlagOnHoldsRunUntilChildResultThenSeals(t *testing.T) {
	command, release := recoveryJobCommand(t)
	// The marker is COMPUTED by the shell, so its literal never appears in
	// the command text: the completion notice quotes the command, and
	// classifying on the literal "$((37+5))" would match the command
	// instead of the child's actual result.
	command += "; echo FN1-ESC-$((37+5))"
	finalBodies := make(chan string, 1)
	provisionalServed := make(chan struct{})
	var provisionalOnce sync.Once
	var releaseOnce sync.Once
	var releaseErr atomic.Pointer[error]
	var totalRequests, launches, startedYields atomic.Int32
	var provisionals, finals, duplicates, anomalies atomic.Int32
	var startedClosed atomic.Bool
	// fnE2EReleaseJob is goroutine-safe (pure file write, error recorded,
	// no require): callable from the provider handler goroutine and from
	// cleanup, exactly like the batch template's pnUnblock.
	fnE2EReleaseJob := func() {
		releaseOnce.Do(func() {
			err := os.WriteFile(release, nil, 0o644)
			releaseErr.Store(&err)
		})
	}
	// fnE2EServeProvisional answers one provisional-answer turn and only
	// then releases the child, so the completion can only arrive after the
	// provisional candidate exists — the hold under test is real.
	fnE2EServeProvisional := func(w http.ResponseWriter) {
		provisionals.Add(1)
		provisionalOnce.Do(func() { close(provisionalServed) })
		admissionWriteSSE(w, []string{
			admissionSSEText("prov", "FN1 provisional"),
			admissionSSEStop("prov", "stop"),
		})
		fnE2EReleaseJob()
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	application, sessionID := newFinalizationE2EApp(t, func(w http.ResponseWriter, r *http.Request) {
		totalRequests.Add(1)
		body, _ := io.ReadAll(r.Body)
		_, lastUser, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(lastUser, "Generate a concise title"):
			// Defensive: the seeded session has a non-default title, so no
			// background title generation should fire; answer anyway so a
			// surprise title request can never hang the run.
			admissionWriteSSE(w, []string{
				admissionSSEText("title", "FN e2e title"),
				admissionSSEStop("title", "stop"),
			})
		case strings.Contains(lastUser, "FN1-ESC-42"):
			// THE final recomposed request: it must carry the child's
			// result. A second one is a double delivery of that result —
			// fail loudly (400), never block.
			finals.Add(1)
			if finals.Load() > 1 {
				duplicates.Add(1)
				http.Error(w, "duplicate final request", http.StatusBadRequest)
				return
			}
			select {
			case finalBodies <- string(body):
			default:
				duplicates.Add(1)
			}
			admissionWriteSSE(w, []string{
				admissionSSEText("final", "FN1 final"),
				admissionSSEStop("final", "stop"),
			})
		case strings.Contains(lastUser, "call-fn1") && strings.Contains(lastUser, "finished"):
			// A completion WITHOUT the computed marker means the child's
			// result did not reach the model — the exact defect the target
			// contract forbids. Count the anomaly and still answer so the
			// run terminates instead of hanging the test.
			anomalies.Add(1)
			admissionWriteSSE(w, []string{
				admissionSSEText("nores", "missing result"),
				admissionSSEStop("nores", "stop"),
			})
		case strings.Contains(lastTool, "started"):
			// First started-carrier request: the detached job's started
			// yield. Later requests still carrying the started tool result
			// are the provisional-answer turn (the recompose rebuilds the
			// same history), not a second yield — this is why the yield is
			// served exactly once via the startedClosed swap.
			if !startedClosed.CompareAndSwap(false, true) {
				fnE2EServeProvisional(w)
				return
			}
			startedYields.Add(1)
			admissionWriteSSE(w, []string{
				admissionSSEText("yield", "FN1 job started"),
				admissionSSEStop("yield", "stop"),
			})
		default:
			if !startedClosed.Load() {
				launches.Add(1)
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("start", "call-fn1", "bash",
						`{"command":`+jsonString(command)+`,"description":"held job"}`),
					admissionSSEStop("start", "tool_calls"),
				})
				return
			}
			fnE2EServeProvisional(w)
		}
	}, true)
	done := make(chan struct{})
	var runErr error
	var result *RunResult
	go func() {
		defer close(done)
		result, runErr = application.RunNonInteractiveWithResult(ctx, io.Discard,
			"run held fn1 job", RunOverrides{Origin: message.OriginCLI}, true,
			RunModeJSON, sessionID, false)
	}()
	// Cleanup order: unblock, join the run goroutine bounded, check the
	// recorded release error, then shut the app down.
	t.Cleanup(func() {
		fnE2EReleaseJob()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		if p := releaseErr.Load(); p != nil {
			require.NoError(t, *p)
		}
		application.Shutdown()
	})
	// Sequencing step 1, the crisp P0 signature: the run must NOT finish
	// while the child job is still running — the provisional answer has to
	// be served first.
	select {
	case <-provisionalServed:
	case <-done:
		t.Fatalf("run completed while the child job was still running: " +
			"finalization did not hold the run (flag on)")
	case <-ctx.Done():
		t.Fatal("the provisional-answer request never arrived; the run " +
			"never recomposed after the started yield")
	}
	// Sequencing step 2: after the provisional answer and the release, the
	// run must finish within the remaining budget.
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the run did not finish within the budget after the " +
			"provisional answer was served")
	}
	require.NoError(t, runErr)
	require.NotNil(t, result)
	require.Equal(t, "FN1 final", result.FinalText,
		"the marker-carrying recomposed request must produce the final answer")
	require.EqualValues(t, 1, launches.Load(), "the job must be launched exactly once")
	require.EqualValues(t, 1, startedYields.Load(), "the started yield must be served exactly once")
	require.EqualValues(t, 1, provisionals.Load(), "exactly one provisional-answer turn is allowed")
	require.EqualValues(t, 1, finals.Load(), "exactly one final recomposed request is allowed")
	require.EqualValues(t, 0, duplicates.Load(),
		"a duplicate delivery of the child result must fail loudly, never pass silently")
	require.EqualValues(t, 0, anomalies.Load(),
		"the request that produces the final answer must carry the child result; "+
			"a completion without the marker means it did not reach the model")
	select {
	case finalBody := <-finalBodies:
		require.Equal(t, 1, strings.Count(finalBody, "FN1-ESC-42"),
			"the child result must reach the model exactly once inside one request")
	case <-ctx.Done():
		t.Fatal("the final request body was not recorded")
	}
	// Plan §5.5 bounds the provider budget: launch, started yield and one
	// finalizing turn are mandatory; whether the provisional answer is the
	// yield turn's own text or a separate provider request is left to the
	// L5 loop implementation, so 3 and 4 are both contract-conformant.
	// Anything above 4 means a recompose loop ran extra provider turns.
	total := totalRequests.Load()
	t.Logf("finalization e2e flag-on: provider saw %d requests", total)
	require.GreaterOrEqual(t, total, int32(3),
		"the provider saw fewer than launch + started yield + finalizing turn")
	require.LessOrEqual(t, total, int32(4),
		"the run exceeded the plan §5.5 budget: a recompose loop ran extra provider turns")
	// The seal is the observable contract: exactly one owner record, and
	// it reached the accepted phase only after the recomposed answer.
	rows, phase := finalizationE2EOwnerRecord(t, application, sessionID)
	require.EqualValues(t, 1, rows,
		"exactly one session_agent_finalizations row must exist for the owner")
	require.Equal(t, "accepted", phase,
		"the seal must record phase accepted, got %q", phase)
	require.Equal(t, 0, finalizationE2EDebtCount(t, application, sessionID),
		"the seal requires settled reaction debts: both production debt "+
			"predicates must count zero")
	require.Equal(t, 1, countBackgroundNotices(t, application, sessionID, "root"),
		"the child completion must be delivered to the model exactly once")
}

// TestFinalizationE2EFlagOnFastPathNoDescendantsSealsClean pins the fast
// path of plan §10 W1: with the flag ON but no descendant work the run
// must not stall and must not add provider requests — one request
// answers, and the candidate still seals as "accepted" with zero debt.
// Reverting the finalization seal path (or making the flag-ON gate stall
// childless runs) reproduces a missing session_agent_finalizations row or
// extra provider turns, and the assertions on the request count, the
// record count, the "accepted" phase and the zero debt count fail. RED
// today for the seal assertions: no production path binds the owner's
// record for a fresh run, which is the P0 evidence.
func TestFinalizationE2EFlagOnFastPathNoDescendantsSealsClean(t *testing.T) {
	var requests atomic.Int32
	application, sessionID := newFinalizationE2EApp(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		admissionWriteSSE(w, []string{
			admissionSSEText("done", "FN2 done"),
			admissionSSEStop("done", "stop"),
		})
	}, true)
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	result, err := application.RunNonInteractiveWithResult(ctx, io.Discard,
		"say done", RunOverrides{Origin: message.OriginCLI}, true,
		RunModeJSON, sessionID, false)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "FN2 done", result.FinalText)
	require.EqualValues(t, 1, requests.Load(),
		"fast path: with no children the flag must add neither a recompose "+
			"nor any extra provider turn")
	rows, phase := finalizationE2EOwnerRecord(t, application, sessionID)
	require.EqualValues(t, 1, rows,
		"exactly one session_agent_finalizations row must exist for the owner")
	require.Equal(t, "accepted", phase,
		"the seal must record phase accepted, got %q", phase)
	require.Equal(t, 0, finalizationE2EDebtCount(t, application, sessionID),
		"no descendants means no debt and a clean seal")
}

// TestFinalizationE2EFlagOffKeepsLegacyBehavior characterizes the flag-OFF
// contract (plan §6: with the flag off the behavior must be byte-for-byte
// main): the batch completion still reaches the model exactly once, and
// the legacy path must not create any session_agent_finalizations rows at
// all. Reverting the flag gate — wiring any finalization layer without the
// flag — reproduces stray finalization records on legacy runs, and the
// zero-rows assertion fails. Debt and reacted state are legacy
// reaction-chain behavior and deliberately not asserted here.
func TestFinalizationE2EFlagOffKeepsLegacyBehavior(t *testing.T) {
	command, release := recoveryJobCommand(t)
	// The marker is COMPUTED by the shell, so its literal never appears in
	// the command text: the completion notice quotes the command, and
	// counting a literal marker would see it twice (command + output).
	command += "; echo FN3-ESCAPED-$((40+2))"
	completionText := make(chan string, 1)
	started := make(chan struct{})
	var startedOnce sync.Once
	var releaseOnce sync.Once
	var releaseErr atomic.Pointer[error]
	var launches, completions, duplicates atomic.Int32
	var yielded sync.Once
	// fnE2EReleaseJob is goroutine-safe (pure file write, error recorded,
	// no require): callable from the seam and from cleanup.
	fnE2EReleaseJob := func() {
		releaseOnce.Do(func() {
			err := os.WriteFile(release, nil, 0o644)
			releaseErr.Store(&err)
		})
	}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	// Same seam as the batch template: release the held job at the FIRST
	// turn-done so the completion arrives deterministically afterwards.
	old := cliLoopTurnDoneSeam
	cliLoopTurnDoneSeam = func() {
		yielded.Do(func() {
			select {
			case <-started:
			case <-ctx.Done():
				return
			}
		})
		fnE2EReleaseJob()
	}
	defer func() { cliLoopTurnDoneSeam = old }()
	application, sessionID := newFinalizationE2EApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, lastUser, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(lastUser, "Generate a concise title"):
			admissionWriteSSE(w, []string{
				admissionSSEText("title", "FN3 title"),
				admissionSSEStop("title", "stop"),
			})
		case strings.Contains(lastUser, "call-fn3") && strings.Contains(lastUser, "finished"):
			completions.Add(1)
			// A duplicate completion request is a double-delivery
			// failure, not a stall: expose it, never block.
			select {
			case completionText <- lastUser:
			default:
				duplicates.Add(1)
			}
			admissionWriteSSE(w, []string{
				admissionSSEText("final", "FN3 final"),
				admissionSSEStop("final", "stop"),
			})
		case strings.Contains(lastTool, "started"):
			startedOnce.Do(func() { close(started) })
			admissionWriteSSE(w, []string{
				admissionSSEText("yield", "FN3 job started"),
				admissionSSEStop("yield", "stop"),
			})
		default:
			launches.Add(1)
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("start", "call-fn3", "bash",
					`{"command":`+jsonString(command)+`,"description":"held job"}`),
				admissionSSEStop("start", "tool_calls"),
			})
		}
	}, false)
	require.False(t, application.Config().Options.AgentFinalizationEnabled(),
		"the flag-off harness must leave finalization disabled")
	done := make(chan struct{})
	var runErr error
	var result *RunResult
	go func() {
		defer close(done)
		result, runErr = application.RunNonInteractiveWithResult(ctx, io.Discard,
			"run held fn3 job", RunOverrides{Origin: message.OriginCLI}, true,
			RunModeJSON, sessionID, false)
	}()
	// Cleanup order: unblock, join the run goroutine bounded, check the
	// recorded release error, then shut the app down.
	t.Cleanup(func() {
		fnE2EReleaseJob()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		if p := releaseErr.Load(); p != nil {
			require.NoError(t, *p)
		}
		application.Shutdown()
	})
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the flag-off held job run did not finish")
	}
	require.NoError(t, runErr)
	require.NotNil(t, result)
	require.Equal(t, "FN3 final", result.FinalText)
	require.EqualValues(t, 1, launches.Load(), "the job must be launched exactly once")
	require.EqualValues(t, 1, completions.Load(), "exactly one completion request is allowed")
	require.EqualValues(t, 0, duplicates.Load(),
		"a duplicate completion request is a double-delivery failure")
	select {
	case text := <-completionText:
		require.Equal(t, 1, strings.Count(text, "FN3-ESCAPED-42"),
			"the result must reach the model exactly once")
	case <-ctx.Done():
		t.Fatal("completion text not observed")
	}
	require.Equal(t, 1, countBackgroundNotices(t, application, sessionID, "root"))
	// The point of this test: the flag OFF path must not create
	// finalization records at all.
	rows, _ := finalizationE2EOwnerRecord(t, application, sessionID)
	require.Equal(t, 0, rows,
		"the flag OFF path must not create any session_agent_finalizations row")
}

// TestFinalizationE2EFlagOnCancelDuringWaitRefusesSeal pins plan §10 L6/W2
// cancel acceptance: cancelling the run while a descendant is still alive
// must end the run canceled within bounded time, refuse the seal and
// leave the owner's finalization record in phase "cancelled" — never
// "accepted". The "child result survives and is delivered on a resumed
// run" half of plan scenario 4 is deliberately not covered here: it needs
// a restart-of-run fixture and is documented in the lane report. RED
// today for the record assertions: no production path binds the owner's
// record for a fresh run, so the row does not exist yet — that is the P0
// evidence. Reverting the OnCancel hook (cancel without refusing the
// seal) reproduces a sealed candidate after cancellation, and the phase
// == "cancelled" assertion fails.
func TestFinalizationE2EFlagOnCancelDuringWaitRefusesSeal(t *testing.T) {
	command, release := recoveryJobCommand(t)
	// The shell-computed marker is not classified here; the command only
	// needs to stay alive until the release file appears.
	command += "; echo FN4-DONE"
	yieldServed := make(chan struct{})
	var yieldOnce sync.Once
	var releaseOnce sync.Once
	var releasesAttempted atomic.Int32
	var launches atomic.Int32
	// fnE2EReleaseJob is goroutine-safe (pure file write, no require):
	// safe from the test body after the run returned.
	fnE2EReleaseJob := func() {
		releasesAttempted.Add(1)
		releaseOnce.Do(func() { _ = os.WriteFile(release, nil, 0o644) })
	}
	application, sessionID := newFinalizationE2EApp(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, lastUser, lastTool := lastTurnParts(body)
		switch {
		case strings.Contains(lastUser, "Generate a concise title"):
			admissionWriteSSE(w, []string{
				admissionSSEText("title", "FN e2e title"),
				admissionSSEStop("title", "stop"),
			})
		case strings.Contains(lastTool, "started"):
			yieldOnce.Do(func() { close(yieldServed) })
			admissionWriteSSE(w, []string{
				admissionSSEText("yield", "FN4 job started"),
				admissionSSEStop("yield", "stop"),
			})
		default:
			// Only serve the launch once: a second launch request means
			// the batch runtime relaunched the job — fail loudly, never
			// block, and let the launches counter name the defect.
			if launches.Add(1) > 1 {
				http.Error(w, "relaunch observed", http.StatusBadRequest)
				return
			}
			admissionWriteSSE(w, []string{
				admissionSSEToolCall("start", "call-fn4", "bash",
					`{"command":`+jsonString(command)+`,"description":"held job"}`),
				admissionSSEStop("start", "tool_calls"),
			})
		}
	}, true)
	require.True(t, application.Config().Options.AgentFinalizationEnabled(),
		"the flag-on harness must leave finalization enabled")
	// Follow the cancel template: the run's ctx is the CLI's own external
	// safety valve (--timeout/Ctrl-C cancel exactly this ctx); the outer
	// watch ctx only bounds the test's selects.
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, cancelWatch := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancelWatch()
	runDone := make(chan struct{})
	var result *RunResult
	var runErr error
	go func() {
		defer close(runDone)
		result, runErr = application.RunNonInteractiveWithResult(runCtx, io.Discard,
			"start fn4 held job", RunOverrides{Origin: message.OriginCLI}, true,
			RunModeJSON, sessionID, false)
	}()
	select {
	case <-yieldServed:
	case <-ctx.Done():
		t.Fatal("the held job never produced its started yield")
	}
	// The parent's own turn finished and the run is now waiting on
	// descendant work: in target mode the finalization wait loop, today
	// the batch await — both are "waiting on a live child".
	require.Eventually(t, func() bool {
		return !application.AgentCoordinator.IsSessionBusy(sessionID)
	}, 30*time.Second, 20*time.Millisecond,
		"the parent's own turn must finish and the run must wait on the live child")
	require.EqualValues(t, 0, releasesAttempted.Load(),
		"the child job must not be released before the cancel")
	// The external safety valve: cancel exactly the run's ctx.
	cancel()
	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("RunNonInteractiveWithResult did not return after its ctx " +
			"was canceled")
	case <-ctx.Done():
		t.Fatal("RunNonInteractiveWithResult did not return within the " +
			"test budget after its ctx was canceled")
	}
	require.Error(t, runErr)
	require.NotNil(t, result)
	require.Equal(t, "canceled", result.ExitReason)
	require.EqualValues(t, 1, launches.Load(),
		"cancel during the wait must not relaunch the job")
	// Target assertions, RED today by design: the seal is REFUSED and the
	// record ends cancelled — the CHECK vocabulary spells it "cancelled".
	rows, phase := finalizationE2EOwnerRecord(t, application, sessionID)
	require.EqualValues(t, 1, rows,
		"cancel during the wait must leave exactly one owner finalization record")
	require.Equal(t, "cancelled", phase,
		"cancel during the wait must record phase cancelled, never accepted")
	// Cleanup: let the job's poll loop exit so no process outlives the
	// test holding the TempDir, sweep every mailbox, then rely on the
	// harness Shutdown. No assertions after the cancel beyond the ones
	// above.
	fnE2EReleaseJob()
	application.AgentCoordinator.CancelAll()
}
