# MCP Invariant Registry

Task #902, stage 1.1 of `docs/plans/2026-09-09-mcp-consolidation-plan.md`.

Each row records one law the MCP lifecycle guard code protects — stated so it
survives a total rewrite of the mechanism, not the mechanism itself. Format:
`INV-NN | the law | where in code | proving test | commit that learned it`.

**Line numbers are as of worktree HEAD `2cf5f8379` (branch
`p902-invariant-registry`, 2026-09-09).** They will drift; re-anchor before
relying on them. Re-anchored 2026-09-09 (task #925) to the post-split layout:
`init.go` was split into ten same-package files, and each Where entry now names
its file. Test columns name test functions in
`internal/agent/tools/mcp/*_test.go`. Commit columns are short hashes;
see the "Commit → invariant" section below for what each commit taught.

Where a review document disagreed with the code as read at this HEAD, the row
says so. Verified staleness of the 2026-09-07 chunk-5 review: CH5-1, CH5-2 and
CH5-3 are all closed on this HEAD (by `00c9911f5`, `70c98f828`, `00c9911f5`
respectively); the rows below reflect the closed state.

## Invariants

| ID | The law | Where (file:line) | Proven by | Learned in |
|---|---|---|---|---|
| INV-01 | A published session's transport outlives every context involved in creating it — admission, caller, and the initialization timeout; their cancellation may abort only an unpublished candidate, never a published session. | `sessionContext` handoff session.go:532-684 (promote linearization session.go:612-641), `createSessionWithAdmission` session.go:690-721 + session.go:765 (timer disarmed on connect), session.go:786-787; promote call sites session.go:383, publish.go:199, addreplace.go:251, addreplace.go:634 | TestInitializePublishesHTTPSessionBeyondAdmission, TestPublishedSessionIgnoresCandidateCancellationUntilClose, TestSessionContextPromotionLinearizesCancellation, TestCommitRenewalPublishesOnlyAfterPromotion, TestOwnerCloseVsRenewalDoesNotPublishLateSession (init_test.go), TestSessionCancellationClassification (state_regression_test.go) | Broken by `01e909d0` (CR-1); law learned closing it: `5c8641e7`, `b7d8614f`; timeout leg `99371d14`; renewal leg `922316af` (CR-2) |
| INV-02 | Each MCP registry entry belongs to one active Owner bound to one ConfigStore. Installed and standalone owners with disjoint server names may coexist; replacing or closing one preserves the others' entries. An implicit owner is reclaimed only after its work drains. | `acquire`, `acquireStandalone`, `canReclaimLocked`, `clearRegistryEntriesLocked`, `hasForeignServerClaimLocked` (owner.go); store check (admission.go) | TestOwnerRejectsConfigStoreSwitch, TestEnableServerFromOldOwnerCannotPublishIntoNewOwner, TestAcquireReclaimsEmptyImplicitOwnerWorkers, TestConcurrentAcquireReclaimsImplicitOwnerOnce, TestOwnerClosePreservesOtherOwnersAcrossCloseOrders, TestInstalledClosePreservesStandaloneDisabledState, TestOwnerClosePreservesClaimedNoSessionStatesAcrossOrders | `5f2dbebc` (owner per application), `28ad297f` (idle-worker reclaim), `07a69d8f` (cross-store filtering); standalone close isolation |
| INV-03 | Work authorized by an admission dies the moment its owner, owner generation, or the server's epoch changes — and an epoch is bumped only after the mutation's durable write is known to have succeeded, so a failed mutation never invalidates a healthy published session. | `serverAdmission` admission.go:17-48, `validLocked` admission.go:209-217, `committedValidLocked` admission.go:232-265, bump admission.go:297-303 and `invalidateServerLocked` admission.go:522-530, `cancelServerCandidates` (no bump) admission.go:535-545 | TestFailedDisableDoesNotCancelStartupCandidate, TestFailedRemoveDoesNotCancelStartupCandidate, TestReplaceKnownCommitFencesRuntimeAfterEpochInvalidation, TestRetiredClientCannotPublishAfterReplacement, TestDisableServerCancelsBlockedInitAndLeavesNoLateSession, TestRemoveServerCancelsBlockedInitAndRejectsLateCommit | `963d584a` (admission model); defect from `cf12608e` fixed by `71df8f19` (failed replace keeps old admission) |
| INV-04 | Owner shutdown joins every admitted initialization, refresh worker, and session close before clearing only its owned process-wide registry entries — and an expired Close deadline leaves the owner fenced in closing state rather than letting old-owner callbacks touch the next owner's registry. | `finishClose`, `clearRegistryEntriesLocked`, `hasForeignServerClaimLocked`, `resetBrokerLocked`, `Close`, `trackSessionLocked` (owner.go) | TestOwnerCloseDeadlineRetainsFenceUntilStuckSessionCloses, TestOwnerCloseKeepsFenceWhileDetachedCloseBlocks, TestOwnerCloseResetsRegistryAndAllowsNextLifecycle, TestOwnerClosePreservesOtherOwnersAcrossCloseOrders, TestInstalledClosePreservesStandaloneDisabledState, TestOwnerClosePreservesClaimedNoSessionStatesAcrossOrders, TestOwnerCloseJoinsCanceledRuntimeMCPResolution, TestOwnerCloseCancelsBlockedStartupBeforeCleanup, TestTrackRejectsAdoptionAfterCloseBegins | `5c8641e7` (tracked lifecycle joins), `01e909d0` (deadline keeps fence, R17-3), `60c81f78` (cancel-then-sequential-close) |
| INV-05 | Every mutation of `Owner.committedAdmissions` happens under `lifecycleMu` — a per-server lease is not sufficient, because disable/remove of server A and publish/renew of server B touch the same plain Go map on different names. | `detachSessionLocked` owner.go:651-655 (lock taken inside), `detachSessionLifecycleLocked` owner.go:659-669; writes session.go:402, publish.go:227, addreplace.go:354/addreplace.go:370/addreplace.go:377; reader refresh.go:320 | TestCommittedAdmissionsDisableAndPublishCrossServer, TestCommittedAdmissionsDifferentNameDetachIsSynchronized, TestCommittedAdmissionsDetachStructuralOracle (committed_admissions_sync_test.go) | Introduced unsynchronized by `f2914d53c` (CH5-1, P0); law learned by `00c9911f5`; cross-server oracle `0175b1e0` (stage 0.2) |
| INV-06 | Lock discipline: server leases precede `lifecycleMu`, never the reverse, and cancellation runs outside lifecycle locks. Network I/O is outside both lock types. Durable config writes may intentionally hold the ordered server lease, but never `lifecycleMu`; final admission source validation is the narrow bounded filesystem-read exception allowed under `lifecycleMu` (admission.go:616-632; store_mcp_admission.go:108-130). Other filesystem reads/reloads run without either lock. | order comments owner.go:649-650, refresh.go:340-342; no-I/O lease.go:222-223, addreplace.go:273-277, uncertainty.go:42-47, uncertainty.go:70-72; cancel-outside session.go:201-206 | TestMutationPublicationTakesServerLeaseBeforeConfigLocks, TestDisableReleasesServerLeaseBeforeClosingDetachedTransport, TestSkippedTransitionReleasesLeaseBeforeBlockingRetirement, TestFailClosedMCPHonorsCallerCancellation, TestMCPAdmissionFinalTurnCancellationReleasesConfigLocks, TestReplacePersistRunsOutsideLifecycleLock, TestUncertaintyReloadRunsOutsideLifecycleLocks (no-I/O clause: lock-state probes at the replace-persist and uncertainty-reload seams, lock_discipline_test.go; scope limits in "NO-TEST findings") | `00c9911f5` (fixed detach order), `a470adf6` (moved transport closes outside the lease), `3e7f9df2`/era reviews (order verified) |
| INV-07 | Each server name has one lease instance that serializes replacement/close while readers share it; the registry refcounts identity so a waiter can never have its lease entry reclaimed and swapped underneath it (no ABA); multi-server operations take leases in sorted name order. | registry lease.go:14-158 (`getRetained` lease.go:110, `retain` lease.go:124, `release` lease.go:135), add retains entry across init addreplace.go:564-582, sorted multi-lock lease.go:277-293 | TestLeaseRegistryReclaimsChurnAndProtectsWaiterFromABA, TestAddServerRetainsLeaseAcrossRemoveDuringInitialization, TestSuccessfulAddRemoveReclaimsEveryLeaseReference, TestCanceledAddAfterRetainReclaimsUniqueLeaseReferences, TestAddRollbackConsumesItsLastLeaseReferenceOnce, TestReplaceServerAcquiresLeasesInSortedNameOrder (sorted-order clause, lock_discipline_test.go) | `92f2ee9a` (refcounted registry), `2c23fcd3` (ABA-safe `retain` bool) |
| INV-08 | For one server name, the durable config commit, the runtime swap, and the subscriber-visible events form one linearization point against concurrent remove/disable/add — a remove can never interleave between an Add's disk commit and its publication, and a same-name Add cannot start before subscribers have seen the deletion. | `publishPreparedClientLocked` contract publish.go:164-168, publish paths publish.go:70-113 / addreplace.go:649-711; remove publish-in-lease mutation.go:576-580; replace events-before-unlock addreplace.go:449-451 | TestAddLinearizesPublicationBeforeConcurrentRemoveAfterCommit, TestRemovePublishesBeforeBlockedCloseAndConcurrentAdd (zz_commit_outcome_test.go), TestRemoveDisabledFallbackPublishesOneStateEvent, TestStartFallbackDisabledPublishesBeforeLeaseUnlock, TestReplacePublishesBeforeBlockedCloseAndConcurrentRemove, TestStartFallbackDoesNotReplaceNewerSession (event_order_test.go) | `46f68de7` (commit+publish one lease turn), `49c92cb1` (remove event), `a62a37e6` (replacement events), `62371a33` (pinned against disk) |
| INV-09 | Replacement connects and promotes the new session before the durable rename commits, then switches config, session, advertised data, and state as one transition; any pre-commit failure leaves the old server exactly as it was. | `ReplaceServer` contract addreplace.go:77-81, flow addreplace.go:235-387 (promote addreplace.go:251-258, no-lifecycleMu persist addreplace.go:273-278, publication addreplace.go:322-387) | TestReplaceServerFailedSameNamePreservesLiveServerAndDisk, TestReplaceServerFailedRenamePreservesLiveServerAndDisk, TestReplaceServerSuccessfulSwapPublishesNewSessionOnce, TestReplaceServerSuccessfulRenameRemovesOnlyOldRuntimeState, TestReplaceServerPersistenceFailurePreservesOldAdmissionAndCallbacks (transactional_update_test.go) | `cf12608e` (transactional replace), `71df8f19` (admission survival), `14b8677d` (inactive replacements finalized) |
| INV-10 | A rename replacement is fenced on both identities — source and destination — so a concurrent change or uncertainty on either name invalidates the candidate before the durable commit. | `bindGuardLocked` admission.go:121-131, `replacementNamesValidLocked` admission.go:158-177, guard use addreplace.go:213-220 + addreplace.go:325-327 | TestReplaceServerConditionalTargetCollisionPreservesOldRuntime, TestReplacedRenameAdmissionTracksCommittedConfig, TestReplaceRejectsPendingAddDestinationBeforePreparation, TestReplaceRejectsDestinationFenceRaisedDuringPreparation | `59d1d8d9` (rename/rollback fences), `c7dfca65` (stale replacement results fenced) |
| INV-11 | An Add is a transaction: its prepared client is invisible (no session, tools, prompts, state, or events) until the durable outcome is known, and a rollback removes only that exact transaction — once a user mutation or durable commit has touched the name, rollback is a no-op. | `addTransaction` addreplace.go:13-29, staging publish.go:192-198, staged publish addreplace.go:704-711, `rollbackAddedServer` guards addreplace.go:752-755 + addreplace.go:786-815 | TestAddPrecommitFailureDoesNotPublishPreparedRuntime, TestTypedPrecommitOutcomeDoesNotCommitAdd, TestStaleAddRollbackPreservesNewerSameNameServer, TestPendingAddRemoveRejectsDurableCollision, TestAddCloseRollsBackCanceledPendingConfig | `9d1a6f2f` (transaction closes after durable outcome), `cf12608e`/`71df8f19` (transactional ground work) |
| INV-12 | Disable and remove persist before invalidating: only after the durable write is known-good may runtime state be torn down, so a failed persistence leaves the session enabled with no half-applied result — and for an in-flight pending Add, the disable/remove is one conditional durable write with no enabled window on disk. | persist-first mutation.go:132-134 + mutation.go:181-187, mutation.go:610-616; pending-add one-write mutation.go:134-140 + mutation.go:544-585 | TestPendingAddMutationCommitsCompleteConfigBeforeInvalidatingAdd, TestPendingAddRemoveUsesOneConditionalWriteWithoutEnabledWindow, TestPendingAddDisableUsesOneWriteAndPreservesConcurrentWriter, TestDisableServerCancelsBlockedInitAndLeavesNoLateSession, TestExternalDisableKeepsFullDefinitionAndUsesWorkspaceOverlay | `779e323b` (atomic pending disables), `950c1a04` (single conditional write) |
| INV-13 | A pre-launch Enable failure conditionally restores the prior durable state (generation/CAS) or fences the runtime if rollback cannot be reconciled without clobbering a concurrent writer. Once initialization launches asynchronously, a later connection failure cleans up that candidate/admission; it does not roll back the already committed enable mutation (mutation.go:487-491). | rollback machinery mutation.go:288-324, generation check mutation.go:346-351, uncertainty branches mutation.go:367-371 | TestEnableRollbackPreservesConcurrentDefinitionAndFencesStore, TestEnablePendingGlobalAddRollbackRestoresDisabledDefinition, TestEnableRollbackUncertaintySurvivesOwnerRolloverUntilExactReload | `6c15f330` (conditional rollback), `59d1d8d9` (rollback race fences) |
| INV-14 | A durable outcome that cannot be read back (maybe-committed, or committed-but-unreconciled) fences the runtime — detach, disable, mark uncertain — instead of guessing, inferring config, or starting a fallback. | `fenceMCPRuntimeLocked` uncertainty.go:208-222 (`MarkMCPUncertain` uncertainty.go:211), `commitOutcomeNeedsRuntimeFence` uncertainty.go:223-229, `ErrMCPConfigUncertain` uncertainty.go:11-13 | TestCommittedUnreconciledOutcomesFenceRuntime, TestMaybeCommittedAddFencesUntilReload … TestMaybeCommittedReplaceFencesUntilReload (zz_commit_outcome_test.go), TestUnreconciledCommitBlocksReadmissionUntilReloadReconciliation, TestDisableRechecksUncertaintyAfterWaitingForLease | `b639b317` (fence uncertain mutations), `c4440e60` (typed outcomes), `98f769e7` (reconcile uncertain commits) |
| INV-15 | The uncertainty fence lives in the config store, not the owner: it survives owner rollover and a reload of a different store, and is cleared only by a successful disk reload whose version check proves the fence was not raised while the read was in flight. | capture + version check uncertainty.go:29-68, admissions reject uncertain admission.go:294-296, init.go:292-301, lease.go:429-445 | TestMaybeCommittedFenceSurvivesOwnerRollover, TestWrongStoreReloadDoesNotClearMCPFence, TestReloadAndReconcilePreservesFenceRaisedDuringFinalize, TestEnableRollbackUncertaintySurvivesOwnerRolloverUntilExactReload (uncertainty_fence_test.go) | `da066d7f` (uncertainty moved out of the replaceable snapshot into the store — closed a real fence-loss hole) |
| INV-16 | A list-changed notification driven by an unpublished candidate session is deferred, attached to that exact candidate token (never inferred from the name), published only after that candidate commits, and discarded if the candidate dies. | `refreshRequest.deferUntilCommit/candidateToken` refresh.go:24-38, defer refresh.go:276-290, activation gate refresh.go:67-77, `activateRefreshesLocked` refresh.go:133-173, discard refresh.go:187-193 | TestCandidateListChangedIsRetainedUntilExactCommit, TestFailedCandidateListChangedDropsDeferredEvent, TestCandidateListChangedPublishesAfterCommitAndRefreshesNewSession, TestInitializeSingleDefersCandidateNotificationWithOldSameNameSession, TestReplacementCandidateListChangedUsesCommittedDestinationAdmission, TestInitializeSingleFailedCandidateDropsNotification | `6c2030dd` (defer until commit), `646b0f49` (promotion/notification linearization) |
| INV-17 | Refresh work is coalesced per (name, kind, epoch, candidate): at most one run in flight, later notifications mark it dirty for exactly one rerun, a saturated wake channel drops nothing because the pending map is the authoritative queue, and each rerun is re-admitted under the same epoch fencing as a session. | `enqueueRefreshLocked` refresh.go:100-119 (dirty marker refresh.go:114-117), `signalRefresh` refresh.go:121-128, `runRefresh` re-admission refresh.go:208-247, key refresh.go:195-206 | TestListChangedDuringRefreshSchedulesDirtyRerun, TestRefreshQueueSaturationCoalescesWithoutDropping, TestDirtyRefreshFailuresRetainCurrentSession, TestListChangedRefreshIsAsyncAndStaleAdmissionIsIgnored, TestExportedRefreshFailuresRetainSessionCountsAndRecover | `92f2ee9a` (refresh queue + worker), `2c23fcd3` (authoritative pending map after buffer-overflow loss), `5da43c1a` (exported refresh fenced) |
| INV-18 | State transitions are published only by the admission that owns the state token: suppressed or invalidated candidates publish nothing, and Starting is resolved to Error/Disabled only by its own admission — state never infers session identity from a previous state's Client field. | `admissionStateEventUnpinned` admission.go:797-813, suppress flags admission.go:39-40, `cleanupFailedAdmission` token ownership admission.go:817-853, `setState` admission.go:740-753 | TestStaleAdmissionCleanupLeavesNoOwnerlessStarting, TestInvalidCommittedNotificationClearsAllAdvertisedData, TestReplacePublishesStateEventsForResultBranches, TestInitializeSingleDisabledDetachesPublishedRuntime (event_order_test.go) | `92f2ee9a` (per-admission state), `1bf8bcdc` (removed the heuristic that inferred ownership from previous state) |
| INV-19 | At most one renewal of a server's session runs at a time: concurrent callers wait for the renewal winner instead of failing or renewing in parallel, and an in-flight operation pins its session generation so retirement cancels the operation's context while the transport survives until the last pinned operation releases. | single-flight lease.go:181-202, follower waits lease.go:511-527 + lease.go:609-654, ping-fail detach lease.go:671-688; operation pinning session.go:145-209 | TestGetOrRenewClientWaitsForDetachedRenewalWinner (renewal_followup_test.go), TestRenewalRetainsLeaseIdentityUntilAfterPublish, TestOperationLeaseDefersCloseUntilFinalReference, TestClientLeaseProtectsOperationFromRenewalAndClose, TestDisableCancelsRenewalAndReleasesFollowers, TestOperationLeaseRetiresBlockedCallWithoutWriterConvoy, TestRetiredSessionsCancelBeforeABlockedClose | `e3b79c04` (operation leases; closed the CR-4 reader convoy), `70c98f828` (restored follower wait after `3c3b7dde` had reordered it — CH5-2) |
| INV-20 | Every HTTP request of a session stays cancellable by the admission context that created it — including requests blocked before a response on Windows (explicit CancelRequest) and responses whose body is still being read (the fence follows the body until close/EOF). | `headerRoundTripper.RoundTrip` transport.go:236-300 (fence comment transport.go:272-277), `ownerResponseBody` transport.go:209-234, per-transport pool transport.go:46-63 | TestHeaderRoundTripperCancelsBlockedRequestBeforeResponse, TestHeaderRoundTripperKeepsOwnerCancellationUntilBodyClose, TestOwnerResponseBodyCloseCancelsBeforeUnderlyingClose, TestHeaderRoundTripperUsesOwnedTransportPool, TestCreateTransport_PreCanceledContextWinsFieldValidation | `92f2ee9a` (owned pool + explicit cancel), `2de9d044` (bounded admission I/O lifecycle), `49e008fa` (strict oracles) |
| INV-21 | A stdio MCP server's entire process tree dies with the session context (own process group, group kill) — not just the direct child — and the failure-diagnostic re-run of the command is bounded in both output bytes and time. | process-group comment + call transport.go:110-115 (`configureStdioProcess`), diagnostics transport.go:306-367 + transport.go:410-428, limits transport.go:24-26 | TestCreateTransport_StdioProcessGroup (process_unix_test.go), TestConfigureStdioProcess_CancelTreeKillsOrphanedGrandchild_Windows, TestStdioCheckBoundsDiagnosticOutput, TestStdioDiagnosticWriterLimitAndConcurrency | `b956dacb` (ported tree-kill; upstream `132a8c89`; 15+ production zombies motivated it), `c96eaa4d` (bounded diagnostics) |
| INV-22 | `WaitForInit` blocks only while a real full initialization is in flight: the barrier is created lazily, single-server operations never own it, it is reopened by a later full Initialize, and it is closed before the init wait group drains so waiters can never hang on a dying owner's channel. | lazy barrier init.go:42-60, close init.go:75-94, `WaitForInit` init.go:246-256, shutdown close owner.go:500 | TestWaitForInitIsImmediateWithoutFullInitialize, TestSequentialInitializeReopensInitBarrier, TestInitializeSingleDoesNotOwnInitBarrier, TestInitializeBarrierIsGenerationScopedAcrossRollover, TestInitializeClosesBarrierBeforeInitWaitGroup (init_test.go/state_regression_test.go) | `3199bf34` (reopen; CR-3 closure leg), `922316af` (lazy barrier + shutdown close) |
| INV-23 | The global-scope fallback for an unowned in-memory MCP definition is granted only to a registered in-flight Add transaction; every other unwritable-origin definition fails closed. | `resolveMCPMutationScope` mutation.go:414-433 (authorization mutation.go:425-432), `hasPendingGlobalAddFor` mutation.go:439-448, sole `markPendingGlobalAdd` call addreplace.go:556 | TestBlockedProjectInitializerIsNotPendingGlobalAdd, TestProjectMCPMutationsFailClosed, TestWorkspaceDisableEnableRemovePersistInWorkspaceFile, TestReplaceServerProjectOriginRejectsBeforeCandidateConnection | `95faff44` (restricted fallback to pending adds), `f11d8fed` (origin-scoped mutations) |
| INV-24 | A published session is retired only when its own effective definition diverges from what it was connected with — unrelated config writes, other servers' mutations, and no-op reloads must not disturb it. | `committedValidLocked` + `mcpConnectionConfigEqual` admission.go:232-271 (definition equality, not revision counters), `getOrRenewClientOnce` pre-ping fence lease.go:487-502 | TestPublishedHTTPNotificationSurvivesUnrelatedReload, TestGetOrRenewFencesStaleCommittedSessionBeforePing, TestReloadFencesPublishedSessionAfterRelevantMCPChange, TestRenewedSessionNotificationSurvivesConfigMutations, TestMCPAdmissionSurvivesUnrelatedCOWUpdate | `f2914d53c` ("preserve MCP session identity across revisions") |
| INV-25 | The source bytes a session is admitted from are captured under the server lease and revalidated at the final publication boundary; a no-op reload does not invalidate the candidate, but a semantic change to the definition or its resolved values fails the admission (stale) and the caller retries with a fresh disk snapshot a bounded number of times. | `withMCPAdmissionFinalTurn` admission.go:599-632, revision pinning admission.go:193-203, bounded retry admission.go:634-675 + admission.go:684-716 | TestMCPAdmissionFinalSourceLinearizationRejectsInPlaceChanges, TestMCPAdmissionFinalSourceLinearizationRejectsExpectedAbsentCreation, TestInitializeSlowCandidateSurvivesNoOpReload, TestReplaceAllowsNoOpReloadDuringPreparation, TestInitializeRejectsSemanticResolverChangeAndRetriesFreshCandidate, TestMCPAdmissionFinalTurnValidatesAfterLifecycleAcquisition, TestCreateTransport_PassesLifetimeContextToEveryRuntimeField, TestGetOrRenewClientBoundsCrossStoreChurn | `779802b2` (revisions; introduced CH5-3 over-fencing), `00c9911f5` (no-op vs semantic distinction — CH5-3 closed), `87de26a7` (revalidate all sources), `d2288666` (final source linearization) |

## Classification: S / M / X (stage 1.3)

Task #904. The question asked of every row: if the nine package globals of
`init.go:18-29` were fields of one `Owner` — only `currentOwner *Owner` and
its swap mutex left at package level, the 27 exported functions thin wrappers
over `currentOwner()` — could the law still be broken? If the only way to break
it required two owners sharing process-global state, it is **S** (structural:
the guard becomes unreachable and can be deleted). If it constrains ordering or
exclusion within a single owner, it is **M** (mechanical: the mechanism stays,
localized). **X** (speculative: no observable failure, no test) — none found,
consistent with stage 1.2's zero-vacuous result. Verdicts were decided by
reading the code each row points at, at HEAD `d181f3942`; tests staying green
proves nothing about a law that has become unreachable, so no verdict rests on
test outcomes. This section only labels laws; designing the target structure is
stage 2's job.

Five rows are mixed (one clause structural, one mechanical) and are split
rather than force-labelled: INV-02, INV-03, INV-04, INV-15, INV-22. "Structural
at" names the plan §2.3 migration step after which the S-law is unbreakable.

| ID | Class | Structural at (§2.3) | Why |
|---|---|---|---|
| INV-01 | M | — | The law binds the caller/admission/init-timeout contexts of a single owner's in-flight initialization (promote handoff session.go:612-641); no owner pair is involved, so the handoff mechanism must stay. |
| INV-02a | S | 3.8 (`owner`+`lifecycleMu`) | With per-Owner state an old owner's work can only touch its own fields, so "all state belongs to exactly one active Owner" holds by construction and the `owner == o` guards (admission.go:288, owner.go:356-358, owner.go:493) plus the cross-owner filtering rationale become dead. |
| INV-02b | M | — | An `Owner` still binds its ConfigStore lazily (`rememberConfig` owner.go:366-368; `Acquire()` takes no store) and the implicit owner must still be proven empty before replacement (`canReclaimLocked` owner.go:280-296, `acquire` owner.go:302-315) or an abandoned owner leaks live transports/processes — both ordering laws within one owner's lifetime. |
| INV-03a | S | 3.1+3.7 (`stateOwners`, `sessions`) | The `owner == a.owner` and generation legs of validity (admission.go:204-206, admission.go:242-244) exist only so a stale admission cannot reach a shared registry; owner-relative state makes them tautological (deletable at 3.8). |
| INV-03b | M | — | Epoch invalidation with bump-only-after-durable-write (admission.go:297-303) and cancel-without-bump (admission.go:535-545) is ordering among a single owner's own mutations. |
| INV-04a | M | — | Joining every admitted init, refresh worker and session close before reset (owner.go:448-490), plus the within-owner closing fence, is resource-lifecycle ordering inside the dying owner; removing it leaks transports/processes even with no second owner. |
| INV-04b | S | 3.7 (`sessions`) | The expired-deadline fence's stated purpose — old-owner callbacks must not mutate the next owner's registry (owner.go:421-427, `if owner == o` owner.go:493) — becomes unreachable when callbacks cannot address another owner's fields. |
| INV-05 | M | — | Disable/remove of server A racing publish/renew of server B hits the same `committedAdmissions` map of the SAME owner, so the exclusion survives per-Owner state untouched. |
| INV-06 | M | — | Lease-before-`lifecycleMu` order, no-I/O-under-lock and cancel-outside-lock are lock discipline inside one owner (owner.go:649-658, refresh.go:340-342); the migration neither enforces nor removes them. |
| INV-07 | M | — | Refcounted lease identity, ABA protection and (untested) sorted multi-lock order are mechanisms of one owner's registry with no cross-owner clause. |
| INV-08 | M | — | Commit/publish/events as one linearization point against same-name remove/add is ordering under one name's lease inside one owner. |
| INV-09 | M | — | Transactional replace (promote-before-commit, single transition, no half-applied failure) is a multi-step ordering law within one owner. |
| INV-10 | M | — | Dual-identity (source+destination) fencing guards one owner's rename against its own concurrent mutations. |
| INV-11 | M | — | Add-as-transaction with guarded rollback is staging and cleanup discipline within one owner. |
| INV-12 | M | — | Persist-before-invalidate and the pending-add one-write are durability ordering between one owner and its store, and the store is outside the migration's scope. |
| INV-13 | M | — | Conditional enable rollback (CAS-or-fence) arbitrates concurrent writers on one owner's store. |
| INV-14 | M | — | The unreadable-outcome fence acts through the ConfigStore (`MarkMCPUncertain` uncertainty.go:211), which stays; the fail-closed discipline is unchanged by ownership. |
| INV-15a | S | none — pre-existing | The fence already lives on the ConfigStore (`internal/config/mcp_uncertainty.go:88`), not in any of the nine globals, so "survives rollover" holds by placement today and after the migration; nothing becomes deletable — stage 2 must simply never move it into `Owner`. |
| INV-15b | M | — | The version-check clear discipline (capture before reload uncertainty.go:29-40; clear-only-versions-observed-before-the-read, `store_reload.go:356`/`587`) is a real ordering mechanism on the store and survives the migration untouched. |
| INV-16 | M | — | Candidate-token deferral, activation and discard are publication ordering within one owner. |
| INV-17 | M | — | Refresh coalescing, dirty-rerun and re-admission are mechanisms of one owner's queue and epochs. |
| INV-18 | M | — | State-event token ownership is exclusion among one owner's admissions. |
| INV-19 | M | — | Renewal single-flight and operation pinning are exclusion/ordering within one owner's sessions. |
| INV-20 | M | — | HTTP cancellation fencing is transport-lifetime binding within one admission; owners are irrelevant to it. |
| INV-21 | M | — | Stdio process-group kill and bounded diagnostics are transport-level laws with no owner dimension at all. |
| INV-22a | M | — | Lazy barrier creation, reopen per full init, and close-before-initWG-drain (init.go:42-94, owner.go:500) are close-ordering inside one owner. |
| INV-22b | S | 3.3 (`initDone`) | The barrier channel is already per-owner (`o.initDone`); its cross-rollover leg is carried only by the package mirror (`initDone = o.initDone` owner.go:343/init.go:54, read by `WaitForInit` init.go:248, reset owner.go:498), which dies when `initDone` moves. |
| INV-23 | M | — | Granting the global-scope fallback only to a registered pending Add (`resolveMCPMutationScope` mutation.go:414-433) is an authorization law inside one owner's mutation flow; ownership placement neither enforces nor removes it. |
| INV-24 | M | — | Retire-on-definition-divergence (`mcpConnectionConfigEqual` admission.go:232-271) judges one owner's published session against its own store; unrelated-write tolerance is within-owner semantics, not cross-owner isolation. |
| INV-25 | M | — | Source capture under the lease, final-turn revalidation and bounded retry (admission.go:599-632, admission.go:634-716) order one owner's admission against the store and disk, which the migration does not touch. |

### Summary

| Class | Count | IDs |
|---|---|---|
| S | 5 | INV-02a, INV-03a, INV-04b, INV-15a (pre-existing), INV-22b |
| M | 25 | 20 whole rows (01, 05-14, 16-21, 23-25) + halves 02b, 03b, 04a, 15b, 22a |
| X | 0 | — |

X is empty and that is the finding, not an omission: stage 1.2 found zero
vacuous tests, and the two NO-TEST clauses (INV-06 no-I/O-under-lock, INV-07
sorted lease order) are clauses of M laws, not speculative guards.

S-invariants in the order the migration makes them structural (plan §2.3 step
order):

1. INV-22b — step 3.3 (`initDone`).
2. INV-03a — steps 3.1+3.7 (`stateOwners`, `sessions`); guard text deletable
   at 3.8.
3. INV-04b — step 3.7 (`sessions`).
4. INV-02a — step 3.8 (`owner` + `lifecycleMu`, last).
5. INV-15a — no step: pre-existing structural law (fence on the store);
   migration-neutral.

**M > 10 — the stage-3 `init.go` < 2000-line target must be revisited.** The
plan expected M 5-8 with S the majority; the code says the opposite: 20 of 25
laws are wholly mechanical and 5 more carry a mechanical half. The lease
registry, admissions, transactions, refresh queue, promote handoff, transport
fences and the rest of the M machinery are the bulk of the ~5800 added lines,
and the ownership migration simplifies them (drops cross-owner branches) but
deletes none of them; the only guard code the migration removes is the small S
set above. "< 2000 = upstream 1391 + an honest allowance" was priced for an
S-majority registry, so stage 2 should re-derive the line budget by counting
which guard bodies implement S-laws versus M-laws before committing to a
number.

Findings about the plan's assumed structure, for stage 2 to resolve (not
redesigned here):

