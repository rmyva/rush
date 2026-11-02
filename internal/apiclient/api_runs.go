package apiclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// CreateRun submits a run; an empty idempotency key is generated securely.
func (c *Client) CreateRun(ctx context.Context, sessionID string, request RunRequest, idempotencyKey string) (RunAccepted, error) {
	if idempotencyKey == "" {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return RunAccepted{}, errors.New("generate idempotency key")
		}
		idempotencyKey = "rushapi-" + hex.EncodeToString(raw[:])
	}
	var accepted RunAccepted
	if err := c.mutateJSON(ctx, http.MethodPost, "/sessions/"+url.PathEscape(sessionID)+"/runs", request, idempotencyKey, &accepted); err != nil {
		return RunAccepted{}, err
	}
	return accepted, nil
}

// GetRun returns a run and optionally waits for a state change.
func (c *Client) GetRun(ctx context.Context, id string, waitSeconds int) (*Run, error) {
	query := url.Values{}
	if waitSeconds != 0 {
		query.Set("wait", strconv.Itoa(waitSeconds))
	}
	var run Run
	if err := c.getJSON(ctx, "/runs/"+url.PathEscape(id), query, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// WaitRun polls using the server's long-wait endpoint until completion or timeout.
func (c *Client) WaitRun(ctx context.Context, id string, overall time.Duration) (*Run, error) {
	if overall <= 0 {
		return nil, &RunTimeoutError{Err: context.DeadlineExceeded}
	}
	waitCtx, cancel := context.WithTimeout(ctx, overall)
	defer cancel()
	for {
		run, err := c.GetRun(waitCtx, id, 25)
		if err != nil {
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return nil, &RunTimeoutError{Err: context.DeadlineExceeded}
			}
			return nil, err
		}
		switch run.State {
		case "succeeded":
			return run, nil
		case "failed":
			return run, runFailure(run)
		case "cancelled":
			failure := runFailure(run)
			failure.Code = "cancelled"
			return run, failure
		case "awaiting_answer":
			return run, &RunPausedError{Run: run}
		}
		if err := waitCtx.Err(); err != nil {
			return nil, &RunTimeoutError{Err: err}
		}
	}
}

// CancelRun requests cancellation of a run.
func (c *Client) CancelRun(ctx context.Context, id string) (*Run, error) {
	var run Run
	if err := c.mutateJSON(ctx, http.MethodPost, "/runs/"+url.PathEscape(id)+"/cancel", struct{}{}, "", &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// Inject adds a message to an active run.
func (c *Client) Inject(ctx context.Context, sessionID, message string, interrupt bool) error {
	mode := "merge"
	if interrupt {
		mode = "interrupt"
	}
	return c.mutateJSON(ctx, http.MethodPost, "/sessions/"+url.PathEscape(sessionID)+"/inject", struct {
		Message string `json:"message"`
		Mode    string `json:"mode"`
	}{message, mode}, "", nil)
}

// RunTimeoutError indicates that the overall run wait duration elapsed.
type RunTimeoutError struct{ Err error }

func (e *RunTimeoutError) Error() string { return "run wait timed out" }
func (e *RunTimeoutError) Unwrap() error { return e.Err }

// RunPausedError carries the run that requires a permission answer.
type RunPausedError struct{ Run *Run }

func (e *RunPausedError) Error() string { return ErrRunPaused.Error() }
func (e *RunPausedError) Unwrap() error { return ErrRunPaused }

func runFailure(run *Run) *RunError {
	if run.Error == nil {
		return &RunError{Run: run}
	}
	failure := *run.Error
	failure.Run = run
	return &failure
}
