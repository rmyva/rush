package agent

// R12-H2 chunk D, acceptance item 4 (design §3 row "Live attach").
//
// Production entry point exercised: BatchWorkIngress.Invoke (the ONE
// authority: batch_ingress.go Invoke → invokeOnce singleflight →
// invokeFresh durable ProbeLive attach → DeliverBatch) with the provider
// blocked INSIDE its WriteFile — the probe boundary is observed from the
// h2aGatingDisk's "entered" signal, which fires inside the provider call
// AFTER the gate is taken, so the live attach is probed while the physical
// provider I/O is genuinely in flight (review P1-7e: NOT while the leaf is
// merely parked before its provider entry).
//
// Oracles:
//  1. Identical hash only AFTER Validate normalizes: a retry invocation with
//     PADDED identity values and UNSET delivery/escape defaults hashes
//     differently pre-normalization and identically post-Validate.
//  2. The normalized-same retry (same call id, same input, same binding)
//     joins the in-flight execution: ONE provider invocation, ONE root.
//  3. A DIFFERENT registered binding on the same live call identity is
//     rejected (typed mismatch) with no additional provider write.
//
// Production mutation caught: hashing the invocation before Validate
// canonicalizes its defaults would split one logical call into two roots
// (double provider write); dropping the binding id from the canonical
// payload would let a mismatched binding attach to a live provider-bound
// root and reroute its writes to another provider instance.
//
// Housekeeping: the test releases the gate (the physical write lands), lets
// the root settle through the real engine, then releases the fixture's
// Construction owner and asserts no provider owner remains.

import (
	"context"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/stretchr/testify/require"
)