- The shared-state set is 12 package globals, not nine: `allTools`
  (tools.go:29), `allPrompts` (prompts.go:16) and `allResources`
  (resources.go:21) are consulted by `canReclaimLocked` (owner.go:284-286),
  cleared by `resetRegistryLocked` (owner.go:516-524) and read by the exported
  getters. No S-law above is fully unbreakable until these three move too.
- INV-02a's isolation payoff holds only while consumers route through their
  acquired `*Owner`. Under thin wrappers over `currentOwner()`, a second
  ConfigStore in one process (the SDK multi-App case that motivated
  `07a69d8f`) still reaches the current owner's registry through name-only
  getters, so the `IsConfigured` per-store filtering convention
  (owner.go:611-622) may remain necessary even after the migration.
- Step 3.2 (`broker`) carries no S-conversion: the broker is already shut down
  and recreated at registry reset (owner.go:526-527), so no invariant depends
  on cross-rollover event continuity.

## Test → invariant map

Task #903, stage 1.2. One row per test function in
`internal/agent/tools/mcp/*_test.go` (as of worktree HEAD `b35c13ab6`, branch
`p903-test-invariant-map`). Verdicts:

- **INV-NN** — the test would fail if that invariant were violated. The first
  ID is the primary law; parenthesised IDs are secondary laws the same test pins.
