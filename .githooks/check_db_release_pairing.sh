#!/usr/bin/env bash
# check_db_release_pairing.sh: guard against *_test.go files calling
# db.Connect()/db.ConnectRead() without pairing them with db.Release()
# in the same file -- the bug class behind the chronic ubuntu-latest
# CI kill documented in docs/lessons/2026-08-30-ci-goroutine-leak.md.
#
# Background: internal/db/connect.go's Connect/ConnectRead return
# handles from a process-wide REF-COUNTED pool (pool
# map[string]*connEntry, keyed by absolute database path). Every call
# increments the entry's refCount; only db.Release(dataDir) decrements
# it and (at zero) closes the underlying *sql.DB handles. A test that
# cleans up with conn.Close() on the raw *sql.DB compiles fine, looks
# correct, and is silently wrong: it bypasses the bookkeeping entirely,
# so the pool entry -- and, worse, its stdlib
# database/sql.(*DB).connectionOpener background goroutine, which only
# exits when the *sql.DB is closed via the correct API -- stays alive
# for the remaining life of the test binary. That leak was real and
# expensive: dozens of leaked connectionOpener goroutines across
# internal/agent's ~396 tests eventually starved GitHub hosted runners
# into killing the job (exit 143), a symptom that looked like pure
# runner flakiness for days. The compiler, go vet, and staticcheck all
# have no way to see the invariant -- it lives only in connect.go's doc
# comment -- so, exactly like the ASCII-SQL and stale-model-slot
# guards, a byte-level repo check is the only automated defense.
#
# Heuristic (same philosophy as this repo's other guards -- same-file
# occurrence counting, not static analysis): for every tracked
# *_test.go file that mentions db.Connect( or db.ConnectRead( at least
# once, count db.Release(, db.ReleaseAll( and db.ReleaseConn( in the same
# file, with full-line // comments stripped first. If the release count
# is lower than the connect count, the file is a violation. Disclosed
# imprecision, accepted deliberately:
#   - It counts text, not dataflow. The refCount contract is per CALL
#     (connect.go: "one Connect, one Release", twice for a
#     Connect+ConnectRead pair), so counting calls is the right
#     granularity; but a mention inside a string literal, a /* */
#     block, or a trailing comment after code on the same line counts
#     too (only full-line // comments are stripped). If that ever
#     flags a correct file, fix the wording or extend the per-file
#     allow-list below -- do not delete the guard.
#   - db.ReleaseAll( counts as a release. It forcibly closes the entry
#     regardless of refCount, which is leak-safe even though it is not
#     the contractually preferred pairing; it exists precisely for
#     tests whose cleanup cannot know the live refCount (App.Shutdown's
#     forced-shutdown path deliberately skips Release -- see its doc
#     comment). internal/server/handlers_test.go is a legitimate user.
#   - db.ResetPool() deliberately does NOT count: it tears down every
#     pooled entry process-wide, which this repo's own comments
#     (internal/cmd/providers_cmd_test.go and friends) treat as the
#     wrong cleanup under t.Parallel(). A file pairing Connect with
#     ResetPool-only cleanup will be flagged on purpose; switch it to
#     db.Release/db.ReleaseAll.
#   - Only *_test.go files are scanned, and only the literal `db.`
#     qualifier. Package-internal callers in internal/db itself use
#     bare Connect(...) and are invisible here; production files are
#     skipped because App.Shutdown intentionally never calls Release in
#     its normal path, so a production scan would flag by design, not
#     by defect.
#   - A leak created via an indirect helper (test file A calls a
#     helper in file B that Connects and Releases) is invisible to a
#     same-file count by construction: the helper file carries both
#     sides, so it pairs.
#   - db.ReleaseConn( releases one reference by connection identity and
#     counts just like db.Release(, without the path-generation ambiguity.
#
# Implementation note: the per-file loop runs only over files that
# mention db.Connect/db.ConnectRead (~40 today), never over every
# tracked file -- see check_stale_model_slots.sh for why a whole-repo
# per-file loop is untenable (2500+ files, over a minute). Listing
# candidates with one `git grep -l` keeps this well under a second.
set -uo pipefail

# Deliberately NOT `set -e`: this script is invoked two ways --
# directly by .githooks/pre-push (plain `bash
# .githooks/check_db_release_pairing.sh`, no -e) and by
# .github/workflows/ci.yml's pairing-guard step, whose `run:` block
# runs under GitHub Actions' default `bash --noprofile --norc -eo
# pipefail {0}` -- but that parent -e does not propagate into a fresh
# `bash <file>` subprocess. This file's own `set` line is the only
# thing that governs it. With -e, the grep pipelines below (grep exits
# 1 whenever it finds zero matches -- the PASS case for most files)
# would abort the script mid-loop; see check_stale_model_slots.sh for
# the longer version of this exact warning.

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	printf '\033[31mcheck_db_release_pairing.sh: not inside a git work tree; refusing to guess (run from a repository checkout)\033[0m\n' >&2
	exit 1
