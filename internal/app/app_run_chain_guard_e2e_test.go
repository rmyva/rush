// The reaction chain guard (#1113), end to end through the real `rush run`
// loop (design test 6): the first turn launches a HELD background job W plus
// two timers whose completions open the poll legs, and the model reacts to
// each timer's completion with a Drain leg that polls for W with a pure wait
// command (`echo tick-N`). While W is the session's own undelivered work the
// in-turn progress guard (§2.2 of
// docs/plans/2026-10-01-in-turn-progress-guard.md) refuses every one of
// those launches at once -- no workLedger.Start, no durable row in either
// authority, no #1113 claim
// -- so a leg that carries neither a claim nor progress is invisible to
// chainLink: no reaction chain forms, the "reaction chain stopped" line never
// appears, and the loop simply keeps waiting on W. Releasing W (once both
// legs are served) produces exactly one more Drain -- a real fact -- whose
// answer ends the run. The second test in this file keeps the old shape with
// no W at all: there the sleeps really run (ownWork is false and a leg's
// streak never reaches S), #1113 counts three links and stops the chain.
//
// ALL THREE commands -- W, timer 1, timer 2 -- are sentinel-held from the
// launch: none can complete until the fixture writes its release file, and
// each release file is written only after the provider handler has SERVED
// the request that the staging depends on. Staging by wall-clock stagger
// (`sleep 4` / `sleep 8`) plus a request-count release was the original
// defect: batch bash has a REAL 3s inline window (batch_delivery.go's
// deliverWithEscapeWindow, wired in batch_install_tools.go), the calls of
// one assistant message run sequentially, and production pulls completion
// notices before EVERY provider step (agent_turn_step.go's PrepareStep), so
// a time-based timer could complete while the launch turn's closing request
// was being prepared; that request answered unconditionally, the pull marked
// the completion reacted, and the counted request was never created -- the
// 90s deadline of shadow-pp-118511110443, reached before the retained
// no-root oracle ever ran.
//
// The provider handler is a phase machine whose every step routes on what
// the CURRENT request actually contains -- its LAST user message and its
// LAST tool result, never concatenated history, so each phase proves the
// intended completion was consumed in THIS request:
//
//	close (request 2): the launch turn's closing step. Its last user
//	  message must carry NO completion marker at all -- every job is still
//	  held, so a marker here would mean a held job completed unaided, a
//	  fixture violation. Served => release TIMER 1.
//	leg1-open:  last user message carries CG-TICK-1 => emit the refused poll.
//	leg1-close: last tool result is the executed "tick-1" (the poll may be
//	  elected inline: the guard's own-work half reads batch rows -- finding 4
//	  in .rush/stdin/jb-f6a3.findings.md) or the guard's refusal text
//	  => "waiting 1"; release TIMER 2.
//	leg2-open:  last user message carries CG-TICK-2 => emit the refused poll.
//	leg2-close: last tool result is "tick-2" or the refusal => "waiting 2";
//	  release W.
//	final:      last user message carries W-DONE => "final answer".
//
// Invariant making this deterministic: at most ONE job is un-held at any
// moment (each release happens in the handler after serving the preceding
// phase's request), so the marker in the request's LAST user message
// uniquely identifies the expected completion; any other shape fails fast
// with t.Errorf and a 400 -- never a silent wait.
//
// Revert-check: dropping ownWork from refuseWait (turn_progress_guard.go)
// lets the echo legs through -- each starts a detached job
// whose completion arrives as a USER notice carrying "tick-1"/"tick-2",
// which no closing phase accepts (a closing step only reads a TOOL result
// or a refusal), so the provider answers 400 and the test goes red. The
// hard ctx deadline plus the provider's 400 guarantee the test fails rather
// than hangs.
package app

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

