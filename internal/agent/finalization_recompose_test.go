package agent

import (
	"testing"

	"github.com/PHPCraftdream/rush/internal/message"
	"github.com/stretchr/testify/require"
)

// newFinalizationRecomposeFixture prepares a quiet seam env with a real
// provisional candidate whose stamp is made stale, forcing one recomposition.
func newFinalizationRecomposeFixture(t *testing.T) *finalizationWaitFixture {
	t.Helper()
	f := newFinalizationWaitFixture(t)
	f.ts.finalizationStamp.revision++
	return f
}

func TestFinalizationRecomposeDefaultRebuildsAndSeals(t *testing.T) {
	f := newFinalizationRecomposeFixture(t)
	previous := f.ts.finalizationStamp
	hook := f.hook(func(d *FinalizationWaitDeps) { d.Compose = nil })
	require.NoError(t, hook(t.Context(), f.ts))
	recordAccepted(t, f)
	require.Greater(t, f.ts.finalizationStamp.serial, previous.serial, "recomposition takes a fresh stamp")
	require.NotEmpty(t, f.ts.finalizationCandidate.messageID)
	stored, err := f.env.messages.Get(t.Context(), f.ts.finalizationCandidate.messageID)
	require.NoError(t, err)
	require.True(t, stored.IsFinished())
	require.Equal(t, f.ts.finalizationCandidate.messageID, f.ts.currentAssistant.ID)
}

func TestFinalizationRecomposeWithoutProvisionalFailsClosed(t *testing.T) {
	for name, prepare := range map[string]func(*finalizationWaitFixture){
		"no-provisional": func(f *finalizationWaitFixture) { f.ts.currentAssistant = nil },
		"no-footer": func(f *finalizationWaitFixture) {
			plain, err := f.env.messages.Create(t.Context(), "session", message.CreateMessageParams{
				Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "plain"}},
			})
			require.NoError(t, err)
			f.ts.currentAssistant = &plain
		},
		"foreign-key": func(f *finalizationWaitFixture) {
			m := *f.ts.currentAssistant
			require.NoError(t, m.MarkFinalizationCandidate(message.FinalizationIdentity{
				Key: "other", Generation: 1, Serial: 9,
			}))
			f.ts.currentAssistant = &m
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFinalizationRecomposeFixture(t)
			prepare(f)
			hook := f.hook(func(d *FinalizationWaitDeps) { d.Compose = nil })
			err := hook(t.Context(), f.ts)
			require.ErrorIs(t, err, ErrFinalizationComposeUnavailable)
			record, err := f.env.scope.store.Snapshot(t.Context(), f.env.scope.identity)
			require.NoError(t, err)
			require.Equal(t, "executing", record.Phase)
			if f.ts.currentAssistant != nil {
				require.False(t, f.ts.currentAssistant.IsFinished(), "a refused compose must not publish")
			}
		})
	}
}
