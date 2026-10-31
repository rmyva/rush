package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/PHPCraftdream/rush/internal/ktavcodec"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// TestSetConfigFieldsRevalidatesSlotAfterWaitingForMigrationLock reproduces a
// writer choosing rush.json, pausing before its sidecar lock, and resuming
// after another process publishes rush.ktav as the active slot.
func TestSetConfigFieldsRevalidatesSlotAfterWaitingForMigrationLock(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "rush.json")
	legacyBytes := []byte(`{"options":{"debug":false}}`)
	require.NoError(t, os.WriteFile(legacy, legacyBytes, 0o600))
	store := newTestConfigStore(testStoreOpts{globalDataPath: legacy})

	entered := make(chan struct{})
	release := make(chan struct{})
	var pauseOnce sync.Once
	configTestHooks.Lock()
	previousAcquire := configTestHooks.acquireConfigLock
	configTestHooks.acquireConfigLock = func(ctx context.Context, path string) (*filelock.FileLock, error) {
		paused := false
		pauseOnce.Do(func() {
			paused = true
			close(entered)
		})
		if paused {
			<-release
		}
		return filelock.AcquireFileLockContext(ctx, path)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.acquireConfigLock = previousAcquire
		configTestHooks.Unlock()
		select {
		case <-release:
		default:
			close(release)
		}
	})

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- store.SetConfigFields(ScopeGlobal, map[string]any{"options.debug": true})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not reach the pre-lock pause")
	}

	require.NoError(t, runKtavMigrationState(context.Background(), legacy, dir))
	close(release)
	select {
	case err := <-writeDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not finish after release")
	}

	active, err := os.ReadFile(ktavSiblingOf(legacy))
	require.NoError(t, err)
	decoded, err := ktavcodec.DecodeKtav(active)
	require.NoError(t, err, "active slot must remain valid Ktav")
	activeJSON, err := json.Marshal(decoded)
	require.NoError(t, err)
	require.True(t, gjson.Get(string(activeJSON), "options.debug").Bool(),
		"successful write must land in the now-active Ktav generation")
	require.Equal(t, legacyBytes, mustReadConfigSlotRegressionFile(t, legacy), "migration must preserve legacy JSON")
}

func TestSetConfigFieldsRecoversAfterPublishMigrationBeforeWrite(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	legacy := filepath.Join(dir, "rush.json")
	legacyBytes := []byte(`{"options":{"debug":false}}`)
	require.NoError(t, os.WriteFile(legacy, legacyBytes, 0o600))
	store := newTestConfigStore(testStoreOpts{globalDataPath: legacy})

	entered := make(chan struct{})
	release := make(chan struct{})
	var pauseOnce sync.Once
	configTestHooks.Lock()
	previousAcquire := configTestHooks.acquireConfigLock
	configTestHooks.acquireConfigLock = func(ctx context.Context, path string) (*filelock.FileLock, error) {
		paused := false
		pauseOnce.Do(func() { paused = true; close(entered) })
		if paused {
			<-release
		}
		return filelock.AcquireFileLockContext(ctx, path)
	}
	configTestHooks.Unlock()
	t.Cleanup(func() {
		configTestHooks.Lock()
		configTestHooks.acquireConfigLock = previousAcquire
		configTestHooks.Unlock()
		select {
		case <-release:
		default:
			close(release)
		}
	})

	previousStep := ktavMigrationStepHook
	ktavMigrationStepHook = func(step string) error {
		if step == "after-publish" {
			return errKtavMigrationCrashInjected
		}
		return nil
	}
	t.Cleanup(func() { ktavMigrationStepHook = previousStep })

	writeDone := make(chan error, 1)
	go func() { writeDone <- store.SetConfigFields(ScopeGlobal, map[string]any{"options.debug": true}) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not reach the pre-lock pause")
	}
	require.ErrorIs(t, runKtavMigrationState(context.Background(), legacy, dir), errKtavMigrationCrashInjected)
	require.FileExists(t, migrationRecordPathFor(legacy))
	close(release)
	select {
	case err := <-writeDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not finish after recovery")
	}
	require.NoFileExists(t, migrationRecordPathFor(legacy))

	active, err := os.ReadFile(ktavSiblingOf(legacy))
	require.NoError(t, err)
	decoded, err := ktavcodec.DecodeKtav(active)
	require.NoError(t, err)
	activeJSON, err := json.Marshal(decoded)
	require.NoError(t, err)
	require.True(t, gjson.Get(string(activeJSON), "options.debug").Bool())

	fresh, err := Load(dir, dir, false)
	require.NoError(t, err)
	require.True(t, fresh.Config().Options.Debug, "fresh Load must observe the persisted edit")
}

func TestSetConfigFieldsRefusesInvalidPendingMigrationWithoutChangingBytes(t *testing.T) {
	ktavIsolateGlobals(t)
	dir := t.TempDir()
	legacy := filepath.Join(dir, "rush.json")
	require.NoError(t, os.WriteFile(legacy, []byte(`{"options":{"debug":false}}`), 0o600))
	previousStep := ktavMigrationStepHook
	t.Cleanup(func() { ktavMigrationStepHook = previousStep })
	ktavMigrationStepHook = func(step string) error {
		if step == "after-publish" {
			return errKtavMigrationCrashInjected
		}
		return nil
	}
	require.ErrorIs(t, runKtavMigrationState(context.Background(), legacy, dir), errKtavMigrationCrashInjected)

	recordPath := migrationRecordPathFor(legacy)
	require.NoError(t, os.WriteFile(recordPath, []byte(`{"invalid":true}`), 0o600))
	paths := []string{legacy, ktavSiblingOf(legacy), recordPath}
	before := make([][]byte, len(paths))
	for i, path := range paths {
		before[i] = mustReadConfigSlotRegressionFile(t, path)
	}
	store := newTestConfigStore(testStoreOpts{globalDataPath: legacy})
	err := store.SetConfigFields(ScopeGlobal, map[string]any{"options.debug": true})
	require.ErrorIs(t, err, errKtavMigrationRecord)
	for i, path := range paths {
		require.Equal(t, before[i], mustReadConfigSlotRegressionFile(t, path), path)
	}
}

func mustReadConfigSlotRegressionFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}