type chainStderr struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *chainStderr) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *chainStderr) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// chainE2E runs one `rush run` scenario. withW: the first turn launches a
// background job that blocks until the test releases it and two timers whose
// completions open the poll legs -- ALL THREE sentinel-held; the gates in
// the provider handler release timer 1 after the launch turn's clean closing
// step, timer 2 after leg 1's reaction was served, and W after leg 2's, so
// the completions can never be drained together or consumed inside the
// launch turn. Without W, the inline window is disabled and the 6-second
// sleeps run for real while the chain forms.
func chainE2E(t *testing.T, withW bool) (app *App, sessionID string, res *RunResult, stderr *chainStderr, requests int32) {
	t.Helper()
	defer agent.SetDrainPacingForTest(100*time.Millisecond, 50*time.Millisecond, 0)()

	var requestsC atomic.Int32
	var wCmd, timer1Cmd, timer2Cmd, wRelease, timer1Release, timer2Release string
	var wGo, timer1Go, timer2Go sync.Once
	if withW {
		wCmd, wRelease = cgHeldCommand(t, cgWDone)
		timer1Cmd, timer1Release = cgHeldCommand(t, cgTick1)
		timer2Cmd, timer2Release = cgHeldCommand(t, cgTick2)
	}
	// The gate: one phase per expected provider request after the launch.
	// The handler runs on the HTTP server's goroutine, so the phase counter
	// is mutex-guarded and the releases never call require.
	var gate struct {
		mu    sync.Mutex
		phase int
	}
	var stderrBuf chainStderr
	restore := cliLoopStderr
	cliLoopStderr = &stderrBuf
	t.Cleanup(func() { cliLoopStderr = restore })

	application, sessionID := newAdmissionRaceApp(t, func(w http.ResponseWriter, r *http.Request) {
		n := int(requestsC.Add(1))
		if !withW {
			// No W: every sleep launches for real (ownWork is false and a
			// leg's streak never reaches S), so each leg's completion feeds the
			// next one and #1113 counts the chain. This scenario disables the
			// inline window. The 6s ticks give each drain several seconds of
			// slack before completion.
			switch {
			case n == 1:
				calls := []string{admissionSSEToolCall("t1", "call-tick1", "bash", `{"command":"sleep 6","run_in_background":true,"description":"tick"}`)}
				calls = append(calls, admissionSSEStop("turn", "tool_calls"))
				admissionWriteSSE(w, calls)
			case n == 2:
				admissionWriteSSE(w, []string{admissionSSEText("a", "first answer"), admissionSSEStop("a", "stop")})
			case n >= 3 && n <= 8:
				// Each chain Drain is two provider steps: one detached bash `sleep 6`
				// tool call, then a plain text step that ends the leg (the model
				// has nothing to say while waiting). n=3/4 -> tick2, 5/6 -> tick3,
				// 7/8 -> tick4.
				step := n - 2
				if step%2 == 1 {
					admissionWriteSSE(w, []string{
						admissionSSEToolCall("t", "call-tick"+itoa(int(step)/2+2), "bash", `{"command":"sleep 6","run_in_background":true,"description":"tick"}`),
						admissionSSEStop("t", "tool_calls"),
					})
					return
				}
				admissionWriteSSE(w, []string{admissionSSEText("w", "waiting "+itoa(int(step/2))), admissionSSEStop("w", "stop")})
			default:
				http.Error(w, "unexpected model call (n="+itoa(n)+"): the chain guard let another drain through", http.StatusBadRequest)
			}
			return
		}
		// W alive. The polls carry the tool's required `description` --
		// fantasy validates a call against the tool's schema BEFORE any
		// wrapper sees it (agent.go's validateToolCall), and a call it marks
		// Invalid never reaches the guard at all: its plain error result then
		// classifies as act, the streak never grows and the guard would never
		// refuse anything (the hang T5 hit first). Timer 2 and W are
		// sentinel-held, not timed: no release file is written before the
		// handler has served the phase that must precede their completion.
		switch n {
		case 1:
			calls := []string{
				sseToolCallIndex("w", "call-w", "bash",
					`{"command":`+jsonString(wCmd)+`,"run_in_background":true,"description":"held job"}`, 1),
				sseToolCallIndex("t1", "call-timer1", "bash",
					`{"command":`+jsonString(timer1Cmd)+`,"run_in_background":true,"description":"timer"}`, 2),
				sseToolCallIndex("t2", "call-timer2", "bash",
					`{"command":`+jsonString(timer2Cmd)+`,"run_in_background":true,"description":"timer"}`, 3),
			}
			calls = append(calls, admissionSSEStop("turn", "tool_calls"))
			admissionWriteSSE(w, calls)
		default:
			lastUser, allUsers, lastTool, err := cgRequestTexts(r)
			if err != nil {
				t.Errorf("chain gate: %v", err)
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			gate.mu.Lock()
			p := gate.phase
			accept := false
			switch p {
			case cgPhaseClose:
				// Launch turn's closing step. Every job is still held, so NO
				// completion may have been consumed anywhere yet -- check the
				// whole user history, not just the last message. A marker here
				// means a held job completed unaided: fixture violation.
				accept = !strings.Contains(allUsers, cgTick1) &&
					!strings.Contains(allUsers, cgTick2) &&
					!strings.Contains(allUsers, cgWDone)
			case cgPhaseLeg1Open:
				// Timer 1's completion notice is the CURRENT drain: it must be
				// the LAST user message of this request.
				accept = strings.Contains(lastUser, cgTick1)
			case cgPhaseLeg1Close:
				// Leg 1's poll outcome: executed inline (the poll may be elected
				// inline) or refused -- read from the LAST tool result only.
				accept = strings.Contains(lastTool, "tick-1") || strings.Contains(lastTool, cgRefused)
			case cgPhaseLeg2Open:
				accept = strings.Contains(lastUser, cgTick2)
			case cgPhaseLeg2Close:
				accept = strings.Contains(lastTool, "tick-2") || strings.Contains(lastTool, cgRefused)
			case cgPhaseFinal:
				// W's completion notice is the CURRENT drain's last user message.
				accept = strings.Contains(lastUser, cgWDone)
			}
			if !accept {
				gate.mu.Unlock()
				t.Errorf("chain gate: unexpected provider request at phase %d: lastUser=%q lastTool=%q -- the gate saw an event out of sequence or a completion was consumed by the wrong request",
					p, lastUser, lastTool)
				http.Error(w, "unexpected provider request (leg phase "+itoa(p)+"): the gate saw an event out of sequence", http.StatusBadRequest)
				return
			}
			gate.phase = p + 1
			gate.mu.Unlock()
			switch p {
			case cgPhaseClose:
				// Launch turn served clean; the GATE: only now may timer 1 complete.
				admissionWriteSSE(w, []string{admissionSSEText("a", "first answer"), admissionSSEStop("a", "stop")})
				cgReleaseFile(t, timer1Release, &timer1Go)
			case cgPhaseLeg1Open:
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("t", "call-tick1", "bash", `{"command":"echo tick-1","description":"tick"}`),
					admissionSSEStop("t", "tool_calls"),
				})
			case cgPhaseLeg1Close:
				// Leg 1 served; the GATE: only now may timer 2 complete.
				admissionWriteSSE(w, []string{admissionSSEText("w", "waiting 1"), admissionSSEStop("w", "stop")})
				cgReleaseFile(t, timer2Release, &timer2Go)
			case cgPhaseLeg2Open:
				admissionWriteSSE(w, []string{
					admissionSSEToolCall("t", "call-tick2", "bash", `{"command":"echo tick-2","description":"tick"}`),
					admissionSSEStop("t", "tool_calls"),
				})
			case cgPhaseLeg2Close:
				// Leg 2 served; the GATE: only now may W complete -- the
				// guard's verdict on both refused legs is already on record.
				admissionWriteSSE(w, []string{admissionSSEText("w", "waiting 2"), admissionSSEStop("w", "stop")})
				cgReleaseFile(t, wRelease, &wGo)
			case cgPhaseFinal:
				admissionWriteSSE(w, []string{admissionSSEText("f", "final answer"), admissionSSEStop("f", "stop")})
			}
		}
	})
	if !withW {
		require.True(t, agent.SetInlineWindowForTest(application.AgentCoordinator, false))
	}

	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	res, err := application.RunNonInteractiveWithResult(ctx, &stdout, "wait for the jobs",
		RunOverrides{Origin: message.OriginCLI}, true, RunModeJSON, sessionID, false)
	require.NoError(t, err)
	return application, sessionID, res, &stderrBuf, requestsC.Load()
}

