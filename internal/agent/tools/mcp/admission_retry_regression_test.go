package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

type retryCandidate struct {
	server    *mcp.Server
	http      *httptest.Server
	started   chan struct{}
	release   chan struct{}
	closed    chan struct{}
	stop      chan struct{}
	tokens    chan string
	startOnce sync.Once
	closeOnce sync.Once
	count     atomic.Int32
}

type renewalEndpoint struct {
	server           *mcp.Server
	http             *httptest.Server
	candidateStarted chan struct{}
	releaseCandidate chan struct{}
	candidateOnce    sync.Once
	blockCandidate   atomic.Bool
	failPing         atomic.Bool
	initializeCnt    atomic.Int32
}

func newRenewalEndpoint(t *testing.T, toolName string) *renewalEndpoint {
	t.Helper()
	endpoint := &renewalEndpoint{
		server:           mcp.NewServer(&mcp.Implementation{Name: toolName}, nil),
		candidateStarted: make(chan struct{}),
		releaseCandidate: make(chan struct{}),
	}
	mcp.AddTool(endpoint.server, &mcp.Tool{Name: toolName}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	endpoint.server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "initialize" {
				endpoint.initializeCnt.Add(1)
				if endpoint.blockCandidate.Load() {
					endpoint.candidateOnce.Do(func() { close(endpoint.candidateStarted) })
					select {
					case <-endpoint.releaseCandidate:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
			}
			if method == "ping" && endpoint.failPing.Load() {
				return nil, context.Canceled
			}
			return next(ctx, method, request)
		}
	})
	endpoint.http = httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return endpoint.server
	}, nil))
	cleanupTestMCPServer(t, endpoint.server, endpoint.http)
	return endpoint
}

func newRetryCandidate(t *testing.T, toolName string, blocked bool) *retryCandidate {
	t.Helper()
	candidate := &retryCandidate{
		server:  mcp.NewServer(&mcp.Implementation{Name: toolName}, nil),
		started: make(chan struct{}),
		release: make(chan struct{}),
		closed:  make(chan struct{}),
		stop:    make(chan struct{}),
		tokens:  make(chan string, 16),
	}
	t.Cleanup(func() { close(candidate.stop) })
	mcp.AddTool(candidate.server, &mcp.Tool{Name: toolName}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	candidate.server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if blocked && method == "tools/list" {
				go func() {
					// Lives until the request is cancelled or the test ends: a fixed
					// cap races with disk-bound config persists on loaded Windows runners.
					select {
					case <-ctx.Done():
						candidate.closeOnce.Do(func() { close(candidate.closed) })
					case <-candidate.stop:
					}
				}()
			}
			if method == "initialize" {
				candidate.count.Add(1)
			}
			if blocked && method == "tools/list" {
				candidate.startOnce.Do(func() { close(candidate.started) })
				select {
				case <-candidate.release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return next(ctx, method, request)
		}
	})
	delegate := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return candidate.server
	}, nil)
	candidate.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token := r.Header.Get("X-Retry-Token"); token != "" {
			select {
			case candidate.tokens <- token:
			default:
			}
		}
		delegate.ServeHTTP(w, r)
	}))
	cleanupTestMCPServer(t, candidate.server, candidate.http)
	return candidate
}

func TestInitializeCrossStoreStaleAdmissionRetriesFreshDiskSnapshot(t *testing.T) {
	runCrossStoreAdmissionRetry(t, false)
}

func TestInitializeSingleCrossStoreStaleAdmissionRetriesFreshDiskSnapshot(t *testing.T) {
	runCrossStoreAdmissionRetry(t, true)
}

