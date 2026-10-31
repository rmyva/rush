package mcp

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestEnableCanceledAfterAdmissionCreationReleasesInitForClose(t *testing.T) {
	store := isolatedMCPStore(t)
	const name = "enable-cancel-after-admission"
	require.NoError(t, store.PersistMCPConfig(config.ScopeGlobal, name, config.MCPConfig{
		Type: config.MCPStdio, Command: name, Disabled: true,
	}))
	owner, err := Acquire()
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var initialized atomic.Bool
	err = enableServerWithPersistenceAndInitializerForOwner(owner, ctx, store, name,
		func(cfg *config.ConfigStore, scope config.Scope, name string, pending *config.MCPConfig) (config.MCPMutationResult, error) {
			result, persistErr := enableMCPConfig(cfg, scope, name, pending)
			if persistErr == nil {
				// Cancel after the durable enable succeeds but before admission creation
				// and its validity check in the caller.
				cancel()
			}
			return result, persistErr
		},
		func(context.Context, *config.ConfigStore, string, config.MCPConfig, config.VariableResolver, *serverAdmission) error {
			initialized.Store(true)
			return nil
		},
	)
	require.ErrorIs(t, err, config.ErrMCPMutationStale)
	require.False(t, initialized.Load(), "canceled admission must not launch initialization")

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	closeDone := make(chan error, 1)
	go func() { closeDone <- owner.Close(closeCtx) }()
	select {
	case closeErr := <-closeDone:
		require.NoError(t, closeErr)
	case <-closeCtx.Done():
		t.Fatal("Owner.Close exceeded 5 seconds after canceled enable")
	}
}