func TestLiveAttachBindingIdentity(t *testing.T) {
	env := newH2aGatedInstall(t)
	env.fake.arm() // hold the provider INSIDE WriteFile; the leaf runs straight to its provider op
	const callID = "call-attach-1"

	// Invocation A: explicit defaults, validated then hashed (the canonical
	// shape the runtime itself hashes after Validate).
	invA, callA, _, hashA := h2aGatedInvocation(t, env, env.bindingID, callID, batchDeliverySync)

	// Hash oracle: the SAME logical invocation, built with UNSET
	// delivery/escape defaults and PADDED (un-normalized) identity values,
	// must reproduce the attach hash ONLY after Validate canonicalises it.
	// The spec payload carries the identity values; Validate trims them and
	// fills the delivery defaults, and the production ingress hashes the
	// NORMALIZED spec (batch_ingress.go Invoke). Hashing a raw invocation —
	// or hashing before Validate — splits one logical call into two roots.
	invRaw := BatchInvocation{
		SessionID: "  " + env.sessionID + " ", MessageID: " m-1 ",
		RegistryID: " " + env.registryID + " ",
		Origin:     "coordinator", CallID: callID,
		Policy: batchLeafPolicyContext{
			RegistryID:        " " + env.registryID + " ",
			RestrictedToolset: " " + env.fingerprint + " ",
			CustomDisk:        " h2a ", DiskProvider: true,
			DiskBindingID: "  " + env.bindingID + " ",
		},
	}
	// Pre-normalization hash: the padded identity values serialize into the
	// payload verbatim, so it differs from the canonical hash.
	preInput, err := batchCanonicalInput(callA.Input)
	require.NoError(t, err)
	preSpec, err := batchOneItemSpec(invRaw, callA, preInput)
	require.NoError(t, err)
	require.NotEqual(t, hashA, batchInvocationHash(preSpec),
		"sanity: the un-normalized invocation must not yet carry the canonical hash")
	// Validate mutates its receiver, so preserve the padded invocation for
	// the retry and validate a separate copy for the local hash oracle.
	normalizedInv := invRaw
	require.NoError(t, normalizedInv.Validate())
	normSpec, err := batchOneItemSpec(normalizedInv, callA, preInput)
	require.NoError(t, err)
	require.Equal(t, hashA, batchInvocationHash(normSpec),
		"identical hashes only after Validate normalizes the identity/defaults")

	// First execution through the real ingress; the probe waits for the
	// provider's OWN "entered" signal — WriteFile has taken its gate and is
	// physically in flight — never merely the leaf having started.
	type invokeResult struct {
		resp fantasy.ToolResponse
		err  error
	}
	resp1Ch := make(chan invokeResult, 1)
	go func() {
		resp, err := env.install.Runtime.Invoke(context.Background(), invA, env.leaf, callA)
		resp1Ch <- invokeResult{resp: resp, err: err}
	}()
	select {
	case <-env.fake.enteredCh():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the provider write never entered the gated WriteFile through the real executor")
	}
	root, attached, err := env.install.Attach.ProbeLive(context.Background(), h2aProbe(env, hashA, callID))
	require.NoError(t, err)
	require.True(t, attached, "the live root attaches under the normalized hash")

	// Matching attach: the retry is the UN-normalized invocation above —
	// same call id, same binding, unset delivery defaults. Invoke re-runs
	// Validate, re-normalizes, re-hashes and must JOIN the in-flight
	// execution: one root, one leaf run, zero additional provider writes.
	retryInv, retryCall := invRaw, callA
	retryRespCh := make(chan invokeResult, 1)
	go func() {
		resp, err := env.install.Runtime.Invoke(context.Background(), retryInv, env.leaf, retryCall)
		retryRespCh <- invokeResult{resp: resp, err: err}
	}()
	entry := env.install.Runtime.entryFor(env.sessionID, callID)
	require.NotNil(t, entry, "first invocation remains in flight at provider gate")
	select {
	case <-entry.joined:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "matching retry never joined the in-flight invocation")
	}

	// Different REGISTERED binding on the same live call identity: rejected.
	other := newH2aGatingDisk()
	otherBinding, err := env.install.DiskBindings.RegisterProvider(other)
	require.NoError(t, err, "the conflicting binding is a separately registered binding")
	mismatchInv, mismatchCall, _, mismatchHash := h2aGatedInvocation(t, env, otherBinding, callID, batchDeliverySync)
	require.NotEqual(t, hashA, mismatchHash, "a different binding changes the canonical hash")
	mismatchErr := make(chan error, 1)
	go func() {
		_, err := env.install.Runtime.Invoke(context.Background(), mismatchInv, env.leaf, mismatchCall)
		mismatchErr <- err
	}()
	select {
	case err := <-mismatchErr:
		require.ErrorIs(t, err, errBatchInvocationMismatch,
			"a live call identity with a different binding hash is a typed mismatch")
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the mismatched retry must be rejected promptly")
	}

	// Physical return: the write lands once; both waiters get the ONE
	// response; exactly one root settles.
	env.fake.letGo()
	var first, retry invokeResult
	select {
	case first = <-resp1Ch:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "first caller delivery timed out")
	}
	select {
	case retry = <-retryRespCh:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "matching-attach joiner delivery timed out")
	}
	require.NoError(t, first.err)
	require.NoError(t, retry.err)
	require.Equal(t, first.resp, retry.resp, "the joiner receives the first caller's exact envelope")
	require.Equal(t, 1, env.leaf.runCount(), "matching attach must not re-submit: one leaf run")
	require.Equal(t, 1, env.fake.writeCount(), "one physical provider write")
	require.Zero(t, other.writeCount(), "the rejected binding never touched its provider")
	got, ok := env.fake.has(env.targetPath)
	require.True(t, ok)
	require.Equal(t, "lifecycle-data", got)
	require.Eventually(t, func() bool {
		states := h2aTaskStates(t, env.store, env.sessionID, root)
		return len(states) > 0 && !h2aAnyNonTerminal(states)
	}, 5*time.Second, time.Millisecond, "the ONE root settles")

	// REAL release trigger for the lingering Execution owner (R12-H2 §1.2):
	// the root settled, and the adopted Done watcher (batchRootLeaseStore
	// .adopt) calls the Runner's Release AUTOMATICALLY once Done closes —
	// no manual Runner.Release for the adopted root. The physical-Dispose
	// wrapper drops the Execution owner
	// (internal/jobbatch/runner_control.go).

	// Housekeeping: release the fixture's own Construction owner; no
	// provider owner remains.
	env.releaseConstruction()
	require.Eventually(t, func() bool {
		_, err := env.install.DiskBindings.Lookup(env.bindingID)
		return err != nil
	}, 5*time.Second, time.Millisecond, "binding unavailable after all owners end")
	require.Equal(t, 0, env.install.DiskBindings.ownerCount())
	require.Zero(t, env.fake.closeCount(), "provider Close is never called by Rush")
}
