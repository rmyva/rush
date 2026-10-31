package agent

// Regression coverage for the operator's peak-hours message
// (config.PeakHoursWindow.Message) along the whole refusal chain the
// 2026-09-23 smoke test exercised: config window -> checkPeakHours /
// checkLivePeakHours -> *PeakHoursError -> guidance text -> mid-turn
// watcher's persisted Finish details.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/PHPCraftdream/rush/internal/config"
	"github.com/PHPCraftdream/rush/internal/csync"
	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

const regressionPeakMessage = "OPERATOR-NOTE: quota exhausted, resume after midnight"

func TestCheckPeakHours_CarriesOperatorMessage(t *testing.T) {
	w := peakWindowAroundNow()
	w.Message = regressionPeakMessage

	err := checkPeakHours(config.ProviderConfig{ID: "zai", PeakHours: w})
	var pe *PeakHoursError
	require.True(t, errors.As(err, &pe), "refusal must be a *PeakHoursError")
	require.Equal(t, regressionPeakMessage, pe.Message, "the window's message must reach the error")
	require.Contains(t, PeakHoursGuidance(err), regressionPeakMessage, "guidance must append the operator message")
}

// Mid-turn source: the live check re-reads config edited on disk while the
// agent runs (what `rush providers set --local ...` does to a running
// session) and must pick up the message too, not just the window.
func TestCheckLivePeakHours_CarriesMessageFromEditedConfig(t *testing.T) {
	// Legacy-JSON fixture test: it rewrites rush.json at its literal path
	// after config.Init and expects the live reload to see it, so the gate
	// must be off (after migration the active file is rush.ktav).
	restore := config.SetKtavAutoMigrateForTest(false)
	t.Cleanup(restore)

	workDir := t.TempDir()
	configPath := filepath.Join(workDir, "rush.json")
	base := `{"options":{"disable_default_providers":true},"providers":{"custom":{"api_key":"k","base_url":"https://api.example.test/v1","models":[{"id":"model"}]%s}}}`
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf(base, "")), 0o600))

	store, err := config.Init(workDir, workDir, false)
	require.NoError(t, err)
	coord := &coordinator{cfg: store}
	require.NoError(t, coord.checkLivePeakHours("custom"))

	w := peakWindowAroundNow()
	peak := fmt.Sprintf(`,"peak_hours":{"start":%q,"end":%q,"message":%q}`, w.Start, w.End, regressionPeakMessage)
	require.NoError(t, os.WriteFile(configPath, []byte(fmt.Sprintf(base, peak)), 0o600))

	err = coord.checkLivePeakHours("custom")
	var pe *PeakHoursError
	require.True(t, errors.As(err, &pe), "mid-session window must refuse with *PeakHoursError, got %v", err)
	require.Equal(t, regressionPeakMessage, pe.Message)
}

// The watcher that stops an in-flight turn must persist the message in the
// Finish details (what `sessions why/last` and the web UI show).
func TestPeakHoursWatcher_PersistsOperatorMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		msgSvc := &fakePeakHoursMsgSvc{}
		checkErr := &PeakHoursError{ProviderID: "zai", Start: "20:49", End: "23:59", ReopensAt: time.Now().Add(3 * time.Hour), Message: regressionPeakMessage}
		checkedProvider := make(chan string, 1)
		a := &sessionAgent{
			messages:       msgSvc,
			activeRequests: csync.NewMap[string, context.CancelFunc](),
			peakHoursCheck: func(providerID string) error {
				checkedProvider <- providerID
				return checkErr
			},
		}
		a.activeRequests.Set("sess-1", func() {})

		var sessionLock sync.Mutex
		assistant := &message.Message{ID: "msg-1", SessionID: "sess-1", Role: message.Assistant}
		genCtx, genCancel := context.WithCancel(t.Context())
		defer genCancel()

		w := newPeakHoursWatcher(a, "sess-1", "pinned-provider", t.Context(), genCtx, &sessionLock, &assistant)
		done := w.start()
		synctest.Wait()
		<-done

		require.Equal(t, "pinned-provider", <-checkedProvider, "watcher must check the turn's pinned provider")

		msgSvc.mu.Lock()
		persisted := msgSvc.lastUpdated
		msgSvc.mu.Unlock()
		finish := persisted.FinishPart()
		require.NotNil(t, finish, "watcher must persist a Finish part")
		require.Equal(t, message.FinishReasonError, finish.Reason)
		require.Contains(t, finish.Details, regressionPeakMessage, "persisted finish details must carry the operator message")
		require.Equal(t, 1, strings.Count(finish.Details, regressionPeakMessage), "message must appear once")

		var pe *PeakHoursError
		require.True(t, errors.As(w.getAbortErr(), &pe), "abort error must stay typed so the run result can render the message")
		require.Equal(t, regressionPeakMessage, pe.Message)
	})
}