- **MACHINERY** — no registry law behind it. None of these are fence
  bookkeeping; they are transport/tool feature pins that do not touch the nine
  package globals stage 3 migrates, so they are stage-3 neutral.
- **VACUOUS** — would pass with the behaviour it names removed. **None found**
  (method below).
- Two functions named `Test*` are subprocess helpers, not tests; they are
  listed for completeness and excluded from all counts.

### Summary

| Verdict | Count |
|---|---|
| INV-NN (one primary per test) | 219 |
| MACHINERY | 10 |
| VACUOUS | 0 |
| Real tests | 229 |
| Helpers named `Test*` (not tests) | 2 |

Count vs baseline (230 via `go test -list '.*'` on Windows, see
`docs/plans/mcp-consolidation-baseline.md`): 230 = 229 real tests + 2 helpers.
The helpers are `TestMCPStdioHelper` (state_regression_test.go:1866) and
`TestMCPStdioDiagnosticHelper` (stdio_diagnostic_test.go:24) — subprocess
helpers selected by env var; when the suite runs them as tests they exit
immediately and pass. `TestMain` (process_windows_test.go) is not listed by
`go test -list`. Build-tagged files swap one test per platform
(`TestCreateTransport_StdioProcessGroup` is `!windows`,
`TestConfigureStdioProcess_CancelTreeKillsOrphanedGrandchild_Windows` is
`windows`), so the count is identical on this Windows baseline.