func TestInitializeSlowCandidateSurvivesNoOpReload(t *testing.T) {
	const name = "no-op-reload-admission"
	candidate := newRetryCandidate(t, "stable-tool", true)
	store := persistedMCPStore(t, name, candidate.http.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	done := make(chan struct{})
	go func() {
		owner.Initialize(context.Background(), nil, store, false)
		close(done)
	}()
	awaitMCPSignal(t, candidate.started)
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	close(candidate.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Initialize did not finish after no-op reload")
	}
	require.Equal(t, int32(1), candidate.count.Load())
	require.Equal(t, []string{"stable-tool"}, GetServerToolNames(name))
	require.Equal(t, StateConnected, mustState(t, name).State)
}

func TestInitializeSingleSlowCandidateSurvivesNoOpReload(t *testing.T) {
	const name = "single-no-op-reload-admission"
	candidate := newRetryCandidate(t, "single-stable-tool", true)
	store := persistedMCPStore(t, name, candidate.http.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	done := make(chan error, 1)
	go func() { done <- InitializeSingle(context.Background(), name, store) }()
	awaitMCPSignal(t, candidate.started)
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	close(candidate.release)
	require.NoError(t, awaitMCPError(t, done))
	require.Equal(t, int32(1), candidate.count.Load())
	require.Equal(t, []string{"single-stable-tool"}, GetServerToolNames(name))
	require.Equal(t, StateConnected, mustState(t, name).State)
}

func TestInitializeRejectsSemanticResolverChangeAndRetriesFreshCandidate(t *testing.T) {
	runSemanticResolverRetry(t, false)
}

func TestInitializeSingleRejectsSemanticResolverChangeAndRetriesFreshCandidate(t *testing.T) {
	runSemanticResolverRetry(t, true)
}

func TestInitializeRejectsCommandResolverChangeWithoutEnvironmentChange(t *testing.T) {
	runCommandResolverRetry(t, false)
}

func TestInitializeSingleRejectsCommandResolverChangeWithoutEnvironmentChange(t *testing.T) {
	runCommandResolverRetry(t, true)
}

func TestInitializeRejectsSingleQuotedCommandResolverChangeWithoutEnvironmentChange(t *testing.T) {
	runCommandResolverRetryWithExpression(t, false, func(path string) string {
		return "'$(read token < '" + path + "'; printf '%s' \"$token\")'"
	}, "'old'", "'new'")
}

func TestInitializeSingleRejectsSingleQuotedCommandResolverChangeWithoutEnvironmentChange(t *testing.T) {
	runCommandResolverRetryWithExpression(t, true, func(path string) string {
		return "'$(read token < '" + path + "'; printf '%s' \"$token\")'"
	}, "'old'", "'new'")
}

func TestInitializeRejectsBackquoteCommandResolverChangeWithoutEnvironmentChange(t *testing.T) {
	runCommandResolverRetryWithExpression(t, false, func(path string) string {
		return "`read token < '" + path + "'; printf '%s' \"$token\"`"
	}, "old", "new")
}

func TestInitializeSingleRejectsBackquoteCommandResolverChangeWithoutEnvironmentChange(t *testing.T) {
	runCommandResolverRetryWithExpression(t, true, func(path string) string {
		return "`read token < '" + path + "'; printf '%s' \"$token\"`"
	}, "old", "new")
}

func TestInitializeReloadsStaleDisabledSnapshotBeforeAdmission(t *testing.T) {
	endpoint := newRetryCandidate(t, "reloaded-disabled-tool", false)
	store := persistedMCPStore(t, "reloaded-disabled", endpoint.http.URL, true)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	require.NoError(t, contender.PersistMCPFieldsExact(config.ScopeGlobal, "reloaded-disabled", map[string]any{
		"disabled": false,
	}))
	runStaleSkippedInitialize(t, store, "reloaded-disabled", "reloaded-disabled-tool", false)
}

func TestInitializeReloadsStaleCLISkippedSnapshotBeforeAdmission(t *testing.T) {
	endpoint := newRetryCandidate(t, "reloaded-cli-tool", false)
	store := persistedMCPStore(t, "reloaded-cli", endpoint.http.URL, false)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	require.NoError(t, contender.PersistMCPFieldsExact(config.ScopeGlobal, "reloaded-cli", map[string]any{
		"enabled_in_cli": true,
	}))
	runStaleSkippedInitialize(t, store, "reloaded-cli", "reloaded-cli-tool", true)
}

func TestInitializeReloadsStaleSkippedSnapshotAfterCrossStoreRemoval(t *testing.T) {
	const name = "reloaded-removed"
	endpoint := newRetryCandidate(t, "never-connected-tool", false)
	store := persistedMCPStore(t, name, endpoint.http.URL, true)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	require.NoError(t, contender.PersistRemoveMCPConfigExact(config.ScopeGlobal, name))

	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	done := make(chan struct{})
	go func() {
		owner.Initialize(context.Background(), nil, store, false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Initialize did not finish after reloading a removed skipped server")
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, WaitForInit(waitCtx))
	require.False(t, store.SnapshotMCPAdmission(name).Exists)
	require.False(t, hasSession(name))
	require.Empty(t, GetServerToolNames(name))
	require.Equal(t, StateDisabled, mustState(t, name).State)
	lifecycleMu.Lock()
	require.Zero(t, owner.initCount)
	require.Zero(t, owner.fullInitCount)
	lifecycleMu.Unlock()
}

func TestInitializeReloadsStaleSkippedSnapshotAfterCrossStoreReplacement(t *testing.T) {
	const name = "reloaded-replaced"
	oldEndpoint := newRetryCandidate(t, "old-skipped-tool", false)
	newEndpoint := newRetryCandidate(t, "new-replaced-tool", false)
	store := persistedMCPStore(t, name, oldEndpoint.http.URL, true)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	require.NoError(t, contender.PersistMCPFieldsExact(config.ScopeGlobal, name, map[string]any{
		"url":      newEndpoint.http.URL,
		"disabled": false,
	}))
	runStaleSkippedInitialize(t, store, name, "new-replaced-tool", false)
	require.Zero(t, oldEndpoint.count.Load())
}

func runStaleSkippedInitialize(
	t *testing.T,
	store *config.ConfigStore,
	name string,
	toolName string,
	restrictToCLIEnabled bool,
) {
	t.Helper()
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	done := make(chan struct{})
	go func() {
		owner.Initialize(context.Background(), nil, store, restrictToCLIEnabled)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Initialize did not finish after reloading a stale skipped snapshot")
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, WaitForInit(waitCtx))
	require.Equal(t, StateConnected, mustState(t, name).State)
	require.True(t, hasSession(name))
	require.Equal(t, []string{toolName}, GetServerToolNames(name))
	lifecycleMu.Lock()
	require.Zero(t, owner.initCount)
	require.Zero(t, owner.fullInitCount)
	lifecycleMu.Unlock()
}

func TestInitializeClosesBarrierBeforeInitWaitGroup(t *testing.T) {
	store := config.NewTestStore(&config.Config{MCP: config.MCPs{
		"barrier-order": {Type: config.MCPStdio, Command: "unused", Disabled: true},
	}})
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	barrierResult := make(chan error, 1)
	mcpInitTestHooks.Lock()
	previous := mcpInitTestHooks.afterWaitGroupDone
	mcpInitTestHooks.afterWaitGroupDone = func() {
		waitCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		barrierResult <- WaitForInit(waitCtx)
		cancel()
	}
	mcpInitTestHooks.Unlock()
	t.Cleanup(func() {
		mcpInitTestHooks.Lock()
		mcpInitTestHooks.afterWaitGroupDone = previous
		mcpInitTestHooks.Unlock()
	})

	owner.Initialize(context.Background(), nil, store, false)
	select {
	case err := <-barrierResult:
		require.NoError(t, err)
	default:
		t.Fatal("init wait-group ordering hook did not run")
	}
}

// forcedStaleWatchdog only catches a retry loop that never ends. Each of the
// maxAdmissionRetries attempts durably persists the config from the hook, so
// the wall-clock cost is disk-bound (seconds on a busy machine); the retry bound
// itself is asserted through the exact hook call count.
const forcedStaleWatchdog = 30 * time.Second

func TestInitializeBoundsForcedStaleSkippedAdmission(t *testing.T) {
	runForcedStaleSkippedInitialize(t, false)
}

func TestInitializeSingleBoundsForcedStaleSkippedAdmission(t *testing.T) {
	runForcedStaleSkippedInitialize(t, true)
}

func runForcedStaleSkippedInitialize(t *testing.T, single bool) {
	t.Helper()
	name := "forced-stale-skipped"
	if single {
		name += "-single"
	}
	store := persistedMCPStore(t, name, "http://forced-stale.example", true)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	fakeSession := &ClientSession{}
	sessions.Set(name, fakeSession)
	allTools.Set(name, []*Tool{{Name: "stale-tool"}})
	allPrompts.Set(name, []*Prompt{{Name: "stale-prompt"}})
	allResources.Set(name, []*Resource{{Name: "stale-resource", URI: "stale://resource"}})
	setState(name, StateConnected, nil, fakeSession, Counts{Tools: 1})

	var calls atomic.Int32
	hookErrors := make(chan error, maxAdmissionRetries)
	mcpInitTestHooks.Lock()
	previous := mcpInitTestHooks.beforeSkippedAdmission
	mcpInitTestHooks.beforeSkippedAdmission = func(hookName string, _ config.MCPAdmissionSnapshot) {
		if hookName != name {
			return
		}
		attempt := calls.Add(1)
		if persistErr := contender.PersistMCPFieldsExact(config.ScopeGlobal, name, map[string]any{
			"timeout": int(attempt),
		}); persistErr != nil {
			hookErrors <- persistErr
		}
	}
	mcpInitTestHooks.Unlock()
	t.Cleanup(func() {
		mcpInitTestHooks.Lock()
		mcpInitTestHooks.beforeSkippedAdmission = previous
		mcpInitTestHooks.Unlock()
	})

	started := time.Now()
	if single {
		done := make(chan error, 1)
		go func() { done <- InitializeSingle(context.Background(), name, store) }()
		select {
		case err := <-done:
			require.ErrorIs(t, err, config.ErrMCPMutationStale)
		case <-time.After(forcedStaleWatchdog):
			t.Fatal("InitializeSingle did not terminate after bounded stale retries")
		}
	} else {
		done := make(chan struct{})
		go func() {
			owner.Initialize(context.Background(), nil, store, false)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(forcedStaleWatchdog):
			t.Fatal("Initialize did not terminate after bounded stale retries")
		}
	}
	require.Less(t, time.Since(started), forcedStaleWatchdog)
	require.Equal(t, int32(maxAdmissionRetries), calls.Load())
	select {
	case persistErr := <-hookErrors:
		require.NoError(t, persistErr)
	default:
	}
	state, ok := GetState(name)
	require.True(t, ok)
	require.Equal(t, StateError, state.State)
	require.ErrorIs(t, state.Error, config.ErrMCPMutationStale)
	require.False(t, hasSession(name))
	require.Empty(t, GetServerToolNames(name))
	_, hasPrompts := allPrompts.Get(name)
	require.False(t, hasPrompts)
	_, hasResources := allResources.Get(name)
	require.False(t, hasResources)
	lifecycleMu.Lock()
	require.Zero(t, owner.initCount)
	require.Zero(t, owner.fullInitCount)
	lifecycleMu.Unlock()
	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, WaitForInit(waitCtx))
}

func TestAdmissionRejectsDirectProjectRushSourceReplaceBeforePublication(t *testing.T) {
	// Legacy JSON semantics: the test replaces the project rush.json source
	// in place and requires the admission to observe the change. With Ktav
	// auto-migration on, config.Load renames rush.json to rush.ktav, so the
	// fixture must stay in the legacy format.
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)
	runDirectAdmissionSourceMutation(t, false, false)
}

func TestAdmissionRejectsDirectExternalMCPSourceRemovalBeforePublication(t *testing.T) {
	runDirectAdmissionSourceMutation(t, true, true)
}

func runDirectAdmissionSourceMutation(t *testing.T, external, remove bool) {
	t.Helper()
	name := "direct-source-admission"
	if external {
		name += "-external"
	} else {
		name += "-project"
	}
	if remove {
		name += "-remove"
	}
	candidate := newRetryCandidate(t, name+"-tool", false)
	store := isolatedMCPStore(t)
	path := filepath.Join(store.WorkingDir(), "rush.json")
	key := "mcp"
	if external {
		path = filepath.Join(store.WorkingDir(), ".mcp.json")
		key = "mcpServers"
	}
	write := func(url string) {
		data, err := json.Marshal(map[string]any{key: map[string]any{
			name: map[string]any{"type": "http", "url": url},
		}})
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, data, 0o600))
	}
	write(candidate.http.URL)
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	events := SubscribeEvents(eventsCtx)
	var calls atomic.Int32
	mcpInitTestHooks.Lock()
	previous := mcpInitTestHooks.beforeAdmissionTurn
	mcpInitTestHooks.beforeAdmissionTurn = func(hookName string) {
		if hookName != name {
			return
		}
		attempt := calls.Add(1)
		if remove {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Errorf("remove source: %v", err)
			}
			return
		}
		write(fmt.Sprintf("%s?replacement=%d", candidate.http.URL, attempt))
	}
	mcpInitTestHooks.Unlock()
	defer func() {
		mcpInitTestHooks.Lock()
		mcpInitTestHooks.beforeAdmissionTurn = previous
		mcpInitTestHooks.Unlock()
	}()

	err = InitializeSingle(context.Background(), name, store)
	require.Error(t, err)
	require.GreaterOrEqual(t, calls.Load(), int32(1))
	_, ok := sessions.Get(name)
	require.False(t, ok)
	require.Empty(t, GetServerToolNames(name))
	if state, stateOK := GetState(name); stateOK {
		require.NotEqual(t, StateConnected, state.State)
	}
	for {
		select {
		case event := <-events:
			if event.Payload.Name == name && event.Payload.State == StateConnected {
				t.Fatalf("stale admission published a connected event: %#v", event)
			}
		default:
			return
		}
	}
}

