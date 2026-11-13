package server

import (
	"context"
	"sync"

	appPkg "github.com/PHPCraftdream/rush/internal/app"
	"github.com/PHPCraftdream/rush/internal/pubsub"
)

type apiEventFeeder struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func startAPIEventFeeder(app *appPkg.App, log *APIEventLog, registry *APIRunRegistry) *apiEventFeeder {
	ctx, cancel := context.WithCancel(context.Background())
	feeder := &apiEventFeeder{cancel: cancel, done: make(chan struct{})}
	if registry != nil {
		registry.setObserver(func(run APIRun) {
			data := struct {
				RunID     string      `json:"run_id"`
				State     APIRunState `json:"state"`
				ErrorCode string      `json:"error_code,omitempty"`
			}{RunID: run.ID, State: run.State}
			if run.Error != nil {
				data.ErrorCode = run.Error.Code
			}
			log.Publish(run.SessionID, "run", data)
		})
	}
	go func() {
		defer close(feeder.done)
		if app == nil {
			<-ctx.Done()
			return
		}
		// Fork patch (finalization): nil gate (flag off) leaves both feeds
		// byte-for-byte unchanged; a held terminal event waits for the seal.
		gate := finalizationTerminalGateFor(app)
		var wg sync.WaitGroup
		if app.Messages != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for event := range FinalizationGatedMessageStream(ctx, gate, app.Messages.Subscribe(ctx)) {
					if event.Type == pubsub.CreatedEvent || event.Type == pubsub.UpdatedEvent {
						log.Publish(event.Payload.SessionID, "message", apiMessageFromModel(event.Payload))
					}
				}
			}()
		}
		if app.Sessions != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for event := range FinalizationGatedSessionStream(ctx, gate, app.Sessions.Subscribe(ctx)) {
					switch event.Type {
					case pubsub.CreatedEvent, pubsub.UpdatedEvent:
						log.Publish(event.Payload.ID, "session", apiSessionFromModel(event.Payload))
					case pubsub.DeletedEvent:
						log.DeleteSession(event.Payload.ID)
					}
				}
			}()
		}
		<-ctx.Done()
		wg.Wait()
	}()
	return feeder
}

func (f *apiEventFeeder) Stop(ctx context.Context) error {
	if f == nil {
		return nil
	}
	f.cancel()
	select {
	case <-f.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