**Invariants with zero non-vacuous tests: none.** Every INV-01..INV-25 has at
least one INV-verdict oracle. The two clause-level gaps stage 1.1 recorded stand
unchanged and are the complete 1.4 backlog: INV-06 "no I/O under
`lifecycleMu`/server lease", INV-07 "multi-server leases in sorted name order".

How verdicts were determined (reading only — no test was run, per task
constraints): every test was read in full including setup and helpers; a test
counts as INV-pinning only if an asserted postcondition would observably differ
if the law's guard were removed. Where that judgment needed production code, the
code was read: `startFallback`'s `reflect.DeepEqual` staleness guard
(mutation.go:793) is what makes the `fallback_race_test.go` pair fail if removed;
`SetSkipPermissionRequests` (internal/config/store.go:354) performs a real
store COW write, so `TestMCPAdmissionSurvivesUnrelatedCOWUpdate` genuinely
discriminates definition-equality from snapshot identity. Caveats that are
flake/scope risks, not vacuity: four tests use timing-based negative windows
(100 ms polls, 25–50 ms sleeps); the three `committedAdmissions` oracles are
`-race` oracles and are only fully load-bearing under `-race`, which every
stage-3 step already mandates.

Names that promise more than their assertions pin (each still a real,
non-vacuous test — read the assertion, not the name, before leaning on it in
stage 3):

- `TestHeaderRoundTripperUsesOwnedTransportPool` — asserts only pointer
  inequality of two cloned transports; INV-20's behavioural follow-through is
  pinned by its siblings.
- `TestInitialize_RestrictToCLIEnabled` — infers "connection attempted"
  indirectly (`State != Disabled` against a command that is not an MCP server).
- `TestMCPAdmissionSurvivesUnrelatedCOWUpdate` — a single `admission.valid()`
  call; the discrimination only works together with the negative
  store-generation siblings.
- `TestOwnerCloserUsesOneWorkerForBlockedRetirements` — the drain assertion is
  INV-04; the "exactly one worker" half pins closer-worker bookkeeping and will
  need rewriting if stage 3 reshapes the closer, with no law lost.

Seam-coupled oracles (stage-3 caution — these are INV-verdicts, not MACHINERY):
many tests are wired through production test seams — `serverLeaseHooks`,
`mcpInitTestHooks`, `addAdmissionHooks`, `resourcesBeforePublishHook`,
`mcpReloadAfterSuccessHook`, and the `renewalWaitHook` / `renewalBeginHook` /
`renewalPublishHook` / `renewalEndHook` fields on `serverLease`. If stage 3
moves or removes the seams while migrating `leases`/`owner`, those tests stop
compiling; that is a mechanical rewrite, never bookable as lost coverage. The
deepest mechanism-coupled oracles: `TestRenewalRetainsLeaseIdentityUntilAfterPublish`
(asserts exact lease refcounts 3→3→2→1 through renewal hooks) and
`TestCanceledAddAfterRetainReclaimsUniqueLeaseReferences` (256 iterations via
`addAdmissionHooks`).

### The map

