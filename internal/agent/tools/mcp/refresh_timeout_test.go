package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestRefreshWorkerRequestTimeoutDoesNotBlockOtherServer(t *testing.T) {
	const stalledName = "refresh-stalled-request"
	const healthyName = "refresh-healthy-request"
	stalledStarted := make(chan struct{})
	stalledCanceled := make(chan struct{})
	releaseStalled := make(chan struct{})
	var stallRefresh atomic.Bool
	stalled := mcp.NewServer(&mcp.Implementation{Name: "refresh-stalled"}, nil)
	mcp.AddTool(stalled, &mcp.Tool{Name: "stalled-tool"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	stalled.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "tools/list" && stallRefresh.Load() {
				close(stalledStarted)
				// releaseStalled keeps a regressed timeout from wedging the
				// server's Close behind a handler that never returns.
				select {
				case <-ctx.Done():
					close(stalledCanceled)
				case <-releaseStalled:
				}
				return nil, errors.New("stalled list request ended")
			}
			return next(ctx, method, request)
		}
	})
	stalledHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return stalled }, nil))
	cleanupTestMCPServer(t, stalled, stalledHTTP)
	t.Cleanup(func() { close(releaseStalled) })

	healthy := mcp.NewServer(&mcp.Implementation{Name: "refresh-healthy"}, nil)
	mcp.AddTool(healthy, &mcp.Tool{Name: "healthy-tool"}, func(context.Context, *mcp.CallToolRequest, any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{}, nil, nil
	})
	healthyHTTP := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return healthy }, nil))
	cleanupTestMCPServer(t, healthy, healthyHTTP)

	store := persistedMCPStore(t, stalledName, stalledHTTP.URL, false)
	_, ok := store.UpdateMCP(stalledName, func(cfg *config.MCPConfig) { cfg.Timeout = 1 })
	require.True(t, ok)
	store.AddMCP(healthyName, config.MCPConfig{Type: config.MCPHttp, URL: healthyHTTP.URL})
	owner, err := Acquire()
	require.NoError(t, err)
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, owner.Close(closeCtx))
	}()

	for _, name := range []string{stalledName, healthyName} {
		err := InitializeSingle(context.Background(), name, store)
		require.NoError(t, err)
	}
	stalledSession, ok := sessions.Get(stalledName)
	require.True(t, ok)
	healthySession, ok := sessions.Get(healthyName)
	require.True(t, ok)
	stalledAdmission, err := owner.admitServer(context.Background(), store, stalledName, false)
	require.NoError(t, err)
	defer stalledAdmission.done()
	healthyAdmission, err := owner.admitServer(context.Background(), store, healthyName, false)
	require.NoError(t, err)
	defer healthyAdmission.done()
	stalledAdmission.committed = true
	stalledAdmission.committedName = stalledName
	stalledAdmission.committedEpoch = stalledAdmission.epoch
	stalledAdmission.publishedSession = stalledSession
	stallRefresh.Store(true)
	allTools.Del(healthyName)
	healthyAdmission.committed = true
	healthyAdmission.committedName = healthyName
	healthyAdmission.committedEpoch = healthyAdmission.epoch
	healthyAdmission.publishedSession = healthySession

	lifecycleMu.Lock()
	owner.refreshPending[refreshKeyForRequest(refreshRequest{kind: refreshToolsKind, name: stalledName, admission: stalledAdmission})] = refreshRequest{
		kind: refreshToolsKind, name: stalledName, cfg: store, admission: stalledAdmission,
	}
	lifecycleMu.Unlock()
	owner.signalRefresh()

	select {
	case <-stalledStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("stalled list request did not start")
	}
	lifecycleMu.Lock()
	owner.refreshPending[refreshKeyForRequest(refreshRequest{kind: refreshToolsKind, name: healthyName, admission: healthyAdmission})] = refreshRequest{
		kind: refreshToolsKind, name: healthyName, cfg: store, admission: healthyAdmission,
	}
	lifecycleMu.Unlock()
	select {
	case <-stalledCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("refresh request context was not canceled at its timeout")
	}
	require.Eventually(t, func() bool {
		return GetServerToolNames(healthyName) != nil
	}, 3*time.Second, 5*time.Millisecond, "unrelated server refresh did not proceed after timeout")
}
