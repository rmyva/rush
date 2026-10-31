package filelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// FileLock is a generic exclusive lock on an arbitrary path on disk.
// Use AcquireFileLock for blocking acquisition (waits for the lock) or
// TryAcquireFileLock for non-blocking (returns immediately with an error
// if contended).
//
// Backed by the same OS primitives as SessionLock (flock on POSIX,
// LockFileEx on Windows). The lock is released automatically when the
// process dies; explicit Release closes the file handle.
//
// Use case (callers added 2026-05): the cliprovider qwen/gemini MCP-id
// files and their respective ~/.{qwen,gemini}/settings.json edits are
// read-modify-write across multiple rush processes that share a working
// directory or a home directory. Without a lock, two processes racing to
// generate the same MCP ID end up with split-brain (loser's in-memory
// ID points to no server) and concurrent settings.json edits clobber
// each other.
type FileLock struct {
	Path string
	f    *os.File
}

// Release unlocks and closes the lock file. Safe to call on nil.
//
// Deliberately does NOT os.Remove the sidecar file at l.Path — the lock file
// is left on disk permanently by design. Removing it here would race a
// concurrent process that acquires the lock (reopens/relocks the same path)
// between this unlock and the remove: on POSIX flock is attached to the inode,
// so unlinking the path leaves existing descriptors locked but lets a later
// opener create and lock a different inode at the same path, allowing two
// holders to believe they own the same lock. On Windows, the file is
// opened without FILE_SHARE_DELETE, so a concurrent holder's open handle
// would make the remove fail with a sharing violation, and even a
// successful delete risks a subsequent opener creating a distinct file
// object at the same path while an older one is still locked, which can
// reintroduce the exact split-brain this lock exists to prevent. Leaving a
// handful of empty *.lock sidecars on disk is a one-time, bounded cost;
// deleting them is not worth the cross-platform risk.
func (l *FileLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	unlockErr := unlockFile(l.f)
	closeErr := l.f.Close()
	l.f = nil
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// ErrLockContended is returned (wrapped) by TryAcquireFileLock when another
// process already holds the exclusive lock on Path. Callers should use
// errors.As to detect this case rather than matching on the error string,
// which is not a stable contract: an errors.As-based check keeps working
// across any wording change to this error or to TryAcquireFileLock's
// wrapping, whereas a substring check (the previous approach) would silently
// misclassify a genuine contention as fatal (or vice versa) the moment
// either message is edited, with nothing catching the mismatch at compile
// time or in tests that don't happen to exercise that exact string.
type ErrLockContended struct {
	Path string
}

func (e *ErrLockContended) Error() string {
	return fmt.Sprintf("file lock contended (%s)", e.Path)
}

// TryAcquireFileLock opens lockPath (creating it if needed) and takes an
// exclusive non-blocking lock. Returns immediately with an error if
// another process holds the lock. The lock file directory is created if
// it does not exist.
//
// Classification (host-lock module, docs/plans/2026-09-28-async-phase4-
// durable-core.md sec.3.6): tryLockFile's error is wrapped as
// *ErrLockContended ONLY when isLockContentionError says it genuinely means
// "another process holds this lock right now" (EWOULDBLOCK/EAGAIN on POSIX,
// ERROR_LOCK_VIOLATION/ERROR_SHARING_VIOLATION on Windows). Any other error
// (permission denied, a filesystem without lock support, ...) is returned
// as-is, unwrapped. Before this, every tryLockFile failure was blanket-
// wrapped as contended, which made isContentionError (and therefore
// AcquireFileLockContext's retry loop) misclassify a genuine, non-retryable
// failure as ordinary contention and poll it all the way to the caller's
// timeout instead of surfacing it immediately.
func TryAcquireFileLock(lockPath string) (*FileLock, error) {
	f, err := openLockFile(lockPath)
	if err != nil {
		return nil, err
	}
	return classifyAndLock(f, lockPath)
}

// classifyAndLock takes the exclusive non-blocking lock on an already-open
// f and applies TryAcquireFileLock's error classification. Split out from
// TryAcquireFileLock so a test can exercise the classification with a
// deliberately broken fd (EBADF from flock, not EWOULDBLOCK) without
// depending on OS-specific contention setup.
func classifyAndLock(f *os.File, lockPath string) (*FileLock, error) {
	if err := tryLockFile(f); err != nil {
		f.Close()
		if isLockContentionError(err) {
			return nil, fmt.Errorf("%w: %w", &ErrLockContended{Path: lockPath}, err)
		}
		return nil, fmt.Errorf("file lock %s: %w", lockPath, err)
	}
	return &FileLock{Path: lockPath, f: f}, nil
}

// AcquireFileLock opens lockPath (creating it if needed) and blocks
// until it can take an exclusive lock. Use for short critical sections
// that mutate a shared file (settings.json, generated-ID files, etc.).
// Combine with deferred Release.
//
// WARNING: blocks indefinitely. If the holding process is wedged
// (debugger attached, suspended, frozen on a network FS), this call
// never returns. Production parallel-process callers must use
// AcquireFileLockContext with a timeout.
func AcquireFileLock(lockPath string) (*FileLock, error) {
	f, err := openLockFile(lockPath)
	if err != nil {
		return nil, err
	}
	if err := lockFileBlocking(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("file lock blocking-acquire failed (%s): %w", lockPath, err)
	}
	return &FileLock{Path: lockPath, f: f}, nil
}

// AcquireFileLockContext is the bounded-wait variant of AcquireFileLock.
// It polls TryAcquireFileLock with exponential backoff until success or
// ctx is done; returns ctx.Err() on timeout/cancel. Use this in any
// parallel-process path where a hung holder must not freeze the whole
// fleet — see for example the MCP register/deregister flow.
//
// Polling (rather than running blocking flock in a goroutine + select
// on ctx) is intentional: a blocking flock that succeeds after ctx
// cancellation would leak the lock file handle held by an orphan
// goroutine, defeating the whole timeout. Backoff schedule is
// 25ms, 50ms, 100ms, 250ms, then capped at 500ms.
func AcquireFileLockContext(ctx context.Context, lockPath string) (*FileLock, error) {
	backoff := 25 * time.Millisecond
	const maxBackoff = 500 * time.Millisecond
	for {
		l, err := TryAcquireFileLock(lockPath)
		if err == nil {
			return l, nil
		}
		// Surface IO/permission errors immediately; only retry on genuine
		// lock contention. TryAcquireFileLock wraps contention in
		// *ErrLockContended; openLockFile failures (missing dir, permission
		// denied, etc.) don't, so isContentionError's errors.As check
		// distinguishes the two without depending on error text.
		if !isContentionError(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("file lock %s: %w", lockPath, ctx.Err())
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// isContentionError returns true when err comes from TryAcquireFileLock
// reporting that another holder has the lock (as opposed to IO or
// permission failures that should not be retried).
func isContentionError(err error) bool {
	var contended *ErrLockContended
	return errors.As(err, &contended)
}

func openLockFile(lockPath string) (*os.File, error) {
	if lockPath == "" {
		return nil, fmt.Errorf("file lock: empty path")
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("file lock: create parent dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("file lock: open %s: %w", lockPath, err)
	}
	return f, nil
}
