// Disk persistence: resolving a scope to its on-disk path, the
// inter-process write-lock machinery that guards read-modify-write cycles,
// and the raw set/remove field operations built on top of it.
package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// configPath returns the file path for the given scope.
func (s *ConfigStore) configPath(scope Scope) (string, error) {
	switch scope {
	case ScopeWorkspace:
		workspacePath := s.loadSnapshot().workspacePath
		if workspacePath == "" {
			return "", ErrNoWorkspaceConfig
		}
		return workspacePath, nil
	default:
		return s.globalDataPath, nil
	}
}

// HasWorkspaceConfig reports whether a workspace config path is resolvable
// at all — distinct from whether the workspace file has any explicit
// content. Read/write calls at config.ScopeWorkspace fail with
// ErrNoWorkspaceConfig when this is false (e.g. no .rush directory could be
// resolved for the current working directory). Used by API consumers, such
// as the web server's scoped-models UI, that need to grey out folder-scope
// controls rather than surface a write error after the fact.
func (s *ConfigStore) HasWorkspaceConfig() bool {
	_, err := s.configPath(ScopeWorkspace)
	return err == nil
}

// errKtavWriteNotSupported guards the (D) write boundary: until the Ktav
// writer lands (KTAV-3), a selected .ktav target must never be edited by
// the sjson/JSON read-modify-write path.
var errKtavWriteNotSupported = errors.New("writing rush.ktav is not supported yet in this build")

// HasConfigField checks whether a key exists in the config file for the given
// scope. The read goes through the settings-document layer, so a .ktav file
// is decoded before the key lookup; a decode failure is not "field absent".
func (s *ConfigStore) HasConfigField(scope Scope, key string) bool {
	path, err := s.configPath(scope)
	if err != nil {
		return false
	}
	doc, err := readSettingsDocument(path)
	if err != nil {
		return false
	}
	return gjson.Get(string(doc.jsonBytes), key).Exists()
}

// configWriteLockTimeout caps how long a config write waits for the
// inter-process sidecar lock before failing. Mirrors the 30s budget used by
// cliprovider's acquireMCPConfigLock for the same class of cross-process
// config read-modify-write, so a wedged sibling rush process (debugger
// attached, suspended shell, frozen network mount) cannot indefinitely freeze
// parallel runs that share the same rush.json.
//
// This is the budget for explicit, user-triggered writes (SetConfigFields,
// RemoveConfigField/RemoveProviderAPIKey called outside of Load/reload).
// Callers that run INSIDE Load/reloadFromDiskLocked while holding publishMu
// must NOT use this timeout directly — see internalConfigWriteLockTimeout.
const configWriteLockTimeout = 30 * time.Second

// internalConfigWriteLockTimeout bounds config writes issued from inside
// configureProviders (e.g. the "drop legacy providers.anthropic OAuth
// config" cleanup), which runs synchronously inside Load and
// reloadFromDiskLocked while publishMu is held for the whole call. Regressed
// by b53cbf3d: before that commit this was a same-process disk write with no
// cross-process lock and completed in milliseconds; after it, the same call
// can block for up to configWriteLockTimeout (30s) on the inter-process
// sidecar lock if a sibling rush process holds it (or is wedged holding
// it) — and because publishMu is held for the duration, EVERY reader of the
// config store (including app startup via Load, and ReloadFromDisk,
// SetSkipPermissionRequests, CaptureStalenessSnapshot, etc.) stalls too.
//
// A much shorter budget is safe here specifically because this call site
// already tolerates failure: on timeout the on-disk key simply isn't
// removed yet, the in-memory config is still corrected via Providers.Del
// right after (see configureProviders), and the very next successful
// autoReload/Load (this process or a sibling one) will retry the disk
// cleanup. This is a best-effort cache-eviction of a deprecated on-disk
// key, not a user-initiated write that must succeed synchronously.
const internalConfigWriteLockTimeout = 2 * time.Second

// configWriteLockStallLogThreshold is how long withConfigWriteLock waits
// for the inter-process sidecar lock before logging a warning that the wait
// is unusually long. This makes cross-process contention (or a wedged
// sibling process holding the lock) diagnosable — without it, a stalled
// config write just looks like "rush doesn't start" or "rush hangs" with
// no signal pointing at the lock file. Logged regardless of which timeout
// (configWriteLockTimeout or internalConfigWriteLockTimeout) is in effect,
// and regardless of whether the wait eventually succeeds or times out.
const configWriteLockStallLogThreshold = 500 * time.Millisecond

