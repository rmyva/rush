# Lesson: tests that their author never ran fail for the same four reasons

## Symptom

During the agent-finalization sprint, delegated `/wcrush` lanes wrote code
and tests but, by rule, never ran `go test` (a test run on the shared
machine hangs it). The orchestrator ran the suites afterwards. Four of the
first eight lanes (L2b, L3, L4, L8) handed over tests that failed or hung
while the product code they covered was correct. Each author had "checked
every assertion line by line", which did not catch any of the four causes
below: reading a test is not running it.

## Causes (all reproduced)

1. **One connection, open transaction.** The test database
   (`db.Connect`) uses `SetMaxOpenConns(1)`. While a transaction from
   `BeginTx` is open, any use of the connection outside that transaction
   (`QueryRow`, `Exec`, a second `BeginTx`) blocks forever. A test that
   held a transaction and then read through `conn` hung for the whole
   package timeout. Fix: roll back or commit first, or read through the
   transaction itself.
2. **Ranging over a channel nobody closes.** A pump or wrapper stream
   closes its output only after its input closes. A test that sealed a
   hold and then collected the output with `range` never terminated
   because the input channel was still open. Fix: close the input before
   collecting.
3. **Raw `UPDATE` levers that violate a table CHECK.** `batch_nodes`
   allows `delivery = 'void'` only on a selected notice row
   (`notice_selected = 1` with every `notice_*` column set); an unselected
   row may only be `delivery = 'none'`, `wake = 0`. Forcing a state with a
   direct `UPDATE` must keep the CHECK satisfied.
4. **Unbounded waits.** A waiting test needs a real deadline (context
   timeout or `-timeout`), otherwise a product bug turns into a ten-minute
   hang instead of a failure.

A fifth, found by the same discipline of actually running the test, was a
product bug rather than a test bug: the SSE handler deduplicated live
events against a running high-water mark, which silently dropped a held
terminal event whose id was lower than an already delivered one.

## What to do

- When tests are written by someone who may not run them, put these four
  points in the task prompt and have the orchestrator run the suites
  serially (`go test -p 1 -parallel 1 -count=1 -timeout 300s -run ...`)
  before accepting the lane.
- Prefer `-timeout 300s` on every delegated run so a hang fails fast.
- Fix the failures where they arise; a green `go vet` only proves the
  tests compile.
