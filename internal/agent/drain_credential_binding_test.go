package agent

import (
	"context"
	"errors"
	"testing"

	"charm.land/fantasy"
	"github.com/stretchr/testify/require"
)

func TestCredentialedRootMarkingOnlyAfterAdmission(t *testing.T) {
	for _, existingMembership := range []bool{false, true} {
		t.Run(map[bool]string{false: "new-root", true: "existing-root"}[existingMembership], func(t *testing.T) {
			f := newAttemptFixture(t, "credentialed-drain-admission", attemptFixtureOpts{noIdle: true, noDriver: true})
			f.withOAuthProvider("probe")
			f.coord.currentAgent = f.sa
			if existingMembership {
				f.coord.markCredentialedRoot(f.sessID)
			}
			_, epoch, cancel, ok := f.sa.ReserveExclusive(context.Background(), f.sessID)
			require.True(t, ok)
			defer f.sa.ReleaseExclusive(f.sessID, epoch, cancel)

			setupCalls := 0
			ctx := WithCallOptions(context.Background(), &CallOptions{FailIfSessionBusy: true})
			ctx = WithAdmissionSetup(ctx, func(context.Context) error {
				setupCalls++
				return nil
			})
			_, err := f.coord.RunWithCredentials(ctx, f.sessID, "busy", &CredentialSet{
				Credentials: []Credential{{Provider: "tenant", Type: ProviderTypeOpenAI, APIKey: "test"}},
				Models: map[Role]ModelChoice{
					RoleSmart: {Provider: "tenant", Model: "model"},
					RoleFast:  {Provider: "tenant", Model: "model"},
				},
			})
			require.ErrorIs(t, err, ErrSessionBusy)
			require.Zero(t, setupCalls)
			require.Equal(t, existingMembership, f.coord.isCredentialedRoot(f.sessID))
		})
	}
}

func TestCredentialedRootAdmissionFailureDoesNotMark(t *testing.T) {
	f := newAttemptFixture(t, "credentialed-admission-failure", attemptFixtureOpts{noIdle: true, noDriver: true})
	f.withOAuthProvider("probe")
	f.coord.currentAgent = f.sa
	setupErr := errors.New("admission setup failed")
	ctx := WithCallOptions(context.Background(), &CallOptions{})
	ctx = WithAdmissionSetup(ctx, func(context.Context) error { return setupErr })
	_, err := f.coord.RunWithCredentials(ctx, f.sessID, "prompt", &CredentialSet{
		Credentials: []Credential{{Provider: "tenant", Type: ProviderTypeOpenAI, APIKey: "test"}},
		Models: map[Role]ModelChoice{
			RoleSmart: {Provider: "tenant", Model: "model"},
			RoleFast:  {Provider: "tenant", Model: "model"},
		},
	})
	require.ErrorIs(t, err, setupErr)
	require.False(t, f.coord.isCredentialedRoot(f.sessID))
}

func TestCredentialedAdmissionPreservesCallerSetup(t *testing.T) {
	for _, setupErr := range []error{nil, errors.New("caller admission setup failed")} {
		name := "success"
		if setupErr != nil {
			name = "error"
		}
		t.Run(name, func(t *testing.T) {
			f := newAttemptFixture(t, "credentialed-caller-admission-setup", attemptFixtureOpts{noIdle: true, noDriver: true})
			f.withOAuthProvider("probe")
			setupCalls := 0
			f.coord.currentAgent = &mockSessionAgent{runFunc: func(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
				if setup := admissionSetupFrom(ctx); setup != nil {
					if err := setup(ctx); err != nil {
						return nil, err
					}
				}
				return &fantasy.AgentResult{}, nil
			}}
			ctx := WithAdmissionSetup(context.Background(), func(context.Context) error {
				setupCalls++
				return setupErr
			})
			_, err := f.coord.RunWithCredentials(ctx, f.sessID, "prompt", &CredentialSet{
				Credentials: []Credential{{Provider: "tenant", Type: ProviderTypeOpenAI, APIKey: "test"}},
				Models: map[Role]ModelChoice{
					RoleSmart: {Provider: "tenant", Model: "model"},
					RoleFast:  {Provider: "tenant", Model: "model"},
				},
			})
			require.Equal(t, 1, setupCalls)
			if setupErr == nil {
				require.NoError(t, err)
				require.True(t, f.coord.isCredentialedRoot(f.sessID))
			} else {
				require.ErrorIs(t, err, setupErr)
				require.False(t, f.coord.isCredentialedRoot(f.sessID))
			}
		})
	}
}