// withConfigWriteLock runs fn while holding BOTH the in-process diskWriteMu
// and an exclusive OS-level lock on a sidecar lock file co-located with the
// config path, waiting up to configWriteLockTimeout for the OS lock. Use
// this for explicit, user-triggered writes. Callers running inside
// Load/reloadFromDiskLocked (i.e. anything reachable from configureProviders
// while publishMu is held) must call withConfigWriteLockCtx with a much
// shorter, caller-supplied timeout instead — see internalConfigWriteLockTimeout.
func (s *ConfigStore) withConfigWriteLock(path string, fn func(configWriteTarget) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), configWriteLockTimeout)
	defer cancel()
	return s.withConfigWriteLockCtx(ctx, path, fn)
}

// withConfigWriteLockCtx is withConfigWriteLock parameterised on the
// inter-process lock wait's deadline via ctx, so callers on a tight budget
// (e.g. the config-cleanup call inside configureProviders, which runs while
// publishMu is held for the entire Load/reloadFromDiskLocked call) can pass
// a much shorter timeout than the default 30s. See
// internalConfigWriteLockTimeout for why a short budget is safe there.
//
// diskWriteMu serialises concurrent goroutines inside THIS process; the OS
// lock (flock on POSIX, LockFileEx on Windows, via filelock.FileLock)
// serialises SEPARATE rush processes that share the same config file — two
// parallel `rush run` sessions on one machine each own a private
// diskWriteMu, so without the OS lock each could read the same pre-write
// file, apply only its own key, and the second atomicWriteFile would
// silently erase the first writer's key (a classic lost-update across
// processes that diskWriteMu alone cannot prevent, since it is not visible
// to the other process). The OS lock is per open file description, so two
// ConfigStore instances in one test (each with its own diskWriteMu) contend
// on it exactly as two real processes would.
//
// Both locks are released before fn returns, so the caller's subsequent
// autoReload (which acquires publishMu via ReloadFromDisk) cannot deadlock
// against a re-entrant caller that already holds publishMu and entered this
// path (e.g. Load → configureProviders → RemoveConfigField). Lock ordering
// is always diskWriteMu → inter-process file lock.
type configWriteTarget struct {
	path         string
	selectedPath string
	lockPath     string
	expected     reloadFileFingerprint
	owner        int
	enforce      bool
}

// resolveConfigWriteTarget pins the physical spelling used for the sidecar
// lock before the lock is acquired. Symlink aliases consequently share one
// sidecar rather than racing on alias.lock and target.lock independently.
func (s *ConfigStore) resolveConfigWriteTarget(path string) (configWriteTarget, error) {
	owner, enforce, err := s.mcpOwnerPolicy(path)
	if err != nil {
		return configWriteTarget{}, err
	}
	_, expected, readErr := readStableConfigFileOwned(path, owner, enforce)
	if readErr != nil && !os.IsNotExist(readErr) {
		return configWriteTarget{}, readErr
	}
	resolved := normalizeReloadPath(path)
	if resolved == "" {
		return configWriteTarget{}, fmt.Errorf("empty config write target")
	}
	// The sidecar lock anchors on the LEGACY JSON spelling so a rush.ktav
	// store contends on rush.json.lock, the same sidecar the pre-Ktav write
	// path used (see configWriteLockAnchor). target.selectedPath stays the
	// physical slot file. The anchor derives from the RESOLVED physical
	// destination (normalizeReloadPath first, Ktav->legacy mapping second),
	// so two symlink aliases of one rush.ktav resolve to the same physical
	// spelling and share one lock.
	return configWriteTarget{path: resolved, selectedPath: normalizeDiscoveryPath(path), lockPath: normalizeReloadPath(configWriteLockAnchor(resolved)) + ".lock", expected: expected, owner: owner, enforce: enforce}, nil
}

// verifyConfigWriteTarget rereads the destination after the sidecar lock is
// acquired. A cooperating writer may have committed a newer inode while this
// caller waited, so the reread becomes the current read-modify-write base.
// The resolved physical path must nevertheless remain the one pinned before
// locking; an alias retarget is rejected.
func verifyConfigWriteTarget(target configWriteTarget) error {
	return verifyConfigTarget(target)
}