func TestRunNonInteractive_ChainGuardStopsAfterThreeDrainsWaitsForWork(t *testing.T) {
	app, sessionID, res, stderr, requests := chainE2E(t, true)

	require.EqualValues(t, 7, requests, "first turn (2 steps) + 2 poll legs (2 steps each) + the W-fact drain")
	require.NotContains(t, stderr.String(), "reaction chain stopped",
		"the refused waits start no jobs, so #1113 never sees a chain")
	require.Empty(t, chainWarnings(res), "no chain stopped warning in the envelope either")
	// The in-turn guard refuses each poll: no job is ever created -- no batch
	// row in ANY state (terminal included). The guard's own-work half reads
	// batch rows (package O, PR #151), which is exactly what this strict
	// no-root oracle needs; the historical gap is recorded as finding 4
	// in .rush/stdin/jb-f6a3.findings.md.
	for _, callID := range []string{"call-tick1", "call-tick2"} {
		f6a3NoWorkForCall(t, app, sessionID, callID)
	}
	require.Equal(t, "final answer", res.FinalText, "the W completion's drain is the answer")
	require.Equal(t, "end_turn", res.ExitReason, "the guard never rewrites the exit reason")
	require.Eventually(t, func() bool {
		debt, ok := f6a3ReactionDebtExistsOK(t, app, sessionID)
		return ok && !debt
	}, 10*time.Second, 10*time.Millisecond, "the W drain reacted W")
}

func TestRunNonInteractive_ChainGuardExitsWithoutWork(t *testing.T) {
	app, sessionID, res, stderr, requests := chainE2E(t, false)

	require.EqualValues(t, 8, requests, "first turn (2 steps) + 3 chain drains (2 steps each), then the loop exits")
	require.Equal(t, 1, strings.Count(stderr.String(), "reaction chain stopped"), stderr.String())
	require.Len(t, chainWarnings(res), 1)
	require.Equal(t, "end_turn", res.ExitReason, "the exit reason is the last drain's own, untouched")
	require.Equal(t, "waiting 3", res.FinalText, "the last completed drain's text is the answer")
	open := f6a3ReactionDebtExists(t, app, sessionID)
	require.True(t, open, "the deferred debt stays for the next turn")
}

// chainWarnings filters the envelope's warnings down to the guard's own line:
// the runs' other diagnostics (bash tool notices etc.) are not this test's
// subject and vary between iterations.
func chainWarnings(res *RunResult) []string {
	var out []string
	for _, w := range res.Warnings {
		if strings.Contains(w, "reaction chain stopped") {
			out = append(out, w)
		}
	}
	return out
}

// sseToolCallIndex is admissionSSEToolCall with an explicit tool_calls index,
// so several calls can be streamed in one assistant message.
func sseToolCallIndex(id, callID, name, args string, index int) string {
	return fmt.Sprintf(`{"id":%q,"object":"chat.completion.chunk","created":1,"model":"probe","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":null}]}`, id, index, callID, name, args)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
