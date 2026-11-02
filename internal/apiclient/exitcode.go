package apiclient

import (
	"context"
	"errors"
)

const (
	ExitCodeSuccess     = 0
	ExitCodeUsage       = 2
	ExitCodeAuth        = 3
	ExitCodeNotFound    = 4
	ExitCodeTimeout     = 5
	ExitCodeRunPaused   = 6
	ExitCodeInterrupted = 130
	ExitCodeFailure     = 1
)

var (
	ErrRunPaused   = errors.New("run paused")
	ErrInterrupted = errors.New("interrupted")
)

// UsageError marks invalid command usage or configuration.
type UsageError struct{ Err error }

func (e *UsageError) Error() string {
	if e == nil || e.Err == nil {
		return "usage error"
	}
	return e.Err.Error()
}

func (e *UsageError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// ExitCodeFor maps an error to the stable command-line exit status.
func ExitCodeFor(err error) int {
	switch {
	case err == nil:
		return ExitCodeSuccess
	case errors.Is(err, ErrInterrupted) && errors.Is(err, context.Canceled):
		return ExitCodeInterrupted
	case hasUsageError(err):
		return ExitCodeUsage
	case IsAuth(err):
		return ExitCodeAuth
	case IsNotFound(err):
		return ExitCodeNotFound
	case IsTimeout(err):
		return ExitCodeTimeout
	case errors.Is(err, ErrRunPaused):
		return ExitCodeRunPaused
	default:
		return ExitCodeFailure
	}
}

func hasUsageError(err error) bool { var usage *UsageError; return errors.As(err, &usage) }