func verifyConfigTarget(target configWriteTarget) error {
	_, actual, err := readStableConfigFileOwned(target.selectedPath, target.owner, target.enforce)
	return verifyConfigTargetBinding(target, actual, err)
}

func verifyConfigTargetBindingAfterLock(target configWriteTarget) error {
	runConfigBeforeMCPTargetBindingVerificationHook(&target)
	return verifyConfigTarget(target)
}

// verifyConfigTargetBinding confirms the physical path and discovery chain
// selected before locking are still the ones reached through the logical
// pathname. It deliberately ignores leaf bytes and inode churn: another
// cooperating writer may have replaced the file while this caller waited.
func verifyConfigTargetBinding(target configWriteTarget, actual reloadFileFingerprint, err error) error {
	if err == nil {
		if normalizeReloadPath(target.selectedPath) != target.path {
			return errConfigCommitVerification
		}
		if target.expected.exists && !actual.identity.valid {
			return errConfigCommitVerification
		}
		if target.expected.exists && (actual.aliasChain != target.expected.aliasChain ||
			actual.parentDiscovery != target.expected.parentDiscovery ||
			actual.parentIdentity != target.expected.parentIdentity) {
			return errConfigCommitVerification
		}
		if !target.expected.exists && target.expected.parentIdentity.valid &&
			(actual.parentDiscovery != target.expected.parentDiscovery || actual.parentIdentity != target.expected.parentIdentity) {
			return errConfigCommitVerification
		}
		return nil
	}
	if os.IsNotExist(err) && normalizeReloadPath(target.selectedPath) == target.path {
		if !target.expected.exists && !target.expected.parentIdentity.valid {
			return nil
		}
		if actual.aliasChain == target.expected.aliasChain &&
			actual.parentDiscovery == target.expected.parentDiscovery &&
			actual.parentIdentity == target.expected.parentIdentity {
			return nil
		}
	}
	if normalizeReloadPath(target.selectedPath) != target.path {
		return errConfigCommitVerification
	}
	return fmt.Errorf("%w: reread config target: %v", errConfigCommitVerification, err)
}

func (s *ConfigStore) withConfigWriteLockCtx(ctx context.Context, path string, fn func(configWriteTarget) error) error {
	s.diskWriteMu.Lock()
	defer s.diskWriteMu.Unlock()
	target, err := s.resolveConfigWriteTarget(path)
	if err != nil {
		return fmt.Errorf("failed to resolve config file %q: %w", path, err)
	}

	waitStart := time.Now()
	lock, err := acquireConfigFileLock(ctx, target.lockPath)
	if wait := time.Since(waitStart); wait >= configWriteLockStallLogThreshold {
		// Diagnosability (see configWriteLockStallLogThreshold): without
		// this, a contended or wedged sidecar lock just looks like "rush
		// hangs on startup" with nothing pointing at the lock file.
		slog.Warn("Config write waited unusually long for inter-process lock",
			"path", target.lockPath, "wait", wait, "succeeded", err == nil)
	}
	if err != nil {
		return fmt.Errorf("failed to lock config file %q: %w", path, err)
	}
	// A slot migration can publish a different active spelling while this
	// writer waits for the shared legacy sidecar lock. Re-select the slot
	// only after acquiring that lock, then pin and verify the resulting
	// physical target before invoking the read-modify-write callback.
	selected := writeTargetSlot(path)
	if selected != target.selectedPath {
		updated, resolveErr := s.resolveConfigWriteTarget(selected)
		if resolveErr != nil {
			_ = releaseConfigFileLock(target.lockPath, lock)
			return fmt.Errorf("failed to resolve config file %q after locking: %w", path, resolveErr)
		}
		if updated.lockPath != target.lockPath {
			_ = releaseConfigFileLock(target.lockPath, lock)
			return fmt.Errorf("config write lock anchor changed for %q", path)
		}
		target = updated
	}
	// Migration and writes share the legacy sidecar. If migration was
	// interrupted after publishing Ktav, finish its authenticated transaction
	// here without reacquiring that lock; then resolve the active slot again
	// because recovery may have changed its spelling or fingerprint.
	legacy := legacySlotOf(target.selectedPath)
	if ktavEntryExists(migrationRecordPathFor(legacy)) {
		if err := runKtavMigrationStateLocked(ctx, legacy, s.workingDir); err != nil {
			_ = releaseConfigFileLock(target.lockPath, lock)
			return fmt.Errorf("failed to recover config migration for %q: %w", path, err)
		}
		selected = writeTargetSlot(path)
		updated, resolveErr := s.resolveConfigWriteTarget(selected)
		if resolveErr != nil {
			_ = releaseConfigFileLock(target.lockPath, lock)
			return fmt.Errorf("failed to resolve config file %q after recovery: %w", path, resolveErr)
		}
		if updated.lockPath != target.lockPath {
			_ = releaseConfigFileLock(target.lockPath, lock)
			return fmt.Errorf("config write lock anchor changed for %q after recovery", path)
		}
		target = updated
	}
	if err := verifyConfigWriteTarget(target); err != nil {
		_ = releaseConfigFileLock(target.lockPath, lock)
		return fmt.Errorf("failed to verify config file %q: %w", path, err)
	}
	// Deliberately NOT calling os.Remove on the sidecar after Release: the
	// lock file is left on disk permanently by design, not cleaned up here.
	// A remove-after-release would race a concurrent process that has
	// already reopened/relocked the same path between our unlock and the
	// remove — on POSIX that's harmless (flock is keyed off the inode, so
	// unlinking a path while another process holds an open fd to the old
	// inode doesn't disturb that process's lock), but on Windows it is not:
	// os.OpenFile here does not pass FILE_SHARE_DELETE, so a concurrent
	// holder's open handle would make our os.Remove fail with a sharing
	// violation, and even if it succeeded, deleting the path out from under
	// a process that just resolved it for LockFileEx risks the next opener
	// creating a distinct, unrelated file object at the same path while the
	// old one is still locked — reintroducing exactly the split-brain this
	// lock exists to prevent. Leaving a handful of empty *.lock sidecars
	// next to rush.json is a one-time, bounded cost; deleting them is not.
	defer releaseConfigFileLock(target.lockPath, lock)
	if err := fn(target); err != nil {
		return err
	}
	s.diskWriteGeneration++
	return nil
}