func TestGetOrRenewClientRetriesCrossStoreReplacement(t *testing.T) {
	runCrossStoreRenewalRecovery(t, false, false)
}

func TestGetOrRenewClientFailsClosedAfterCrossStoreRemoval(t *testing.T) {
	runCrossStoreRenewalRecovery(t, true, false)
}

func TestGetOrRenewClientBoundsCrossStoreChurn(t *testing.T) {
	runCrossStoreRenewalRecovery(t, false, true)
}

func runCrossStoreRenewalRecovery(t *testing.T, remove, churn bool) {
	t.Helper()
	const name = "cross-store-renewal-recovery"
	oldEndpoint := newRenewalEndpoint(t, "old-renewal-tool")
	newEndpoint := newRenewalEndpoint(t, "new-renewal-tool")
	store := persistedMCPStore(t, name, oldEndpoint.http.URL, false)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()
	require.NoError(t, InitializeSingle(context.Background(), name, store))

	oldEndpoint.failPing.Store(true)
	oldEndpoint.blockCandidate.Store(true)
	newEndpoint.blockCandidate.Store(churn)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	done := make(chan struct {
		lease *clientLease
		err   error
	}, 1)
	go func() {
		lease, renewErr := getOrRenewClient(context.Background(), store, name)
		done <- struct {
			lease *clientLease
			err   error
		}{lease: lease, err: renewErr}
	}()
	select {
	case <-oldEndpoint.candidateStarted:
	case result := <-done:
		t.Fatalf("renewal ended before candidate barrier: %v", result.err)
	case <-time.After(5 * time.Second):
		t.Fatal("renewal candidate did not reach initialize")
	}
	if remove {
		require.NoError(t, contender.PersistRemoveMCPConfigExact(config.ScopeGlobal, name))
	} else {
		require.NoError(t, contender.PersistMCPFieldsExact(config.ScopeGlobal, name, map[string]any{
			"url": newEndpoint.http.URL,
		}))
	}
	close(oldEndpoint.releaseCandidate)
	if churn {
		select {
		case <-newEndpoint.candidateStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("fresh recovery candidate did not reach initialize")
		}
		require.NoError(t, contender.PersistRemoveMCPConfigExact(config.ScopeGlobal, name))
		close(newEndpoint.releaseCandidate)
	}

	select {
	case result := <-done:
		if remove || churn {
			require.Error(t, result.err)
			require.Nil(t, result.lease)
			require.Empty(t, GetServerToolNames(name))
			require.False(t, hasSession(name))
			require.Equal(t, int32(2), oldEndpoint.initializeCnt.Load())
			if remove {
				require.Zero(t, newEndpoint.initializeCnt.Load())
			}
			if churn {
				require.Equal(t, int32(1), newEndpoint.initializeCnt.Load())
			}
			lease := serverLeaseFor(name)
			lease.Lock()
			renewing := lease.renewing
			lease.Unlock()
			require.False(t, renewing)
			lifecycleMu.Lock()
			initCount := owner.initCount
			lifecycleMu.Unlock()
			require.Zero(t, initCount)
			return
		}
		require.NoError(t, result.err)
		require.NotNil(t, result.lease)
		result.lease.close()
	case <-time.After(5 * time.Second):
		t.Fatal("renewal recovery did not finish")
	}
	require.Equal(t, int32(2), oldEndpoint.initializeCnt.Load(), "the stale old candidate must be bounded to one retry")
	require.Equal(t, int32(1), newEndpoint.initializeCnt.Load(), "recovery must connect exactly one fresh candidate")
	require.Equal(t, []string{"new-renewal-tool"}, GetServerToolNames(name))
	require.True(t, hasSession(name))
}

