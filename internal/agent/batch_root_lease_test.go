package agent

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
	"github.com/PHPCraftdream/rush/internal/jobbatch"
	"github.com/stretchr/testify/require"
)

// newRootLeaseTestManager pins one construction under registryID in caps,
// wires the identity-index drop hook and builds the root identity store.
// The returned release drops the construction's own owner.
func newRootLeaseTestManager(t *testing.T, caps *DiskProviderRegistry, registryID string) (*batchRootLeaseStore, *BatchLeafRegistryStore, func()) {
	t.Helper()
	registries := NewBatchLeafRegistryStore()
	caps.setRegistryDrop(registries.Delete)
	rootRegistry := &policyLeafRegistry{entries: map[string]policyLeafEntry{}}
	require.NoError(t, registries.Register(registryID, rootRegistry))
	require.NoError(t, caps.acquireConstruction("", registryID, "construction-"+registryID))
	store := newBatchRootLeaseStore(caps)
	return store, registries, func() {
		caps.release(diskOwnerKey{kind: diskOwnerConstruction, instance: "construction-" + registryID, registryID: registryID})
	}
}

// pinTestConstruction publishes the construction record for registryID and
// holds one Construction owner for the fixture's lifetime — the same thing
// the call/build scope of a real trusted call state does between the pin and
// the scope close. Fixtures that submit payloads naming registryID need it:
// the executor's Execution owner and the root lease fail closed when no
// construction record names the registry. The returned closure releases the
// owner explicitly (idempotent); the t.Cleanup release is the safety net for
// fixtures that never assert deletion.
func pinTestConstruction(t *testing.T, caps *DiskProviderRegistry, registryID string) func() {
	t.Helper()
	require.NoError(t, caps.acquireConstruction("", registryID, "construction-"+registryID))
	release := func() {
		caps.release(diskOwnerKey{kind: diskOwnerConstruction, instance: "construction-" + registryID, registryID: registryID})
	}
	t.Cleanup(release)
	return release
}

func registerTestBinding(t *testing.T, caps *DiskProviderRegistry, tag string) string {
	t.Helper()
	binding, err := caps.RegisterProvider(bindingTestNonComparable{DiskProvider: tools.OSDisk(), tags: []string{tag}})
	require.NoError(t, err)
	return binding
}

func TestBatchRootOwnerRejectedSubmitReleasesOnlyRootOwner(t *testing.T) {
	caps := NewDiskProviderRegistry()
	binding := registerTestBinding(t, caps, "provisional")
	store, registries, releaseConstruction := newRootLeaseTestManager(t, caps, "root-lease-provisional")
	root := jobbatch.ID("root-rejected")
	require.NoError(t, store.acquire(root, "provisional-owner", binding, "root-lease-provisional"))
	_, err := caps.Lookup(binding)
	require.NoError(t, err) // The Root owner holds the binding before Submit.
	store.release(root, binding, "root-lease-provisional")
	_, err = caps.Lookup(binding)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable) // Definite rejection drops the Root owner.
	_, err = registries.Resolve("root-lease-provisional")
	require.NoError(t, err) // The construction owner still holds the registry.
	releaseConstruction()
	_, err = registries.Resolve("root-lease-provisional")
	require.Error(t, err) // No owner names the registry any more: record deleted.
}

func TestBatchRootOwnerBridgesToSettlement(t *testing.T) {
	caps := NewDiskProviderRegistry()
	binding := registerTestBinding(t, caps, "root")
	store, registries, releaseConstruction := newRootLeaseTestManager(t, caps, "root-lease-accepted")
	root := jobbatch.ID("root-accepted")
	require.NoError(t, store.acquire(root, "accepted-owner", binding, "root-lease-accepted"))
	_, err := caps.Lookup(binding)
	require.NoError(t, err) // The accepted Root owner holds the binding.
	resolved, err := registries.Resolve("root-lease-accepted")
	require.NoError(t, err)
	require.NotNil(t, resolved)
	done := make(chan struct{})
	require.NoError(t, store.adopt(root, done, binding, "root-lease-accepted", nil))

	close(done) // Settlement: Done means no further child can start.
	require.Eventually(t, func() bool {
		_, lookupErr := caps.Lookup(binding)
		return errors.Is(lookupErr, ErrDiskProviderUnavailable)
	}, time.Second, time.Millisecond)
	_, err = registries.Resolve("root-lease-accepted")
	require.NoError(t, err) // The construction owner is independent of the root.
	releaseConstruction()
	_, err = registries.Resolve("root-lease-accepted")
	require.Error(t, err)
}