// noteInitialLoadWriteLocked advances Load's expected fingerprint after one
// of its own self-healing writes. The caller holds diskWriteMu, so the exact
// bytes written and the final verification share the same in-process write
// boundary. External edits after this point still fail the final digest check.
func (s *ConfigStore) noteInitialLoadWriteLocked(path string, data []byte) {
	if s.initialLoadFingerprints == nil {
		return
	}
	canonical := normalizeReloadPath(path)
	updated := false
	for candidate := range s.initialLoadFingerprints {
		if normalizeReloadPath(candidate) != canonical {
			continue
		}
		s.initialLoadFingerprints[candidate] = dataFingerprint(candidate, data)
		updated = true
	}
	if !updated {
		s.initialLoadFingerprints[canonical] = dataFingerprint(path, data)
	}
}

// SetConfigField sets an sjson path/value pair in the config file for the
// given scope. Callers with dynamic MCP server names must use the dedicated
// PersistMCP* methods, which treat names as literal JSON keys; this legacy
// path API cannot distinguish a server named "foo.disabled" from the
// "disabled" field of a server named "foo". After a successful write, it
// automatically reloads config to keep in-memory state fresh.
func (s *ConfigStore) SetConfigField(scope Scope, key string, value any) error {
	return s.SetConfigFields(scope, map[string]any{key: value})
}

