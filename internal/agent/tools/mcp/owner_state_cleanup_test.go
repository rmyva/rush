package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/stretchr/testify/require"
)

func TestStandaloneDetachClearsItsCommittedAdmission(t *testing.T) {
	if current := currentOwner(); current != nil {
		require.NoError(t, current.Close(context.Background()))
	}
	installed, err := Acquire()
	require.NoError(t, err)
	standalone, err := AcquireStandalone()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = standalone.Close(context.Background())
		if current := currentOwner(); current != nil {
			_ = current.Close(context.Background())
		}
	})

	const name = "standalone-detach-committed-admission"
	session := &ClientSession{owner: standalone}
	admission := &serverAdmission{owner: standalone, name: name}
	lifecycleMu.Lock()
	standalone.committedAdmissions[name] = admission
	sessions.Set(name, session)
	lifecycleMu.Unlock()

	lease := serverLeaseFor(name)
	lease.Lock()
	detached := detachSessionLocked(name)
	lease.Unlock()

	require.Same(t, session, detached)
	lifecycleMu.Lock()
	got, exists := standalone.committedAdmissions[name]
	lifecycleMu.Unlock()
	require.False(t, exists)
	require.Nil(t, got)
	lifecycleMu.Lock()
	_, installedHasAdmission := installed.committedAdmissions[name]
	lifecycleMu.Unlock()
	require.False(t, installedHasAdmission)
}

func TestInstalledClosePreservesStandaloneDisabledState(t *testing.T) {
	if current := currentOwner(); current != nil {
		require.NoError(t, current.Close(context.Background()))
	}

	installed, err := Acquire()
	require.NoError(t, err)
	standalone, err := AcquireStandalone()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = standalone.Close(context.Background())
		if current := currentOwner(); current != nil {
			_ = current.Close(context.Background())
		}
	})

	const name = "installed-close-preserves-disabled"
	store := config.NewLibraryStore(&config.Config{MCP: config.MCPs{
		name: {Type: config.MCPHttp, URL: "http://127.0.0.1:1", Disabled: true},
	}}, "")
	standalone.Initialize(context.Background(), nil, store, false)
	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, standalone.WaitForInit(waitCtx))
	state, ok := standalone.GetState(name)
	require.True(t, ok)
	require.Equal(t, StateDisabled, state.State)
	require.NoError(t, installed.Close(context.Background()))

	state, ok = standalone.GetState(name)
	require.True(t, ok)
	require.Equal(t, StateDisabled, state.State)

	rolled, err := Acquire()
	require.NoError(t, err)
	require.NoError(t, rolled.Close(context.Background()))
	state, ok = standalone.GetState(name)
	require.True(t, ok)
	require.Equal(t, StateDisabled, state.State)

	require.NoError(t, standalone.Close(context.Background()))
	_, ok = GetState(name)
	require.False(t, ok)
}

func TestOwnerClosePreservesClaimedNoSessionStatesAcrossOrders(t *testing.T) {
	for _, state := range []State{StateDisabled, StateError, StateStarting} {
		for _, standaloneFirst := range []bool{false, true} {
			name := state.String()
			if standaloneFirst {
				name += "-standalone-first"
			} else {
				name += "-installed-first"
			}
			t.Run(name, func(t *testing.T) {
				if current := currentOwner(); current != nil {
					require.NoError(t, current.Close(context.Background()))
				}
				installed, err := Acquire()
				require.NoError(t, err)
				standalone, err := AcquireStandalone()
				require.NoError(t, err)
				t.Cleanup(func() {
					_ = standalone.Close(context.Background())
					if current := currentOwner(); current != nil {
						_ = current.Close(context.Background())
					}
				})

				owner := standalone
				if standaloneFirst {
					owner = installed
				}
				lifecycleMu.Lock()
				owner.serverEpochs[name] = 1
				stateErr := error(nil)
				if state == StateError {
					stateErr = errors.New("claimed error")
				}
				states.Set(name, ClientInfo{Name: name, State: state, Error: stateErr})
				lifecycleMu.Unlock()

				if standaloneFirst {
					require.NoError(t, standalone.Close(context.Background()))
				} else {
					require.NoError(t, installed.Close(context.Background()))
				}
				preserved, ok := GetState(name)
				require.True(t, ok)
				require.Equal(t, state, preserved.State)
				require.Equal(t, stateErr, preserved.Error)

				if standaloneFirst {
					require.NoError(t, installed.Close(context.Background()))
				} else {
					require.NoError(t, standalone.Close(context.Background()))
				}
				_, ok = GetState(name)
				require.False(t, ok)
			})
		}
	}
}
