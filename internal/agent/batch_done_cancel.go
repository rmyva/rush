package agent

import "context"

// callerCancelOr maps a read error to ctx.Err() once the caller's context is
// done. A cancel that lands while the durable read is in flight surfaces from
// the SQLite driver as "interrupted (9)", not as a wrapped context error, so
// AwaitDone would otherwise report a caller-cancelled wait as a store failure.
// A nil error and a live context pass through unchanged.
func callerCancelOr(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}