| Test function | File | Verdict | Notes |
|---|---|---|---|
| TestEnsureRawBytes | tools_test.go | MACHINERY | base64-decode heuristic for tool output; table-driven, non-vacuous; stage-3 neutral |
| TestFilterTools | tools_test.go | MACHINERY | enabled/disabled tool filtering unit; non-vacuous; stage-3 neutral |
| TestCreateTransport_StdioProcessGroup | process_unix_test.go | INV-21 | pins Setpgid + Cancel attributes; `!windows` build tag |
| TestConfigureStdioProcess_CancelTreeKillsOrphanedGrandchild_Windows | process_windows_test.go | INV-21 | behavioural tree-kill oracle: grandchild PID reaped after cancel |
| TestStartFallbackDoesNotReplaceNewerSession | fallback_race_test.go | INV-08 | fails without the DeepEqual staleness guard at mutation.go:793 (verified against production) |
| TestStartFallbackLetsQueuedMutationWinBeforeStaleResult | fallback_race_test.go | INV-08 | queued same-name mutation under the lease wins over stale fallback |
| TestAcquireReclaimsEmptyImplicitOwnerWorkers | implicit_owner_reclaim_test.go | INV-02 (INV-04) | implicit owner's workers joined before Acquire returns |
| TestConcurrentAcquireReclaimsImplicitOwnerOnce | implicit_owner_reclaim_test.go | INV-02 | 8 contenders: exactly one winner, rest ErrOwnerBusy |
| TestCanceledAddAfterRetainReclaimsUniqueLeaseReferences | lease_followup_test.go | INV-07 | 256 canceled adds leave zero lease refs; addAdmissionHooks seam |
| TestRetireMCPClientQueuesCloseAfterFinalRelease | lease_followup_test.go | INV-19 | close deferred behind pinned operation; exactly one close |
| TestCommittedAdmissionsDifferentNameDetachIsSynchronized | committed_admissions_sync_test.go | INV-05 | -race oracle: 2000 iters, per-lease detach vs lifecycleMu writer |
| TestCommittedAdmissionsDetachStructuralOracle | committed_admissions_sync_test.go | INV-05 | 10000 iters; also pins that detach removes the admission (map empty) |
| TestCommittedAdmissionsDisableAndPublishCrossServer | committed_admissions_sync_test.go | INV-05 | 3-way cross-server oracle; final identity/absence/length asserted |
| TestMCPAdmissionFinalSourceLinearizationRejectsInPlaceChanges | admission_final_source_linearization_test.go | INV-25 | same-size in-place source rewrite rejected at final turn; unchanged-source control |
| TestMCPAdmissionFinalSourceLinearizationRejectsExpectedAbsentCreation | admission_final_source_linearization_test.go | INV-25 | absent source created mid-admission rejected |
| TestGetOrRenewClientWaitsForDetachedRenewalWinner | renewal_followup_test.go | INV-19 | follower waits on renewDone (winner + canceled-waiter); old session detached pre-publish |
| TestUnreconciledCommitBlocksReadmissionUntilReloadReconciliation | uncertainty_fence_test.go | INV-14 (INV-15) | admission rejected until reload clears the fence; failed reload keeps it |
| TestAddPrecommitFailureDoesNotPublishPreparedRuntime | uncertainty_fence_test.go | INV-11 | prepared client invisible after failed precommit (no session/state/disk) |
| TestReloadAndReconcilePreservesFenceRaisedDuringFinalize | uncertainty_fence_test.go | INV-15 | fence raised during reload finalize survives via version check |
| TestMaybeCommittedFenceSurvivesOwnerRollover | uncertainty_fence_test.go | INV-15 | fence survives owner rollover; reload clears it |
| TestWrongStoreReloadDoesNotClearMCPFence | uncertainty_fence_test.go | INV-15 | reload of a different store leaves the fence intact |
| TestOwnerRejectsConfigStoreSwitch | uncertainty_fence_test.go | INV-02 | ErrMCPConfigStoreBusy for a foreign store on mutation and admission paths |
| TestStdioDiagnosticCommandPreservesStartupAttributes | stdio_diagnostic_test.go | INV-21 | diagnostic re-run keeps env/dir; output joined into the error |
| TestStdioCheckDoesNotDuplicateArgv0 | stdio_diagnostic_test.go | INV-21 | argc==3 pinned; no duplicated argv0 |
| TestStdioDiagnosticCancellationSettlesProcess | stdio_diagnostic_test.go | INV-21 | cancel settles the process and the stdout reader |
| TestStdioDiagnosticWriterLimitAndConcurrency | stdio_diagnostic_test.go | INV-21 | bounded writer: exact/limit+1 semantics; concurrent writes safe |
| TestStdioCheckBoundsDiagnosticOutput | stdio_diagnostic_test.go | INV-21 | output bounded through stdioCheck; ErrStdioDiagnosticTooLarge only past limit |
| TestMaybeStdioErrKeepsEOFAndJoinsDiagnostic | stdio_diagnostic_test.go | INV-21 | EOF preserved; failed rerun joins bounded details |
| TestMCPStdioDiagnosticHelper | stdio_diagnostic_test.go | helper — not a test | subprocess dispatch; passes trivially when run as a test; counted in the 230 |
| TestMCPAdmissionFinalTurnCancellationAfterAcquisitionReleasesLifecycle | lifecycle_perf_regression_test.go | INV-25 (INV-06) | cancel after acquisition: no mutation; lifecycleMu free afterwards |
| TestServerLeaseContentionUsesOneCancelableQueueWait | lifecycle_perf_regression_test.go | INV-06 | exactly one tryLock attempt, then cancelable wait; serverLeaseHooks seam |
| TestRetirementReleaseQueuesExactlyOneBlockingClose | lifecycle_perf_regression_test.go | INV-19 | final release returns without waiting; exactly one close |
| TestConcurrentFinalReleasesQueueOneCloseAndReturn | lifecycle_perf_regression_test.go | INV-19 | 4 releases all return; single serialized close |
| TestOwnerCloserUsesOneWorkerForBlockedRetirements | lifecycle_perf_regression_test.go | INV-04 | drain law pinned; "one worker" half is closer bookkeeping (see names note) |
| TestOwnerCloseKeepsFenceWhileDetachedCloseBlocks | lifecycle_perf_regression_test.go | INV-04 | canceled Close → ErrOwnerBusy until the stuck close finishes |
| TestOwnerCloseCancellationWinsWhileLifecycleIsHeld | lifecycle_perf_regression_test.go | INV-04 | Close canceled while lifecycleMu is held externally; coordinator must not finish |
| TestTrackRejectsAdoptionAfterCloseBegins | lifecycle_perf_regression_test.go | INV-04 | trackSessionLocked refuses an already-closing session (tracked==0) |
| TestReplaceRejectsDestinationFenceRaisedDuringPreparation | p1_p2_regression_test.go | INV-10 (INV-15) | destination fence → ErrOwnerBusy; no persist; no runtime events |
| TestEnableRollbackUncertaintySurvivesOwnerRolloverUntilExactReload | p1_p2_regression_test.go | INV-13 (INV-15) | uncertainty + fence survive rollover; exact reload clears; disk disabled |
| TestEnablePendingGlobalAddRollbackRestoresDisabledDefinition | p1_p2_regression_test.go | INV-13 (INV-12) | rollback restores enabled-URL + Disabled=true on disk |
| TestEnableRollbackPreservesConcurrentDefinitionAndFencesStore | p1_p2_regression_test.go | INV-13 | concurrent writer's definition preserved on disk; fence raised |
| TestMCPAdmissionFinalTurnCancellationReleasesConfigLocks | p1_p2_regression_test.go | INV-25 (INV-06) | canceled turn frees the config sidecar lock (contender write succeeds) |
| TestReplacePublishesStateEventsForResultBranches | event_order_test.go | INV-08 (INV-09, INV-18) | exact event sequences for enabled/disabled/rename-absent branches; no extras |
| TestStartFallbackDisabledPublishesBeforeLeaseUnlock | event_order_test.go | INV-08 | Disabled event visible in the afterUnlock hook; no duplicate |
| TestReplacePublishesBeforeBlockedCloseAndConcurrentRemove | event_order_test.go | INV-08 | Updated before Deleted under blocked close + queued remove |
| TestReplaceRevealedFallbackWaitsForBlockedCloseAndMutation | event_order_test.go | INV-08 (INV-09) | fallback starts only after close completes; concurrent disable wins |
| TestSuccessfulAddRemoveReclaimsEveryLeaseReference | lease_completeness_test.go | INV-07 | leases.Len()==0 after add and after remove |
| TestDisableReleasesServerLeaseBeforeClosingDetachedTransport | lease_completeness_test.go | INV-06 | waiter acquires the same lease identity while the detached close blocks |
| TestAddRollbackConsumesItsLastLeaseReferenceOnce | lease_completeness_test.go | INV-07 (INV-11) | both rollback paths leave zero refs and no disk entry |
| TestRetiredClientCannotPublishAfterReplacement | lease_completeness_test.go | INV-03 | publishIfCurrent after invalidateServer rejected; stale writes never land |
| TestExportedRefreshResourcesRejectsStaleResultAfterReplacement | lease_completeness_test.go | INV-17 (INV-03) | in-flight exported refresh loses to replacement; resourcesBeforePublishHook seam |
| TestLockContextRejectsCanceledContextWithoutAcquisition | lease_completeness_test.go | INV-06 | pre-canceled ctx: no acquisition, no registry entry |
| TestDisableCancelsRenewalAndReleasesFollowers | lease_completeness_test.go | INV-19 | disable cancels the blocked renewal; the follower is released |
| TestOperationLeaseRetiresBlockedCallWithoutWriterConvoy | operation_lease_test.go | INV-19 | lease writer completes while a tool call is blocked (CR-4 convoy oracle) |
| TestCancelledFollowerLeavesOwnerInitReference | operation_lease_test.go | INV-19 (INV-04) | timed-out follower leaves no init ref; owner.Close still succeeds |
| TestOperationLeaseDefersCloseUntilFinalReference | operation_lease_test.go | INV-19 | unit: retire false while pinned; close fires on final release |
| TestRetiredSessionsCancelBeforeABlockedClose | operation_lease_test.go | INV-19 (INV-06) | cancel precedes queueing; serial closer; queued sessions untouched |
| TestPinnedRetirementCancelsOnFinalReleaseBeforeBlockedClose | operation_lease_test.go | INV-19 | cancel fires on final release; close queued behind it |
| TestRenewalRetainsLeaseIdentityUntilAfterPublish | operation_lease_test.go | INV-19 (INV-07) | exact refcount timeline 3→3→2→1 via renewal hooks; deepest seam coupling |
| TestReplaceServerFailedSameNamePreservesLiveServerAndDisk | transactional_update_test.go | INV-09 | failed replace: session/tools/prompts/resources/disk unchanged; admission still valid |
| TestReplaceServerPersistenceFailurePreservesOldAdmissionAndCallbacks | transactional_update_test.go | INV-09 (INV-12) | injected persist error: old session + disk preserved, callbacks alive |
| TestReplaceServerFailedRenamePreservesLiveServerAndDisk | transactional_update_test.go | INV-09 (INV-10) | failed rename leaves old identity fully intact |
| TestReplaceServerConditionalTargetCollisionPreservesOldRuntime | transactional_update_test.go | INV-10 | contender writes target → ErrMCPTargetExists; winner on disk; no new session |
| TestReplaceServerSuccessfulSwapPublishesNewSessionOnce | transactional_update_test.go | INV-09 | swap publishes exactly one state event; new session works |
| TestReplaceServerSuccessfulRenameRemovesOnlyOldRuntimeState | transactional_update_test.go | INV-09 | rename removes only old runtime state; new name fully live |
| TestReplaceServerDisabledSameNameCommitsInactiveRuntime | transactional_update_test.go | INV-09 | disabled replacement commits inactive runtime; one Disabled event |
| TestReplaceServerDisabledRenameCommitsInactiveRuntime | transactional_update_test.go | INV-09 | disabled rename: both names inactive; Deleted+Updated events |
| TestReplaceServerConcurrentPostPersistDisableCommitsInactiveRuntime | transactional_update_test.go | INV-09 (INV-12) | disable inside the persister → inactive runtime, disk disabled |
| TestReplaceServerMissingPostPersistConfigLeavesNoOrphanRuntime | transactional_update_test.go | INV-09 (INV-12) | remove inside the persister → no runtime on either name |
| TestReplacedRenameAdmissionTracksCommittedConfig | transactional_update_test.go | INV-10 (INV-24) | renamed admission valid while enabled; cleared on disable/remove (50ms negative windows) |
| TestReplaceServerDurableCommitDuringCloseHonorsCloseDeadline | transactional_update_test.go | INV-04 (INV-09) | 20ms Close deadline honored mid-commit; durable rename lands; no publish |
| TestReplaceServerConcurrentRemoveRejectsCandidateWithoutStalePublish | transactional_update_test.go | INV-08 (INV-09) | remove during candidate connection rejects it; no session, old config gone |
| TestReplaceServerConcurrentDisableRejectsCandidateAndKeepsDisabledState | transactional_update_test.go | INV-08 (INV-09) | concurrent disable rejects candidate; disabled state kept |
| TestReplaceServerOwnerCloseRejectsCandidateWithoutCommit | transactional_update_test.go | INV-04 (INV-09) | owner close rejects candidate; disk keeps old only |
| TestRemoveWorkspaceOverrideStartsGlobalFallbackAfterClose | origin_scope_regression_test.go | INV-08 (INV-09, INV-23) | fallback starts only after old close; correct global fallback serves tools |
| TestReplaceServerWorkspaceLiteralNamePersistsAcrossReloadAndRestart | origin_scope_regression_test.go | INV-23 (INV-09) | workspace literal-key replace persists in the workspace file across reload/restart |
| TestReplaceServerGlobalOriginKeepsGlobalPersistence | origin_scope_regression_test.go | INV-23 | global-origin replace persists to the global file, not workspace |
| TestWorkspaceDisableEnableRemovePersistInWorkspaceFile | origin_scope_regression_test.go | INV-23 | workspace file is the only file touched across disable/enable/remove |
| TestExternalDisableKeepsFullDefinitionAndUsesWorkspaceOverlay | origin_scope_regression_test.go | INV-23 (INV-12) | external definition kept; disable is a workspace overlay |
| TestAddServerConditionalCollisionPreservesTargetAndOwnRuntime | origin_scope_regression_test.go | INV-12 (INV-11) | durable collision: ErrMCPTargetExists; winner on disk; own candidate closed |
| TestWorkspaceRemovalAndReplaceStartRevealedFallbacks | origin_scope_regression_test.go | INV-23 (INV-08, INV-09) | revealed fallbacks from global/project/external origins; disabled stays disabled |
| TestOwnerCloseFencesRevealedFallbackInitialization | origin_scope_regression_test.go | INV-04 | owner close cancels fallback init; no Connected event; state cleared |
| TestReplaceServerProjectOriginRejectsBeforeCandidateConnection | origin_scope_regression_test.go | INV-23 | project-origin replace rejected with ErrMCPUnwritableOrigin; zero requests |
| TestProjectMCPMutationsFailClosed | origin_scope_regression_test.go | INV-23 | disable/enable/remove on project origin all fail closed; global file untouched |
| TestBlockedProjectInitializerIsNotPendingGlobalAdd | origin_scope_regression_test.go | INV-23 | blocked project init is not a pending global add; mutation rejected; initializer survives |
| TestReplaceServerRejectsScopeChangeBeforeDurableWrite | origin_scope_regression_test.go | INV-23 (INV-25) | scope change mid-preparation rejects; nothing published into the new origin |
| TestInitializeCrossStoreStaleAdmissionRetriesFreshDiskSnapshot | admission_retry_regression_test.go | INV-25 | stale admission retried against fresh disk; stale candidate closed |
| TestInitializeSingleCrossStoreStaleAdmissionRetriesFreshDiskSnapshot | admission_retry_regression_test.go | INV-25 | same, InitializeSingle path |
| TestInitializeSlowCandidateSurvivesNoOpReload | admission_retry_regression_test.go | INV-25 | no-op reload does not kill the slow candidate (initCalls==1) |
| TestInitializeSingleSlowCandidateSurvivesNoOpReload | admission_retry_regression_test.go | INV-25 | same, InitializeSingle path |
| TestInitializeRejectsSemanticResolverChangeAndRetriesFreshCandidate | admission_retry_regression_test.go | INV-25 | resolver change rejects old candidate; both header values observed; stale closed |
| TestInitializeSingleRejectsSemanticResolverChangeAndRetriesFreshCandidate | admission_retry_regression_test.go | INV-25 | same, InitializeSingle path |
| TestInitializeRejectsCommandResolverChangeWithoutEnvironmentChange | admission_retry_regression_test.go | INV-25 | command-substitution resolver change rejected |
| TestInitializeSingleRejectsCommandResolverChangeWithoutEnvironmentChange | admission_retry_regression_test.go | INV-25 | same, InitializeSingle path |
| TestInitializeRejectsSingleQuotedCommandResolverChangeWithoutEnvironmentChange | admission_retry_regression_test.go | INV-25 | single-quoted command-substitution form rejected |
| TestInitializeSingleRejectsSingleQuotedCommandResolverChangeWithoutEnvironmentChange | admission_retry_regression_test.go | INV-25 | same, InitializeSingle path |
| TestInitializeRejectsBackquoteCommandResolverChangeWithoutEnvironmentChange | admission_retry_regression_test.go | INV-25 | backquote form rejected |
| TestInitializeSingleRejectsBackquoteCommandResolverChangeWithoutEnvironmentChange | admission_retry_regression_test.go | INV-25 | same, InitializeSingle path |
| TestInitializeReloadsStaleDisabledSnapshotBeforeAdmission | admission_retry_regression_test.go | INV-25 | stale disabled snapshot reloaded before admission; server connects |
| TestInitializeReloadsStaleCLISkippedSnapshotBeforeAdmission | admission_retry_regression_test.go | INV-25 | stale CLI-skipped snapshot reloaded before admission |
| TestInitializeReloadsStaleSkippedSnapshotAfterCrossStoreRemoval | admission_retry_regression_test.go | INV-25 | removal by another store honored: no session, StateDisabled, no init barrier owned |
| TestInitializeReloadsStaleSkippedSnapshotAfterCrossStoreReplacement | admission_retry_regression_test.go | INV-25 | replacement by another store honored; old endpoint never initialized |
| TestInitializeClosesBarrierBeforeInitWaitGroup | admission_retry_regression_test.go | INV-22 | WaitForInit must not hang at the afterWaitGroupDone seam |
| TestInitializeBoundsForcedStaleSkippedAdmission | admission_retry_regression_test.go | INV-25 | forced stale churn bounded at maxAdmissionRetries → StateError, no runtime |
| TestInitializeSingleBoundsForcedStaleSkippedAdmission | admission_retry_regression_test.go | INV-25 | same, InitializeSingle path |
| TestAdmissionRejectsDirectProjectRushSourceReplaceBeforePublication | admission_retry_regression_test.go | INV-25 | direct source rewrite during admission: error, no session, no Connected event |
| TestAdmissionRejectsDirectExternalMCPSourceRemovalBeforePublication | admission_retry_regression_test.go | INV-25 | direct source removal during admission: same, external (.mcp.json) source |
| TestGetOrRenewClientRetriesCrossStoreReplacement | admission_retry_regression_test.go | INV-25 | renewal recovery connects exactly one fresh candidate; old bounded to one retry |
| TestGetOrRenewClientFailsClosedAfterCrossStoreRemoval | admission_retry_regression_test.go | INV-25 | removal during renewal fails closed; no lease; nothing running |
| TestGetOrRenewClientBoundsCrossStoreChurn | admission_retry_regression_test.go | INV-25 | cross-store churn bounded (2 old + 1 new initializations), then fail-closed |
| TestAddServerReconciledCommitOutcomePublishesRuntime | zz_commit_outcome_test.go | INV-14 | Reconciled outcome applies runtime: config persisted, Connected |
| TestEnableServerReconciledCommitOutcomeStartsRuntime | zz_commit_outcome_test.go | INV-14 | Reconciled enable starts the runtime |
| TestDisableServerReconciledCommitOutcomeAppliesRuntime | zz_commit_outcome_test.go | INV-14 | Reconciled disable detaches the session |
| TestRemoveServerReconciledCommitOutcomeAppliesRuntime | zz_commit_outcome_test.go | INV-14 | Reconciled remove clears config/runtime |
| TestReplaceServerReconciledCommitOutcomePublishesCandidate | zz_commit_outcome_test.go | INV-14 | Reconciled replace publishes the candidate |
| TestCommittedUnreconciledOutcomesFenceRuntime | zz_commit_outcome_test.go | INV-14 | Committed-but-unreconciled fences: session detached, Disabled |
| TestMaybeCommittedAddFencesUntilReload | zz_commit_outcome_test.go | INV-14 (INV-15) | maybe-committed fences the name until reload clears it |
| TestMaybeCommittedEnableFencesUntilReload | zz_commit_outcome_test.go | INV-14 (INV-15) | same for enable |
| TestMaybeCommittedDisableFencesUntilReload | zz_commit_outcome_test.go | INV-14 (INV-15) | same for disable |
| TestMaybeCommittedRemoveFencesUntilReload | zz_commit_outcome_test.go | INV-14 (INV-15) | same for remove |
| TestMaybeCommittedReplaceFencesUntilReload | zz_commit_outcome_test.go | INV-14 (INV-15) | same for replace |
| TestTypedPrecommitOutcomeDoesNotCommitAdd | zz_commit_outcome_test.go | INV-14 (INV-11) | typed precommit outcome: nothing committed, nothing published |
| TestAddDurableCommitFencedByCloseReturnsSuccessWithoutPublishingCandidate | zz_commit_outcome_test.go | INV-04 (INV-11) | close wins publication: durable commit stands, candidate closed once, cleanup before reset |
| TestAddReconciledCommitOutcomeFencedByCloseReturnsOriginalOutcome | zz_commit_outcome_test.go | INV-04 (INV-14) | close during reconciled commit returns the original outcome; no candidate publish |
| TestAddLinearizesPublicationBeforeConcurrentRemoveAfterCommit | zz_commit_outcome_test.go | INV-08 | hook-proven order: remove's lock attempt waits; Connected published under the lease before Deleted |
| TestRemovePublishesBeforeBlockedCloseAndConcurrentAdd | zz_commit_outcome_test.go | INV-08 | Deleted before lease release to a same-name Add; Add publishes after |
| TestTypedPrecommitOutcomeDoesNotDisableRuntime | zz_commit_outcome_test.go | INV-14 (INV-12) | precommit disable leaves runtime enabled and untouched |
| TestTypedPrecommitOutcomeDoesNotReplaceRuntime | zz_commit_outcome_test.go | INV-14 (INV-09) | precommit replace leaves old runtime and config untouched |
| TestReplaceRejectsStalePersisterResultWithoutPublishingCandidate | lifecycle_findings_test.go | INV-08 (INV-10) | persister's post-persist disable wins; candidate not published; exact events |
| TestReplaceRejectsCrossStoreMutationBeforePin | lifecycle_findings_test.go | INV-08 (INV-10) | same via a second store's mutation |
| TestMCPMutationPinHoldsSidecarsThroughLifecyclePublication | lifecycle_findings_test.go | INV-08 | pinned publication blocks the second store's write (100ms window) then both land |
| TestReplaceKnownCommitFencesRuntimeAfterEpochInvalidation | lifecycle_findings_test.go | INV-03 | epoch invalidation inside the persister fences the runtime; no candidate publish |
| TestDisableRechecksUncertaintyAfterWaitingForLease | lifecycle_findings_test.go | INV-14 | lease waiter rechecks the winner's fence: ErrMCPConfigUncertain, no persist |
| TestRemoveRechecksUncertaintyAfterWaitingForLease | lifecycle_findings_test.go | INV-14 | same for remove |
| TestDisableLeaseWaitUncertaintyDuringClose | lifecycle_findings_test.go | INV-14 (INV-04) | same during owner close; close still completes |
| TestRemoveLeaseWaitUncertaintyDuringClose | lifecycle_findings_test.go | INV-14 (INV-04) | same for remove |
| TestMutationPublicationTakesServerLeaseBeforeConfigLocks | lifecycle_findings_test.go | INV-06 | publication blocked on the lease held by a disable; after release it sees the canceled admission |
| TestEnableRejectsChangedConfigBeforePublication | lifecycle_findings_test.go | INV-13 (INV-25) | config changed pre-publication → initializer gets ErrMCPMutationStale; no session |
| TestAddRejectsStoreGenerationChangedDuringPreparation | lifecycle_findings_test.go | INV-03 | store-generation change during preparation rejects the add (ErrOwnerBusy); nothing left |
| TestEnableRejectsStoreGenerationChangedBeforePublication | lifecycle_findings_test.go | INV-03 (INV-13) | generation change before publication fences the enable candidate |
| TestMCPAdmissionSurvivesUnrelatedCOWUpdate | lifecycle_findings_test.go | INV-24 | unrelated store COW write (SetSkipPermissionRequests) keeps the admission valid |
| TestAddCloseRollsBackCanceledPendingConfig | lifecycle_findings_test.go | INV-11 | owner close cancels pending add: no persist, no disk file, no runtime, no events |
| TestInitializeRejectsChangedConfigBeforePublication | lifecycle_findings_test.go | INV-25 | config changed during init → candidate rejected; replacement config stands |
| TestAddRejectsChangedConfigBeforePublication | lifecycle_findings_test.go | INV-25 (INV-11) | changed config before publication → ErrMCPMutationStale; contender value on disk |
| TestReplaceRejectsSourceEditDuringPreparation | lifecycle_findings_test.go | INV-25 | source edit during preparation → ErrOwnerBusy; target absent |
| TestReplaceAllowsNoOpReloadDuringPreparation | lifecycle_findings_test.go | INV-25 | no-op reload tolerated; replacement still commits |
| TestReplaceAllowsUnrelatedCOWDuringPreparation | lifecycle_findings_test.go | INV-24 (INV-25) | unrelated COW write tolerated during preparation |
| TestReplaceRejectsSemanticResolverChangeDuringPreparation | lifecycle_findings_test.go | INV-25 | semantic resolver change rejected; target absent |
| TestReplaceRejectsPendingAddDestinationBeforePreparation | lifecycle_findings_test.go | INV-10 | destination held by a pending add → ErrMCPTargetExists; no persist |
| TestReplaceFallbackSelectionIgnoresAbsentOrDisabledReplacement | lifecycle_findings_test.go | INV-09 | unit: fallback selection picks the fallback for absent/disabled replacement |
| TestMCPAdmissionRejectsSecondStoreChangeBeforePin | lifecycle_findings_test.go | INV-25 | second-store change before the pin → ErrMCPMutationStale |
| TestMCPAdmissionHoldsSidecarsThroughSkippedTransition | lifecycle_findings_test.go | INV-08 | admission holds the sidecar boundary against a second store (100ms window) |
| TestMCPAdmissionFinalTurnValidatesAfterLifecycleAcquisition | lifecycle_findings_test.go | INV-25 | source changed between validate and the lifecycle turn → stale; project + external |
| TestInitializeSingleDisabledDetachesPublishedRuntime | lifecycle_findings_test.go | INV-12 (INV-18) | initialize-single on a now-disabled server detaches runtime; exactly one Disabled event |
| TestInitializeCLISkipDetachesPublishedRuntime | lifecycle_findings_test.go | INV-12 (INV-18) | same for enabled_in_cli=false + restrictToCLIEnabled |
| TestStaleAdmissionCleanupLeavesNoOwnerlessStarting | lifecycle_findings_test.go | INV-18 | cleanupFailedAdmission resolves Starting → StateError; exactly one event |
| TestSkippedTransitionReleasesLeaseBeforeBlockingRetirement | lifecycle_findings_test.go | INV-06 | same-name mutation acquires the lease while the skipped transition's close blocks |
| TestMCPSession_CancelOnClose | init_test.go | MACHINERY | unit: Close cancels the session context (+goleak); building block, no registry law |
| TestCreateTransport_URLResolution | init_test.go | MACHINERY | transport feature: URL resolver seam incl. lenient-nounset and identity resolver; non-vacuous |
| TestCreateTransport_StdioResolution | init_test.go | MACHINERY | command/args/env resolver seam, success + each failure field |
| TestCreateTransport_HeadersResolution | init_test.go | MACHINERY | header resolver seam; a failing header aborts transport creation |
| TestCreateTransport_PassesLifetimeContextToEveryRuntimeField | init_test.go | INV-25 | every resolved field uses the operation context (179cd54b leg) |
| TestCreateTransport_PreCanceledContextWinsFieldValidation | init_test.go | INV-20 | pre-canceled admission context aborts before any resolver I/O |
| TestOwnerCloseJoinsCanceledRuntimeMCPResolution | init_test.go | INV-04 | close joins a blocked runtime resolution; both orders accepted |
| TestCreateSession_ResolutionFailureUpdatesState | init_test.go | MACHINERY | feature: resolution failure publishes StateError (UX contract, no INV) |
| TestInitialize_RestrictToCLIEnabled | init_test.go | MACHINERY | enabled_in_cli gate for CLI mode; "attempted" inferred via State != Disabled |
| TestOwnerCloseResetsRegistryAndAllowsNextLifecycle | init_test.go | INV-02 (INV-04) | second Acquire refused; registry empty after close; next lifecycle unpoisoned |
| TestOwnerClosePreservesOtherOwnersAcrossCloseOrders | standalone_owner_test.go | INV-02 (INV-04) | both close orders preserve the surviving owner's live loopback tool, registry, event subscription, and lease identity across installed-owner rollover |
| TestInstalledClosePreservesStandaloneDisabledState | owner_state_cleanup_test.go | INV-02 (INV-04) | real disabled initialization survives installed-owner close and installed-owner rollover, then is removed by the owning standalone close |
| TestOwnerClosePreservesClaimedNoSessionStatesAcrossOrders | owner_state_cleanup_test.go | INV-02 (INV-04) | disabled, error, and starting states with no session survive the foreign-owner close in either order and are removed by their owner |
| TestOwnerCloseCancelsBlockedStartupBeforeCleanup | init_test.go | INV-04 (INV-20) | blocked init canceled; init finishes before Close returns; states cleared |
| TestOwnerCloseVsRenewalDoesNotPublishLateSession | init_test.go | INV-01 (INV-04) | rejected late renewal closed exactly once; no session/state |
| TestOwnerCommitRenewalPublishesSuccessfulSession | init_test.go | INV-19 (INV-16) | successful commitRenewal publishes session + state + event; deadlock-guarded |
| TestRenewalRejectsCrossStoreDiskReplacement | init_test.go | INV-25 | commitRenewal on a stale cross-store snapshot → stale, no publish |
| TestRenewalRejectsCrossStoreDiskRemoval | init_test.go | INV-25 | same for removal |
| TestFailClosedMCPHonorsCallerCancellation | init_test.go | INV-06 | fail-closed under a held lease returns context.Canceled; no network |
| TestSessionContextPromotionLinearizesCancellation | init_test.go | INV-01 | 3 subtests: caller/candidate/owner races at the promote handoff |
| TestClientSessionCloseAbortsPromotedCandidate | init_test.go | INV-01 | Close aborts a promoted handoff; worker goroutine stops |
| TestPublishedSessionIgnoresCandidateCancellationUntilClose | init_test.go | INV-01 | candidate cancel does not kill a published session context |
| TestRepeatedPromotedCandidateCloseStopsHandoff | init_test.go | INV-01 (INV-04) | 32 closes: handoff goroutine always stopped (no leak) |
| TestCommitRenewalPublishesOnlyAfterPromotion | init_test.go | INV-01 | rejected handoff → ErrOwnerBusy + closed once; accepted survives candidate cancel |
| TestClientLeaseProtectsOperationFromRenewalAndClose | init_test.go | INV-19 | pinned operation blocks renewal and a past-deadline close; then completes |
| TestHeaderRoundTripperKeepsOwnerCancellationUntilBodyClose | init_test.go | INV-20 | owner cancel reaches an open response body; body close unblocks |
| TestOwnerResponseBodyCloseCancelsBeforeUnderlyingClose | init_test.go | INV-20 | unit: body Close cancels the request before closing the underlying body |
| TestOwnerCloseDeadlineRetainsFenceUntilStuckSessionCloses | init_test.go | INV-04 | expired Close deadline keeps the fence until the stuck session closes |
| TestDisableServerCancelsBlockedInitAndLeavesNoLateSession | state_regression_test.go | INV-03 (INV-12) | disable cancels blocked init; no late session; disabled persisted |
| TestRemoveServerCancelsBlockedInitAndRejectsLateCommit | state_regression_test.go | INV-03 (INV-12) | remove cancels init; config gone; no session |
| TestFailedDisableDoesNotCancelStartupCandidate | state_regression_test.go | INV-03 (INV-12) | failed persistence leaves the candidate alive; it publishes Connected |
| TestFailedRemoveDoesNotCancelStartupCandidate | state_regression_test.go | INV-03 (INV-12) | same for remove |
| TestEnableServerFromOldOwnerCannotPublishIntoNewOwner | state_regression_test.go | INV-02 | old owner's initializer never publishes into the new owner |
| TestHeaderRoundTripperCancelsBlockedRequestBeforeResponse | state_regression_test.go | INV-20 | owner cancel reaches a blocked request pre-response |
| TestListChangedRefreshIsAsyncAndStaleAdmissionIsIgnored | state_regression_test.go | INV-17 | callback non-blocking (<100ms); invalidated admission's notification ignored |
| TestCandidateListChangedIsRetainedUntilExactCommit | state_regression_test.go | INV-16 | deferred request pending under the exact candidate token; nothing runs |
| TestFailedCandidateListChangedDropsDeferredEvent | state_regression_test.go | INV-16 | done() discards the deferred request; no event |
| TestCandidateListChangedPublishesAfterCommitAndRefreshesNewSession | state_regression_test.go | INV-16 | exactly one deferred publish post-commit; refresh uses the new session |
| TestReplacementCandidateListChangedUsesCommittedDestinationAdmission | state_regression_test.go | INV-16 | replacement candidate notification uses the committed destination admission |
| TestAddServerRetainsLeaseAcrossRemoveDuringInitialization | state_regression_test.go | INV-07 | lease retained + refcounted during unlocked init; remove reclaims without underflow |
| TestRemoveDisabledFallbackPublishesOneStateEvent | state_regression_test.go | INV-08 (INV-18) | exactly one Disabled updated-event for remove-to-disabled |
| TestPendingAddMutationCommitsCompleteConfigBeforeInvalidatingAdd | state_regression_test.go | INV-12 | pending disable/remove: complete config on disk; transaction closed only after |
| TestPendingAddRemoveUsesOneConditionalWriteWithoutEnabledWindow | state_regression_test.go | INV-12 | exactly one persistence call; no enabled definition ever hits disk |
| TestPendingAddRemoveRejectsDurableCollision | state_regression_test.go | INV-12 (INV-11) | contender wins → ErrMCPTargetExists on both sides; winner persisted |
| TestPendingAddDisableUsesOneWriteAndPreservesConcurrentWriter | state_regression_test.go | INV-12 | full-definition pending write; a concurrent other-server write is preserved |
| TestStaleAddRollbackPreservesNewerSameNameServer | state_regression_test.go | INV-11 | old add's failure rollback no-ops; the replacement stays fully live |
| TestGetOrRenewClientKeepsLeaseIdentityAcrossConcurrentReplacement | state_regression_test.go | INV-19 | two callers, one renewal; queued caller gets the same session; refs drained to 0 |
| TestRenewedSessionNotificationSurvivesConfigMutations | state_regression_test.go | INV-24 | same-server config mutations keep the notification admission; refresh honors filters |
| TestPublishedHTTPNotificationSurvivesUnrelatedReload | state_regression_test.go | INV-24 | an unrelated store write does not retire the published session |
| TestReloadFencesPublishedSessionAfterRelevantMCPChange | state_regression_test.go | INV-24 | a relevant change + reload detaches the session |
| TestInvalidCommittedNotificationClearsAllAdvertisedData | state_regression_test.go | INV-18 (INV-03) | invalidated committed admission clears tools/prompts/resources; one Disabled event |
| TestGetOrRenewFencesStaleCommittedSessionBeforePing | state_regression_test.go | INV-24 | stale session fenced before ping (pingCalls==0) |
| TestReloadReconciliationWaitsForNewerLeaseWinner | state_regression_test.go | INV-03 (INV-24) | reconcile blocked on the lease yields to a newer epoch/admission |
| TestListChangedDuringRefreshSchedulesDirtyRerun | state_regression_test.go | INV-17 | dirty marker coalesces exactly one rerun; latest list published |
| TestDirtyRefreshFailuresRetainCurrentSession | state_regression_test.go | INV-17 | two failures → StateError with session/counts retained; a later refresh recovers |
| TestExportedRefreshFailuresRetainSessionCountsAndRecover | state_regression_test.go | INV-17 | exported tools/prompts/resources refresh failures retain, then recover |
| TestRefreshQueueSaturationCoalescesWithoutDropping | state_regression_test.go | INV-17 | 256 queued past the wake channel; pending map authoritative; duplicates coalesce |
| TestLeaseRegistryReclaimsChurnAndProtectsWaiterFromABA | state_regression_test.go | INV-07 | 500 churn cycles empty the registry; waiter keeps identity (refs==2) |
| TestMCPStdioHelper | state_regression_test.go | helper — not a test | subprocess stdio server; passes trivially when run as a test; counted in the 230 |
| TestInitializePublishesHTTPSessionBeyondAdmission | state_regression_test.go | INV-01 | session survives the init-timer window (1.2s); ping/call work; initCalls stays 1 |
| TestInitializePublishesStdioSessionBeyondAdmission | state_regression_test.go | INV-01 (INV-21) | same for a real subprocess stdio server |
| TestInitializeSinglePinsOwnerAcrossRollover | state_regression_test.go | INV-02 | old owner's InitializeSingle dies with the owner; nothing lands in the new owner |
| TestInitializeSingleReplacesSameNameOldSession | state_regression_test.go | MACHINERY | re-init swaps the session (NotSame + Connected); real behaviour, no registry law |
| TestInitializeSingleDefersCandidateNotificationWithOldSameNameSession | state_regression_test.go | INV-16 | candidate notification deferred past commit; refresh sees committed tools |
| TestInitializeSingleFailedCandidateDropsNotification | state_regression_test.go | INV-16 | failed candidate publishes no tools-list-changed (25ms negative window) |
| TestRefreshAdmissionDoesNotOwnInitializerCancellation | state_regression_test.go | INV-03 (INV-17) | refresh cleanup keeps the initializer's cancel token; invalidation still cancels it |
| TestInitializeBarrierIsGenerationScopedAcrossRollover | state_regression_test.go | INV-22 | old owner's waiter released at close; fresh owner's barrier immediate |
| TestWaitForInitIsImmediateWithoutFullInitialize | state_regression_test.go | INV-22 | lazy barrier: immediate without a full init |
| TestSequentialInitializeReopensInitBarrier | state_regression_test.go | INV-22 | a second full init reopens the barrier; waiters block, then proceed |
| TestInitializeSingleDoesNotOwnInitBarrier | state_regression_test.go | INV-22 | failed + disabled single inits do not own the barrier |
| TestSuccessfulInitializeSingleDoesNotOwnInitBarrier | state_regression_test.go | INV-22 | a successful single init does not own the barrier |
| TestInitializeSingleCannotCompleteFullInitBarrier | state_regression_test.go | INV-22 | a single init cannot close a full init's barrier |
| TestHeaderRoundTripperUsesOwnedTransportPool | state_regression_test.go | INV-20 | structural only: cloned transports are per-session pointers (shallow; see names note) |
| TestCloneHTTPTransportFallsBackFromCustomDefault | state_regression_test.go | MACHINERY | clone falls back when DefaultTransport is a custom RoundTripper; plumbing detail |
| TestMaybeTimeoutErrPreservesNonTimeoutCancellation | state_regression_test.go | INV-01 | unit of the cancellation-vs-owned-timeout classification law |
| TestSessionCancellationClassification | state_regression_test.go | INV-01 | caller cancel/deadline, admission cancel, owned timeout classified (4 subtests) |
| TestPingTimeoutClassification | state_regression_test.go | INV-01 | same classification on the ping path (2 subtests) |

