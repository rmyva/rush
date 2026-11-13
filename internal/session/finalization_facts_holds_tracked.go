package session

// CountFinalizationTrackedProcessHolds returns the process-level count of
// in-process tracked process trees: a non-blocking, mutex-protected
// snapshot of the in-process tracked tree registry (trackedJobs on
// Windows, trackedGroups elsewhere) with no I/O.
//
// This is a PROCESS-level count used as a conservative over-approximation
// of per-owner physical holds: any live tracked tree defers every owner's
// seal until physical teardown, then the count returns to baseline.
// Attribution to a single owning session is not possible today because
// the registries are pid-keyed and the cliprovider spawn sites carry no
// owning-session index.
func CountFinalizationTrackedProcessHolds() int {
	return finalizationTrackedProcessHoldCount()
}