// SetConfigFields sets multiple key/value pairs in the config file for the
// given scope in a single read-modify-write, so callers that need to persist
// several keys at once (e.g. UpdatePreferredModels) get one atomic on-disk
// write instead of one per key. Keys are applied in sorted order so the
// resulting JSON is deterministic regardless of map iteration order, keeping
// rush.json diffs stable across runs.
//
// The read-modify-write cycle is protected at two levels: an in-process
// diskWriteMu (serialises concurrent goroutines within this ConfigStore) and
// an inter-process OS-level file lock on a path+".lock" sidecar, acquired via
// withConfigWriteLock (backed by filelock.FileLock — flock on POSIX,
// LockFileEx on Windows). The in-process mutex alone cannot prevent two
// separate `rush` processes sharing the same rush.json from each reading
// the pre-write file, applying only their own keys, and the second
// atomicWriteFile silently clobbering the first — the file lock closes that
// cross-process gap. After a successful write, it automatically reloads
// config to keep in-memory state fresh.
func (s *ConfigStore) SetConfigFields(scope Scope, kv map[string]any) error {
	path, err := s.configPath(scope)
	if err != nil {
		return fmt.Errorf("%s config path unavailable: %w", settingsScope(scope), err)
	}
	path = writeTargetSlot(path)

	// Apply keys in sorted order so the on-disk output is deterministic
	// regardless of map iteration order (keeps rush.json diffs stable).
	keys := make([]string, 0, len(kv))
	for key := range kv {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var committedErr error
	var audit *SettingsEvent
	refusedBy := ""

	// withConfigWriteLock serialises the read-modify-write both in-process
	// (diskWriteMu) and across processes (OS lock on path+".lock") — see its
	// doc comment. The lock is released before autoReload below so that
	// autoReload's publishMu acquisition cannot deadlock.
	if err := s.withConfigWriteLock(path, func(target configWriteTarget) error {
		raw, expected, err := readStableConfigFileOwned(target.selectedPath, target.owner, target.enforce)
		var doc settingsDocument
		if err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("failed to read config file: %w", err)
			}
			doc = settingsDocument{jsonBytes: []byte("{}"), format: settingsDocumentFormatOf(target.selectedPath)}
		} else {
			doc, err = decodeSettingsDocument(target.selectedPath, raw, expected)
			if err != nil {
				return err
			}
		}
		allRecent := len(keys) > 0
		for _, key := range keys {
			if !strings.HasPrefix(key, "recent_models.") {
				allRecent = false
				break
			}
		}
		if !allRecent {
			ev := &SettingsEvent{Scope: settingsScope(scope), Path: target.path, Keys: slices.Clone(keys), Models: selectedModelsAudit(keys, kv)}
			audit = ev
			refusedBy, err = s.guardSettingsWrite(target.path, doc.jsonBytes, keys)
			if err != nil {
				return err
			}
		}
		newValue := string(doc.jsonBytes)
		for _, key := range keys {
			newValue, err = sjson.Set(newValue, key, kv[key])
			if err != nil {
				return fmt.Errorf("failed to set config field %s: %w", key, err)
			}
		}
		if touchesModelsKey(keys) {
			newValue, err = applyModelsIntegrity(newValue)
			if err != nil {
				return fmt.Errorf("failed to update model settings signature: %w", err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(target.path), 0o755); err != nil {
			return fmt.Errorf("failed to create config directory %q: %w", path, err)
		}
		written, err := encodeSettingsDocument([]byte(newValue), doc.format)
		if err != nil {
			return err
		}
		if !expected.exists {
			_, expected, err = readStableConfigFileOwned(target.selectedPath, target.owner, target.enforce)
			if err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("failed to verify config file before write: %w", err)
			}
		}
		_, commitErr := commitConfigFile(target.selectedPath, target.path, written, 0o600, expected, target.owner, target.enforce)
		if commitErr != nil {
			if outcome, ok := CommitOutcomeFromError(commitErr); ok && outcome.Committed {
				s.noteInitialLoadWriteLocked(path, written)
				committedErr = commitErr
				return nil
			}
			return fmt.Errorf("failed to write config file: %w", commitErr)
		}
		s.noteInitialLoadWriteLocked(path, written)
		return nil
	}); err != nil {
		if audit != nil {
			finalizeSettingsEvent(audit, refusedBy, err)
			emitSettingsEvent(*audit)
		}
		return err
	}
	if audit != nil {
		finalizeSettingsEvent(audit, refusedBy, committedErr)
		emitSettingsEvent(*audit)
	}
	if committedErr != nil {
		if reloadErr := s.autoReloadAfterWrite(context.Background()); reloadErr != nil {
			slog.Warn("Config file committed but in-memory reconciliation was incomplete", "path", path, "error", reloadErr)
			return errors.Join(committedErr, reloadErr)
		}
		return committedErr
	}

	// Auto-reload to keep in-memory state fresh after config edits. Runs
	// OUTSIDE withConfigWriteLock so its publishMu acquisition cannot
	// deadlock against diskWriteMu/the file lock (see SetConfigFields
	// history). This alone does not make autoReload safe to call from
	// EVERY context: a caller that reaches this point while already
	// holding publishMu itself (Load, via configureSelectedModels ->
	// in-memory mutator -> SetConfigFields) would still hang
	// here, because autoReload's own dedup guard is reloadMu.TryLock(),
	// not publishMu — see Load's doc comment on why it now also holds
	// reloadMu for exactly this reason.
	if err := s.autoReloadAfterWrite(context.Background()); err != nil {
		// Log warning but don't fail the write - disk is already updated.
		slog.Warn("Config file updated but failed to reload in-memory state", "error", err)
	}

	return nil
}