## NO-TEST findings — closed by stage 1.4 (task #905)

Both clause-level gaps below were closed by the new file
`internal/agent/tools/mcp/lock_discipline_test.go`. This supersedes the
stage-1.2 statement that the two gaps "stand unchanged and are the complete
1.4 backlog". Both new tests are revert-checked: each was shown to fail when
its guarded behaviour was temporarily broken, and to pass again after a
byte-exact restore.

- INV-07, "multi-server operations take leases in sorted name order":
  COVERED by `TestReplaceServerAcquiresLeasesInSortedNameOrder`. It drives a
  rename `ReplaceServer` whose call site passes (oldName, newName) in reverse
  lexical order (`zz-order-source` → `aa-order-target`), records every lease
  acquisition attempt through `serverLeaseHooks.beforeTryLockFn`, and asserts
  the observed order is the sorted order. Removing `slices.Sort` from
  `lockServerLeasesContext` fails the test with the two names swapped. The
  test pins the shared sort inside `lockServerLeasesContext`, so both
  multi-name lock rounds of ReplaceServer — and any future caller of that
  helper — are covered by construction.
- INV-06, "no network or disk I/O while holding `lifecycleMu` or a server
  lease": COVERED NARROWLY, at two of the call sites the invariant names.
  - `TestReplacePersistRunsOutsideLifecycleLock` probes `lifecycleMu` free at
    the durable replacement write (the injected `persist` seam of
    `replaceServerWithResultPersistenceAndPreparation`). The ordered server
    leases are held at that seam by design ("Keep the ordered server leases,
    but never hold lifecycleMu here") and are deliberately not probed there.
  - `TestUncertaintyReloadRunsOutsideLifecycleLocks` probes `lifecycleMu` and
    every lease entry in the registry write-free at the uncertainty reload's
    finalize seam (`mcpReloadAfterSuccessHook` in
    `reloadWithUncertaintyToken`), with a live seeded lease entry so the
    lease leg cannot pass vacuously.
  - Revert-checks: holding `lifecycleMu` across the persist call, or across
    the reload and its finalization, makes the respective test fail in under
    a second with the held lock named.

  REMAINING UNCOVERED, stated precisely:
  - The general law is not directly assertable in Go; only instrumented seams
    are oracles. Other I/O sites — the network connect in candidate
    preparation, and the persist seams of add/enable/disable/remove — have no
    lock probe.
  - The reload observation point is the finalize seam AFTER the disk read
    returns. A regression that holds `lifecycleMu` only around the raw read
    and releases before finalization would not be caught; a hold through
    finalization — the realistic shape — is caught. Holding it through the
    whole function is independently impossible: `reconcilePublishedSessions`
    re-takes `lifecycleMu` and self-deadlocks (the deferred-hold revert-check
    demonstrated this as a 10-minute test hang — itself evidence the lock
    discipline is load-bearing).
  - The probes test the write side only; read-locked (RLock) lease holders
    are not probed anywhere.

## Review-vs-code notes

- Chunk-5 review (2026-09-07) listed CH5-1 (P0 `committedAdmissions` race),
  CH5-2 (P2 renewal wait), and CH5-3 (P1 no-op reload kills candidates) as
  open. All three are closed on this HEAD, verified in code and via git:
  CH5-1 by `00c9911f5` (+ oracles `0175b1e0`), CH5-2 by `70c98f828`, CH5-3 by
  `00c9911f5`. Their mechanisms remain accurate descriptions of WHY the laws
  above exist.
- The 36h review's CR-1..CR-4 closure claims were re-verified against this
  HEAD's code (sessionContext/promote chain, renewal lifetime, lazy barrier,
  operation leases): all confirmed. No review claim was found to contradict
  the code as read.