func TestBatchRootOwnerDuplicateAndMismatchAdoption(t *testing.T) {
	caps := NewDiskProviderRegistry()
	binding1 := registerTestBinding(t, caps, "one")
	binding2 := registerTestBinding(t, caps, "two")
	store, _, releaseConstruction := newRootLeaseTestManager(t, caps, "root-lease-duplicate")
	root := jobbatch.ID("root-shared")
	done := make(chan struct{})
	require.NoError(t, store.acquire(root, "dup-owner", binding1, "root-lease-duplicate"))
	require.NoError(t, store.adopt(root, done, binding1, "root-lease-duplicate", nil))
	// Repeated acquisition of the SAME key is idempotent (R12-H2 §1.1).
	require.NoError(t, store.acquire(root, "dup-owner", binding1, "root-lease-duplicate"))
	_, err := caps.Lookup(binding1)
	require.NoError(t, err)
	// A different binding for the same root is a mismatch: refused, and no
	// replacement root owner is acquired.
	require.ErrorIs(t, store.acquire(root, "dup-owner", binding2, "root-lease-duplicate"), ErrDiskProviderUnavailable)
	_, err = caps.Lookup(binding2)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable) // The mismatch took no owner.
	require.True(t, store.matches(root, binding1, "root-lease-duplicate"))
	close(done)
	require.Eventually(t, func() bool {
		_, lookupErr := caps.Lookup(binding1)
		return errors.Is(lookupErr, ErrDiskProviderUnavailable)
	}, time.Second, time.Millisecond)
	releaseConstruction()
}

func TestBatchExecutionOwnerIsRetainedUntilDispose(t *testing.T) {
	caps := NewDiskProviderRegistry()
	binding := registerTestBinding(t, caps, "execution")
	_, _, releaseConstruction := newRootLeaseTestManager(t, caps, "exec-lease")
	defer releaseConstruction()
	// Execution owner, acquired by batchExecutor.Start BEFORE side effects;
	// released ONLY by the physical-Dispose wrapper.
	execKey := diskOwnerKey{kind: diskOwnerExecution, instance: "exec-1", bindingID: binding, registryID: "exec-lease"}
	require.NoError(t, caps.acquire(execKey))
	var once bool
	wrapped := newBatchBindingExecution(testBatchLeaseExecution{}, func() {
		if once {
			return
		}
		once = true
		caps.release(execKey)
	})
	_, err := caps.Lookup(binding)
	require.NoError(t, err) // The execution owner holds the binding.
	require.NoError(t, wrapped.(jobbatch.Disposer).Dispose(context.Background()))
	_, err = caps.Lookup(binding)
	require.ErrorIs(t, err, ErrDiskProviderUnavailable) // The wrapper is the sole releaser.
}

type testBatchLeaseExecution struct{}

func (testBatchLeaseExecution) Stop(context.Context) error    { return nil }
func (testBatchLeaseExecution) Dispose(context.Context) error { return nil }

// TestBatchRootLeaseOwnerRecordedAndForgotten proves acquire records the
// owning session in the ledger, ownerForRoot reads it back, and forget
// clears the entry so the reader returns the empty string again.
//
// Revert-check: dropping the owner from the ledger entry (or reading it
// from a side index) starves the finalization holds provider of its only
// attribution source and fails every owner's seal closed.
func TestBatchRootLeaseOwnerRecordedAndForgotten(t *testing.T) {
	caps := NewDiskProviderRegistry()
	binding := registerTestBinding(t, caps, "owner-read")
	store, _, releaseConstruction := newRootLeaseTestManager(t, caps, "root-lease-owner-read")
	defer releaseConstruction()
	root := jobbatch.ID("root-owner-read")
	require.NoError(t, store.acquire(root, "reader-owner", binding, "root-lease-owner-read"))
	require.Equal(t, "reader-owner", store.ownerForRoot(root))
	store.forget(root)
	require.Equal(t, "", store.ownerForRoot(root))
}

