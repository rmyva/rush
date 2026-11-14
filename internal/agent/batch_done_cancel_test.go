package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// Revert-check: returning the driver error as is makes the canceled-context
// case report "interrupted (9)" instead of context.Canceled (the flaky
// TestBatchStoreDoneSourceForeignRootWaitsDurably symptom).
func TestCallerCancelOr(t *testing.T) {
	t.Parallel()
	interrupted := errors.New("interrupted (9)")

	live := t.Context()
	require.NoError(t, callerCancelOr(live, nil))
	require.ErrorIs(t, callerCancelOr(live, interrupted), interrupted, "a live context keeps the store error")

	canceled, cancel := context.WithCancel(live)
	cancel()
	require.NoError(t, callerCancelOr(canceled, nil), "no error stays no error even when canceled")
	require.ErrorIs(t, callerCancelOr(canceled, interrupted), context.Canceled, "a caller cancel wins over the interrupted read")
}