func runSemanticResolverRetry(t *testing.T, single bool) {
	const name = "resolver-change-admission"
	t.Setenv("RUSH_MCP_RETRY_HEADER", "old")
	candidate := newRetryCandidate(t, "resolver-tool", true)
	store := persistedMCPStore(t, name, candidate.http.URL, false)
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "mcp."+name+".headers", map[string]string{
		"X-Retry-Token": "$RUSH_MCP_RETRY_HEADER",
	}))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	done := make(chan error, 1)
	if single {
		go func() { done <- InitializeSingle(context.Background(), name, store) }()
	} else {
		go func() {
			owner.Initialize(context.Background(), nil, store, false)
			done <- nil
		}()
	}
	awaitMCPSignal(t, candidate.started)
	t.Setenv("RUSH_MCP_RETRY_HEADER", "new")
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	close(candidate.release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("initialization did not finish after resolver change")
	}
	seenOld, seenNew := false, false
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !seenOld || !seenNew {
		select {
		case token := <-candidate.tokens:
			seenOld = seenOld || token == "old"
			seenNew = seenNew || token == "new"
		case <-deadline.C:
			t.Fatal("did not observe both old and new resolver headers")
		}
	}
	require.Equal(t, int32(2), candidate.count.Load(), "the resolver change must reject the old candidate")
	require.Eventually(t, func() bool {
		select {
		case <-candidate.closed:
			return true
		default:
			return false
		}
	}, 5*time.Second, time.Millisecond, "the stale resolver candidate was not closed")
	require.Equal(t, []string{"resolver-tool"}, GetServerToolNames(name))
}