// RemoveConfigField deletes a single key from the config file for the given
// scope (e.g. removing a stored provider API key). Like SetConfigFields, the
// on-disk read-modify-write is protected at two levels: the in-process
// diskWriteMu and an inter-process OS-level file lock on a path+".lock"
// sidecar acquired via withConfigWriteLock (backed by filelock.FileLock —
// flock on POSIX, LockFileEx on Windows), so two separate `rush` processes
// racing to edit the same rush.json cannot silently clobber each other's
// change. After a successful write, it automatically reloads config to keep
// in-memory state fresh.
//
// This waits up to the full configWriteLockTimeout (30s) for the
// inter-process lock. Callers running inside Load/reloadFromDiskLocked
// (i.e. under configureProviders while publishMu is held) must NOT use
// this — use removeConfigFieldBestEffort instead, which bounds the wait
// to internalConfigWriteLockTimeout so a contended/wedged lock cannot stall
// the whole config subsystem. See internalConfigWriteLockTimeout's doc.
func (s *ConfigStore) RemoveConfigField(scope Scope, key string) error {
	path, err := s.configPath(scope)
	if err != nil {
		return fmt.Errorf("%s config path unavailable: %w", settingsScope(scope), err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), configWriteLockTimeout)
	defer cancel()
	if err := s.removeConfigFieldAt(ctx, path, key, true); err != nil {
		if outcome, ok := CommitOutcomeFromError(err); ok && outcome.Committed {
			if reloadErr := s.autoReloadAfterWrite(context.Background()); reloadErr != nil {
				slog.Warn("Config field removal committed but in-memory reconciliation was incomplete", "path", path, "error", reloadErr)
				return errors.Join(err, reloadErr)
			}
		}
		return err
	}

	// Auto-reload to keep in-memory state fresh after config edits.
	// Runs OUTSIDE withConfigWriteLock (see SetConfigFields).
	if err := s.autoReloadAfterWrite(context.Background()); err != nil {
		slog.Warn("Config file updated but failed to reload in-memory state", "error", err)
	}

	return nil
}