func TestAllowConfiguredRoleFallbackCredentialedRootMarkedAndDrainRefused(t *testing.T) {
	f := newAttemptFixture(t, "credentialed-fallback-drain", attemptFixtureOpts{noIdle: true, noDriver: true})
	f.withOAuthProvider("probe")
	f.coord.currentAgent = &mockSessionAgent{runFunc: func(ctx context.Context, _ SessionAgentCall) (*fantasy.AgentResult, error) {
		if setup := admissionSetupFrom(ctx); setup != nil {
			if err := setup(ctx); err != nil {
				return nil, err
			}
		}
		return &fantasy.AgentResult{}, nil
	}}
	_, err := f.coord.RunWithCredentials(context.Background(), f.sessID, "prompt", &CredentialSet{
		Credentials: []Credential{{Provider: "tenant", Type: ProviderTypeOpenAI, APIKey: "tenant-key"}},
		Models: map[Role]ModelChoice{
			RoleSmart: {Provider: "tenant", Model: "tenant-model"},
		},
		AllowConfiguredRoleFallback: true,
	})
	require.NoError(t, err)
	require.True(t, f.coord.isCredentialedRoot(f.sessID), "admitted credentialed root must be marked")

	_, _, err = f.coord.drainCallFor(context.Background(), f.sessID)
	require.ErrorIs(t, err, ErrCredentialBindingUnavailable)
}

func TestDrainCredentialedRootRefusalIsRootOnly(t *testing.T) {
	ctx := context.Background()
	f := newAttemptFixture(t, "credentialed-drain-root", attemptFixtureOpts{noIdle: true, noDriver: true})
	f.coord.markCredentialedRoot(f.sessID)
	// Ordinary session-model resolution would fail with a nil config; the
	// credentialed-root refusal must happen before that resolution path.
	f.coord.cfg = nil

	_, _, err := f.coord.drainCallFor(ctx, f.sessID)
	require.ErrorIs(t, err, ErrCredentialBindingUnavailable)
	require.Zero(t, f.requests.Load(), "a refused root drain must not reach the configured model/provider")
	require.False(t, errors.Is(err, ErrDiskProviderUnavailable))

	child := "credentialed-drain-child"
	childCreds := &CredentialSet{
		Credentials: []Credential{{Provider: "tenant", Type: ProviderTypeOpenAI, APIKey: "tenant-key"}},
		Models:      map[Role]ModelChoice{RoleSmart: {Provider: "tenant", Model: "tenant-model"}},
	}
	f.coord.subAgentDrivers.register(child, subAgentDriver{
		agent: f.sa,
		call:  SessionAgentCall{SessionID: child, SmartModel: &f.model, Credentials: childCreds},
	})
	call, prep, err := f.coord.drainCallFor(ctx, child)
	require.NoError(t, err)
	require.True(t, call.IsDrain)
	require.Same(t, childCreds, call.Credentials, "the credentialed driver template must retain its credential snapshot")
	if prep != nil {
		prep.release()
	}
}

func TestDrainCredentialedRootTerminalWakeAndRecheck(t *testing.T) {
	f := newAttemptFixture(t, "credentialed-drain-terminal", attemptFixtureOpts{noIdle: true, noDriver: true})
	f.seedDebt(context.Background(), "call-1", false)
	f.coord.markCredentialedRoot(f.sessID)
	f.coord.addToRecheckSet(f.sessID)

	err := f.coord.handleDrainBuildError(f.sessID, ErrCredentialBindingUnavailable)
	require.ErrorIs(t, err, ErrCredentialBindingUnavailable)
	require.Empty(t, f.coord.drainRecheckSet(), "credential refusal must not schedule periodic retries")
	row := f.row(context.Background(), "call-1")
	require.Zero(t, row.Reacted)
	require.Zero(t, row.ReactedFailed)
}

func TestDrainUnmarkedRootStillUsesFallback(t *testing.T) {
	f := newAttemptFixture(t, "unmarked-drain-root", attemptFixtureOpts{noIdle: true, noDriver: true})
	f.withOAuthProvider("probe")
	call, _, err := f.coord.drainCallFor(context.Background(), f.sessID)
	require.NoError(t, err)
	require.True(t, call.IsDrain)
	require.Equal(t, f.model.ModelCfg.Model, call.SmartModel.ModelCfg.Model)
}