func runCommandResolverRetry(t *testing.T, single bool) {
	runCommandResolverRetryWithExpression(t, single, func(path string) string {
		return "$(read token < '" + path + "'; printf '%s' \"$token\")"
	}, "old", "new")
}

func runCommandResolverRetryWithExpression(
	t *testing.T,
	single bool,
	expression func(string) string,
	oldToken string,
	newToken string,
) {
	t.Helper()
	const name = "command-resolver-change-admission"
	outputPath := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(outputPath, []byte("old"), 0o600))
	candidate := newRetryCandidate(t, "command-resolver-tool", true)
	store := persistedMCPStore(t, name, candidate.http.URL, false)
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "mcp."+name+".headers", map[string]string{
		"X-Retry-Token": expression(filepath.ToSlash(outputPath)),
	}))
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	done := make(chan error, 1)
	if single {
		go func() { done <- InitializeSingle(context.Background(), name, store) }()
	} else {
		go func() {
			owner.Initialize(context.Background(), nil, store, false)
			done <- nil
		}()
	}
	awaitMCPSignal(t, candidate.started)
	require.NoError(t, os.WriteFile(outputPath, []byte("new"), 0o600))
	require.NoError(t, store.ReloadFromDisk(context.Background()))
	close(candidate.release)
	require.NoError(t, awaitMCPError(t, done))

	seenOld, seenNew := false, false
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !seenOld || !seenNew {
		select {
		case token := <-candidate.tokens:
			seenOld = seenOld || token == oldToken
			seenNew = seenNew || token == newToken
		case <-deadline.C:
			t.Fatal("did not observe both old and new command resolver headers")
		}
	}
	require.Equal(t, int32(2), candidate.count.Load(), "a command resolver reload must reject the old candidate")
}