fi
cd "$(git rev-parse --show-toplevel)" || exit 1

# The candidate listing must distinguish "zero matches" (git grep exit
# 1 -- legitimate empty result, empty $conn_files, loop is a no-op)
# from "git grep itself failed" (exit > 1 -- e.g. broken repository or
# mangled paths in mixed Windows/WSL setups). The latter must fail
# closed instead of silently reporting a pass. The `||` form keeps
# status capture correct even if the script ever runs with -e set.
grep_status=0
conn_files="$(git grep -l -E 'db\.(Connect|ConnectRead)\(' -- '*_test.go')" || grep_status=$?
if [ "$grep_status" -gt 1 ]; then
	printf '\033[31mcheck_db_release_pairing.sh: git grep failed (exit %s) -- refusing to report a false pass\033[0m\n' "$grep_status" >&2
	exit 1
fi

violations=""
while IFS= read -r file; do
	[ -z "$file" ] && continue

	# Per-file allow-list, same spirit as check_stale_model_slots.sh's
	# stale-slot-ok marker: a file with a documented reason to be
	# exempt. Keep each entry annotated with that reason; a blanket
	# *.go exemption would also hide a genuine reintroduction in a
	# different file.
	case "$file" in
	internal/app/shutdown_result_test.go)
		# ShutdownWithResult (internal/app/app_lifecycle.go, a production
		# file this guard never scans) calls db.Release(app.dataDir)
		# internally, app.dbReleasesNeeded times, on the graceful path.
		# Two of this file's four db.Connect calls are paired by that
		# invisible internal call, not by a literal db.Release( in this
		# file -- same root cause as the helper-file gap this guard's own
		# doc comment documents, just with the "helper" being the method
		# under test. Verified balanced by hand: TestShutdownWithResult_
		# ForcedSkipsDBReleaseAndReportsForced's 2 Connects are cleared by
		# its own db.ReleaseAll; TestShutdownWithResult_
		# GracefulReleasesDBAndReportsNotForced's first Connect is
		# released internally by ShutdownWithResult, its second Connect
		# by the file's own db.Release.
		continue
		;;
	internal/app/app_broker_shutdown_test.go|\
	internal/app/app_heartbeat_test.go|\
	internal/app/app_run_workspace_test.go|\
	internal/app/app_new_skip_agent_setup_test.go|\
	internal/app/app_run_admission_race_test.go|\
	internal/app/app_finalization_e2e_harness_test.go|\
	internal/app/app_run_json_envelope_golden_test.go|\
	internal/app/app_run_lockbusy_firstturn_test.go|\
	internal/app/app_wake_schedule_wiring_test.go|\
	internal/app/async_phase4_recovery_scenarios_test.go|\
	internal/app/app_run_toolgate_restore_test.go|\
	internal/app/p348_p0_1_ordering_race_test.go|\
	internal/app/p348_p0_1_pump_coordinator_wiring_test.go|\
	internal/app/p350_coder_agent_selfheal_test.go|\
	internal/app/p421_p0_1_interrupt_live_continuation_test.go)
		# These tests hand their connection to App.Shutdown, whose production
		# lifecycle owns the matching db.Release call.
		continue
		;;
	esac

	# Strip full-line // comments before counting, so prose like
	# common_test.go's "(db.Connect(ctx, dbDir))" explainer does not
	# inflate the connect count. Trailing same-line comments and
	# /* */ blocks are accepted gaps (see above).
	code="$(grep -v '^[[:space:]]*//' "$file" 2>/dev/null || true)"

	connects="$(printf '%s\n' "$code" | grep -o -E 'db\.(Connect|ConnectRead)\(' | wc -l)"
	releases="$(printf '%s\n' "$code" | grep -o -E 'db\.(Release|ReleaseAll|ReleaseConn)\(' | wc -l)"

	if [ "$((releases))" -lt "$((connects))" ]; then
		violations="${violations}  ${file}: ${connects} db.Connect/ConnectRead call(s), ${releases} db.Release/ReleaseAll/ReleaseConn call(s)"$'\n'
	fi
done <<<"$conn_files"

if [ -n "$violations" ]; then
	printf '\033[31munpaired db.Connect/db.ConnectRead found (ref-counted pool contract, see internal/db/connect.go):\033[0m\n'
	printf '%s' "$violations"
	printf '\033[33mdb.Connect/db.ConnectRead increment a process-wide refCount that only db.Release(dataDir) decrements. Cleaning up with conn.Close() on the returned *sql.DB bypasses that bookkeeping and leaks the pool entry AND its database/sql connectionOpener goroutine for the life of the test binary -- this is the leak that killed ubuntu CI runners (docs/lessons/2026-08-30-ci-goroutine-leak.md). Pair every call with db.Release(dataDir) in the same file; db.ReleaseAll(dataDir) also counts, for forced-teardown cases. If a file is a genuine, documented exception, extend the allow-list in .githooks/check_db_release_pairing.sh -- do not just delete the call.\033[0m\n'
	exit 1
fi

exit 0