// removeConfigFieldAt performs the on-disk read-modify-write for
// RemoveConfigField/removeConfigFieldBestEffort against an already-resolved
// path, bounding the inter-process lock wait to ctx's deadline. Shared so
// both the full-timeout public API and the short-timeout internal caller
// (configureProviders) go through one write implementation.
func (s *ConfigStore) removeConfigFieldAt(ctx context.Context, path, key string, userInitiated bool) error {
	path = writeTargetSlot(path)
	if touchesReservedKey(key) {
		if userInitiated {
			ev := SettingsEvent{Scope: s.settingsScopeForPath(path), Path: path, Keys: []string{key}}
			finalizeSettingsEvent(&ev, "", errSettingsLockKeyReserved)
			emitSettingsEvent(ev)
		}
		return errSettingsLockKeyReserved
	}
	// Skip acquiring the write lock entirely when there is no config file to
	// edit: withConfigWriteLockCtx creates a path+".lock" sidecar (and its
	// parent directory) as a side effect of acquiring the OS-level lock, so
	// without this check, removing a field from a config that was never
	// written (a clean install, or a scope whose file genuinely doesn't
	// exist) would leave behind an empty *.lock file and directory for no
	// reason. There is nothing to delete from a nonexistent file, so this is
	// a legitimate no-op rather than an error.
	//
	// This check races benignly against a concurrent process creating path:
	// if that happens in the narrow window between this Stat and the lock
	// acquisition below, this call simply misses removing the key this time
	// (the same best-effort semantics removeConfigFieldBestEffort already
	// documents) rather than erroring or corrupting anything.
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to stat config file: %w", err)
	}

	var committedErr error
	var audit *SettingsEvent
	refusedBy := ""
	if err := s.withConfigWriteLockCtx(ctx, path, func(target configWriteTarget) error {
		raw, expected, err := readStableConfigFileOwned(target.selectedPath, target.owner, target.enforce)
		if err != nil {
			return fmt.Errorf("failed to read config file: %w", err)
		}
		doc, err := decodeSettingsDocument(target.selectedPath, raw, expected)
		if err != nil {
			return err
		}
		if userInitiated && !strings.HasPrefix(key, "recent_models.") {
			audit = &SettingsEvent{Scope: s.settingsScopeForPath(target.path), Path: target.path, Keys: []string{key}}
		}
		refusedBy, err = s.guardSettingsWrite(target.path, doc.jsonBytes, []string{key})
		if err != nil {
			return err
		}
		newValue, err := sjson.Delete(string(doc.jsonBytes), key)
		if err != nil {
			return fmt.Errorf("failed to delete config field %s: %w", key, err)
		}
		if settingsKeyFirstComponent(key) == "models" {
			newValue, err = applyModelsIntegrity(newValue)
			if err != nil {
				return fmt.Errorf("failed to update model settings signature: %w", err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(target.path), 0o755); err != nil {
			return fmt.Errorf("failed to create config directory %q: %w", path, err)
		}
		written, err := encodeSettingsDocument([]byte(newValue), doc.format)
		if err != nil {
			return err
		}
		_, commitErr := commitConfigFile(target.selectedPath, target.path, written, 0o600, expected, target.owner, target.enforce)
		if commitErr != nil {
			if outcome, ok := CommitOutcomeFromError(commitErr); ok && outcome.Committed {
				s.noteInitialLoadWriteLocked(path, written)
				committedErr = commitErr
				return nil
			}
			return fmt.Errorf("failed to write config file: %w", commitErr)
		}
		s.noteInitialLoadWriteLocked(path, written)
		return nil
	}); err != nil {
		if audit != nil {
			finalizeSettingsEvent(audit, refusedBy, err)
			emitSettingsEvent(*audit)
		}
		return err
	}
	if audit != nil {
		finalizeSettingsEvent(audit, refusedBy, committedErr)
		emitSettingsEvent(*audit)
	}
	return committedErr
}

// removeConfigFieldBestEffort deletes a single key from the config file for
// the given scope, bounding the inter-process lock wait to
// internalConfigWriteLockTimeout instead of the full configWriteLockTimeout.
//
// Use this ONLY from call paths that run inside Load/reloadFromDiskLocked
// while publishMu is held for the whole call (currently: the "drop legacy
// providers.anthropic OAuth config" cleanup in configureProviders). Because
// publishMu gates every reader of the config store (ReloadFromDisk,
// SetSkipPermissionRequests, CaptureStalenessSnapshot, and app startup via
// Load itself), a stall here — waiting on a sidecar lock held by a
// contended or wedged sibling rush process — would freeze the whole config
// subsystem for as long as the wait budget allows. Regressed by b53cbf3d,
// which replaced a same-process, lock-free disk write with the
// cross-process-locked withConfigWriteLock path without adjusting the
// timeout for callers running under publishMu.
//
// Does NOT call autoReload — unlike RemoveConfigField, this is always
// called from inside Load/reloadFromDiskLocked, where publishMu is held for
// the whole call but reloadMu is NOT. autoReload's redundant-reload dedup is
// reloadMu.TryLock() (see the reloadMu field doc), which would SUCCEED here
// (nothing holds reloadMu), so calling autoReload from this path would be a
// genuine, deadlocking re-entrant call — not a safe no-op — since it would
// proceed into buildAndPublishReload and try to acquire publishMu on the
// same goroutine that already holds it. This function must never call
// autoReload for exactly that reason. The in-memory removal is instead
// handled by the caller via Providers.Del immediately after, and any
// process's next successful reload re-reads the on-disk state.
//
// On error (including timeout), the error is logged and swallowed: the
// caller already tolerates this key not being removed from disk yet — the
// in-memory state is corrected regardless, and the next successful
// Load/reload (in this process or a sibling one) retries the disk cleanup.
func (s *ConfigStore) removeConfigFieldBestEffort(scope Scope, key string) {
	path, err := s.configPath(scope)
	if err != nil {
		slog.Warn("Skipping best-effort config field removal: no path for scope", "key", key, "error", err)
		return
	}

	ctx, cancel := configContextWithTimeout(context.Background(), internalConfigWriteLockTimeout)
	defer cancel()
	if err := s.removeConfigFieldAt(ctx, path, key, false); err != nil {
		if errors.Is(err, ErrSettingsLocked) {
			slog.Debug("Best-effort config field removal was refused by settings lock")
			return
		}
		slog.Warn("Best-effort config field removal did not complete; will retry on next reload",
			"key", key, "path", path, "error", err)
	}
}
