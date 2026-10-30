package agent

// Durable-restart refusal for calls carrying a caller-supplied
// tools.DiskProvider (SDK task #859, design doc §7).
//
// A DiskProvider is arbitrary in-process Go code (typically a closure
// over host state: an open DB handle, an HTTP client, an in-memory map)
// with no serializable form at all — unlike permission.FolderScope,
// which T12 persists as an uncompiled spec and recompiles on restart, a
// disk provider has nothing to mirror. If a call carrying one were
// durably enqueued, a restart would rebuild it with CallOptions.DiskProvider
// == nil, and buildTools' diskOrOS normalizes that nil straight to
// OSDisk() — silently replaying the model's writes onto the operator's
// REAL filesystem instead of the host's. This follows T9's "refuse
// outright" precedent (rejectScopedCallOnCLIProvider), not T12's
// "persist and restore" one: dropping the call is the fail-closed
// direction, since the host is still in-process and can retry.

import (
	"errors"

	"github.com/PHPCraftdream/rush/internal/agent/tools"
)

// ErrDiskProviderNotDurable is returned only for a forbidden durable
// handoff / rebuild of a provider-dependent session call. It no longer
// classifies an ordinary valid provider-backed batch invocation: those
// carry a process-bound binding (disk_provider_binding.go) instead.
var ErrDiskProviderNotDurable = errors.New(
	"agent: a call with a caller-supplied disk provider cannot be durably queued")

// ErrDiskProviderUnavailable is returned when batch work requires a
// process-bound disk binding that is no longer registered (released,
// drained, or lost to a restart). It never falls back to OS disk.
var ErrDiskProviderUnavailable = errors.New(
	"agent: the process-bound disk provider for this work is no longer available")

// ErrCredentialBindingUnavailable is returned when credentials originating
// from a tenant call cannot be retained for automatic continuation.
var ErrCredentialBindingUnavailable = errors.New(
	"agent: credentials originating from this tenant are unavailable for automatic continuation")

// ErrCredentialSetNotDurable is returned when a process-local tenant
// credential bundle reaches a durable handoff path.
var ErrCredentialSetNotDurable = errors.New(
	"agent: a call with per-call credentials cannot be durably queued")

// callCarriesDiskProvider reports whether call's per-call options carry a
// host-supplied filesystem that must never reach the durable run queue.
func callCarriesDiskProvider(call SessionAgentCall) bool {
	return call.CallOptions != nil && tools.HasDiskProvider(call.CallOptions.DiskProvider)
}

// callCarriesNonDurableDependency reports whether rebuilding a call from a
// durable row could change its execution identity. Both credentials and a
// caller-supplied DiskProvider are process-local and must stay in-process.
func callCarriesNonDurableDependency(call SessionAgentCall) bool {
	return call.Credentials != nil || callCarriesDiskProvider(call)
}