## Commit → invariant

Reading `git log -- internal/agent/tools/mcp/` through this table. Most
commits have empty bodies; the reasoning lives here and in
`docs/reviews/2026-09-05-2047-commit-review-36h.md` (CR-1..CR-4) and the five
2026-09-07 chunk reviews (CH5-*).

| Commit | Subject | Invariants | What it taught |
|---|---|---|---|
| `5f2dbebc` | own MCP lifecycle per application | INV-02, INV-04 | one owner per process; explicit lifecycle token |
| `01e909d0` | harden MCP owner lifecycle shutdown | INV-01 (broke it: CR-1), INV-04 | close deadline keeps the fence (R17-3); `defer cancel()` killed transports |
| `60c81f78` | correct MCP shutdown cleanup ordering | INV-04, INV-19 | cancel every transport first, then sequential closes; renewal consumes the session on every path |
| `963d584a` | lease MCP sessions through operations | INV-03 | admission = owner generation + server epoch; `acceptsGeneration` |
| `92f2ee9a` | fence MCP lifecycle state and refreshes | INV-03, INV-07, INV-17, INV-18, INV-20 | refcounted lease registry, refresh queue/worker, per-admission state, owned HTTP pool with explicit cancel |
| `49e008fa` | test: assert MCP admission cancels HTTP requests | INV-20 | strict non-vacuum cancellation oracles |
| `2c23fcd3` | harden MCP lease and refresh lifecycle | INV-07, INV-17 | ABA-safe `retain` bool; pending map as authoritative queue after wake-channel loss |
| `71bd43db` | preserve latest MCP lifecycle state | INV-03 | identity pinning narrowed to where it is semantically needed |
| `bc786a4d` | allow MCP callbacks after config updates | INV-01, INV-24 | renewal sessions moved to lifecycle ctx (CR-2 interim) |
| `5c8641e7` | harden MCP session lifecycle ownership | INV-01, INV-04 | `sessionContext` handoff (CR-1 mechanism); tracked sessions joined at close |
| `99371d14` | disarm MCP initialization timeouts | INV-01 | init timer must not fire after successful connect (F4, CR-1 echo with 15 s delay) |
| `b7d8614f` | linearize MCP session promotion | INV-01 | promotion is the atomic handoff point, checked under `lifecycleMu` before publish |
| `922316af` | close MCP lifecycle gaps | INV-01, INV-16, INV-22 | handoff extended to renewal path; deferred refresh activation; lazy init barrier + shutdown close (CR-3) |
| `3199bf34` | preserve refresh status and reopen init barriers | INV-22 | barrier reopened per full init; CR-3 closure leg |
| `1bf8bcdc` | preserve runtime lifecycle on refresh failures | INV-18 | state must never infer session ownership from previous state's Client |
| `cf12608e` | make updates transactional and literal-key safe | INV-09, INV-11 | replace as a transaction; introduced the epoch-bump defect fixed in `71df8f19` |
| `71df8f19` | preserve admissions during transactional replace | INV-03, INV-09 | failed replace must not invalidate the live old admission |
| `14b8677d` | finalize inactive replacements | INV-09 | durable-replace-but-not-runnable outcome fences both names, events describe reality |
| `f11d8fed` | scope MCP lifecycle mutations by origin | INV-23 | mutations resolve a writable origin |
| `95faff44` | restrict MCP global fallback to pending adds | INV-23 | global scope only for a registered pending Add |
| `9d1a6f2f` | close MCP add transactions after durable outcome | INV-11 | add transaction outlives initializer, closes after durable commit/rollback |
| `779e323b` | persist pending MCP disables atomically | INV-12 | pending disables are atomic full-definition writes |
| `950c1a04` | restrict MCP global fallback to pending adds (remove path) | INV-12 | one conditional write, no enabled window on disk |
| `08f5071b` | fence revealed MCP fallbacks | INV-09 | fallback starts under the lease with effective-definition check |
| `e3b79c04` | add cancellable MCP session operation leases | INV-19, INV-06 | operation pinning; network never under the server lease (CR-4 convoy closed) |
| `3c3b7dde` | complete MCP lease lifecycle fencing | INV-07, INV-19 | `publishIfCurrent` validate+mutate critical section; introduced CH5-2 wait-order regression |
| `5da43c1a` | fence exported MCP resource refresh | INV-17 | exported refresh re-admitted under fencing |
| `6c2030dd` | defer candidate notification events until commit | INV-16 | candidate notifications are invisible until that candidate commits |
| `646b0f49` | linearize MCP candidate promotion and notifications | INV-16 | promotion/notification ordering under one lock |
| `98f769e7` | reconcile uncertain lifecycle commits | INV-14, INV-15 | uncertain commits are reconciled, not ignored |
| `c4440e60` | distinguish typed commit outcome status | INV-14 | typed Committed/MaybeCommitted/Reconciled outcomes |
| `18332c65b` | fence uncertain config lifecycle | INV-14, INV-15 | reload boundary owned by `ReloadAndReconcileMCPConfig` |
| `b639b317` | fence uncertain MCP mutations | INV-14 | unreadable outcome fences runtime, never guesses |
| `da066d7f` | persist config uncertainty across owner lifecycles | INV-15 | uncertainty belongs to the store, survives rollover and reload |
| `59d1d8d9` | fence rename and enable rollback races | INV-10, INV-13 | dual-identity guards; rollback failure raises fence |
| `c7dfca65` | fence stale replacement lifecycle results | INV-10 | stale results cannot publish stale lifecycle state |
| `6c15f330` | make enable rollback conditional | INV-13 | rollback only onto the exact prior state (CAS) |
| `46f68de7` | linearize add publication with durable commit | INV-08 | commit+publication in one lease transition |
| `4040f4d5` | make add linearization oracle deterministic | INV-08 | `serverLeaseHooks` test seams; production lock ownership unchanged |
| `a470adf6` | close detached MCP transports outside server leases | INV-06, INV-08 | transport closes (can block) never inside the lease |
| `49c92cb1` | linearize remove event publication | INV-08 | Deleted event reaches subscribers before lease release to a same-name Add |
| `a62a37e6` | linearize replacement event publication | INV-08 | replacement events are part of the lease linearization point |
| `62371a33` | pin MCP lifecycle publication against disk | INV-08 | publication pinned to the exact durable result |
| `779802b2` | fence MCP lifecycle admissions | INV-25 | admission source revisions; introduced CH5-3 (resolver revision over-fenced candidates) |
| `f2914d53c` | preserve MCP session identity across revisions | INV-24, INV-05 (broke it: CH5-1) | committed sessions judged by definition equality, not revisions; introduced the unsynchronized map |
| `00c9911f5` | harden MCP admission publication and lifecycle synchronization | INV-05, INV-25 | `lifecycleMu` inside `detachSessionLocked` (CH5-1 closed); no-op reload tolerated, semantic change retried (CH5-3 closed) |
| `70c98f828` | close MCP generations before fallback | INV-19 | follower renewal-wait restored before availability check (CH5-2 closed) |
| `87de26a7` | revalidate all MCP admission sources | INV-25 | every admission path revalidates its source |
| `d2288666` | linearize MCP admission source bytes | INV-25 | final-turn byte validation of the pinned source |
| `260abdc7` | linearize MCP admission final turn | INV-25 | final validate happens after lifecycle acquisition |
| `c5f1785d` | close MCP admission race windows | INV-05, INV-08 | admission/publication windows closed |
| `cebc912f` | harden MCP admission and lifecycle | INV-03, INV-04 | admission + lifecycle hardening sweep |
| `57806e47` | pin MCP lifecycle admissions and cleanup | INV-03, INV-04 | admissions pinned through cleanup |
| `28ad297f` | reclaim implicit MCP owner workers | INV-02 | implicit owner reclaim waits for its workers |
| `07a69d8f` | isolate library MCP surfaces by config | INV-02 | registry data filtered by `IsConfigured` per consuming store |
| `2de9d044` | bound MCP admission I/O lifecycle | INV-20 | admission context fences all admission I/O |
| `e0d375d8` | cancel retired MCP sessions before close queue | INV-19, INV-06 | cancellation precedes queueing; nonblocking under no lifecycle lock |
| `4d6e0ac4` | unblock readers after canceled writer | INV-06 | contextlock fairness under cancellation (supports INV-06 discipline) |
| `179cd54b` | honor MCP operation context in value resolution | INV-25 | resolver values resolved under the operation's context |
| `b956dacb1` | port(mcp): tree-kill stdio server process groups on cancel | INV-21 | whole process tree dies with the session context (ported from upstream `132a8c89`; 15+ orphaned zombies motivated it) |
| `c96eaa4de` | bound stdio startup diagnostics | INV-21 | diagnostic re-run bounded in bytes and time |
| `0175b1e0` | test(mcp): pin cross-server committedAdmissions locking discipline | INV-05 | stage 0.2 oracle; revert-check reproduced the fatal race twice |