// TestBatchRootLeaseOwnerConflictPreservesOriginalEntry proves a second
// acquire of the SAME identity under a DIFFERENT owner conflicts with the
// unavailable sentinel while the original ledger entry and its owner stay
// intact and matching still succeeds.
//
// Revert-check: letting a different owner overwrite or erase the recorded
// entry would re-attribute an accepted root to the wrong session and could
// seal while the first owner's root is still live.
func TestBatchRootLeaseOwnerConflictPreservesOriginalEntry(t *testing.T) {
	caps := NewDiskProviderRegistry()
	binding := registerTestBinding(t, caps, "owner-dup")
	store, _, releaseConstruction := newRootLeaseTestManager(t, caps, "root-lease-owner-dup")
	defer releaseConstruction()
	root := jobbatch.ID("root-owner-dup")
	require.NoError(t, store.acquire(root, "dup-owner", binding, "root-lease-owner-dup"))
	require.ErrorIs(t,
		store.acquire(root, "other-owner", binding, "root-lease-owner-dup"),
		ErrDiskProviderUnavailable)
	require.Equal(t, "dup-owner", store.ownerForRoot(root))
	require.True(t, store.matches(root, binding, "root-lease-owner-dup"))
}

// TestBatchRootLeaseAcquireRefusesEmptyOwner proves acquire fails closed on
// an empty owner for both the caps path and the owner-only path, and that
// no ledger entry is recorded for a refused acquire.
//
// Revert-check: an ownerless ledger entry is unattributable at seal time;
// recording it would silently downgrade the holds provider to a
// fail-closed refusal (or worse, a zero) for a live root.
func TestBatchRootLeaseAcquireRefusesEmptyOwner(t *testing.T) {
	caps := NewDiskProviderRegistry()
	binding := registerTestBinding(t, caps, "owner-empty")
	store, _, releaseConstruction := newRootLeaseTestManager(t, caps, "root-lease-owner-empty")
	defer releaseConstruction()
	root := jobbatch.ID("root-owner-empty")
	require.ErrorIs(t,
		store.acquire(root, "", binding, "root-lease-owner-empty"),
		ErrDiskProviderUnavailable)
	require.ErrorIs(t, store.acquire(root, "", "", ""), ErrDiskProviderUnavailable)
	require.Equal(t, "", store.ownerForRoot(root))
}

// TestBatchRootLeaseOwnerOnlyPathRecordsWithoutCapsOwner proves the
// owner-only path (empty binding AND registry, the CLI submit shape)
// records the ledger entry WITHOUT taking any capability owner, and that
// ownerForRoot returns the recorded owner.
//
// Revert-check: an owner-only entry that also acquired a caps owner would
// retain a disk binding no CLI root ever used; one that recorded no entry
// would leave CLI roots unattributable for finalization.
func TestBatchRootLeaseOwnerOnlyPathRecordsWithoutCapsOwner(t *testing.T) {
	caps := NewDiskProviderRegistry()
	store, _, releaseConstruction := newRootLeaseTestManager(t, caps, "root-lease-owner-only")
	defer releaseConstruction()
	ownersBefore, _ := caps.ownerCounts()
	root := jobbatch.ID("root-owner-only")
	require.NoError(t, store.acquire(root, "cli-owner", "", ""))
	ownersAfter, _ := caps.ownerCounts()
	require.Equal(t, ownersBefore, ownersAfter) // No capability owner was taken.
	require.Equal(t, "cli-owner", store.ownerForRoot(root))
	require.True(t, store.matches(root, "", ""))
}

var (
	_ jobbatch.Execution = testBatchLeaseExecution{}
	_ jobbatch.Disposer  = testBatchLeaseExecution{}
	_ tools.DiskProvider = (*bindingTestNilProvider)(nil)
	_ io.ReadCloser      = io.NopCloser(nil)
)