func runCrossStoreAdmissionRetry(t *testing.T, single bool) {
	t.Helper()
	const name = "cross-store-admission-retry"
	a := newRetryCandidate(t, "a-tool", true)
	b := newRetryCandidate(t, "b-tool", false)
	store := persistedMCPStore(t, name, a.http.URL, false)
	contender, err := config.Init(store.WorkingDir(), store.WorkingDir(), false)
	require.NoError(t, err)
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() { require.NoError(t, owner.Close(context.Background())) }()

	done := make(chan error, 1)
	if single {
		go func() { done <- InitializeSingle(context.Background(), name, store) }()
	} else {
		go func() {
			owner.Initialize(context.Background(), nil, store, false)
			done <- nil
		}()
	}
	select {
	case <-a.started:
	case <-time.After(5 * time.Second):
		t.Fatal("initial candidate did not reach tools/list")
	}
	require.NoError(t, contender.PersistMCPFieldsExact(config.ScopeGlobal, name, map[string]any{
		"url": b.http.URL,
	}))
	close(a.release)
	require.NoError(t, awaitMCPError(t, done))

	require.Eventually(t, func() bool {
		state, ok := GetState(name)
		return ok && state.State == StateConnected && hasSession(name)
	}, 5*time.Second, time.Millisecond)
	require.Equal(t, int32(1), b.count.Load(), "fresh B candidate must publish exactly once")
	require.Equal(t, []string{"b-tool"}, GetServerToolNames(name))
	require.Eventually(t, func() bool {
		select {
		case <-a.closed:
			return true
		default:
			return false
		}
	}, 5*time.Second, time.Millisecond, "stale A candidate was not closed")
	require.Equal(t, int32(1), a.count.Load(), "A must never be initialized again after the stale pin")
}
