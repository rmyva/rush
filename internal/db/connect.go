package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/PHPCraftdream/rush/internal/db/storepath"
)

// BusyTimeout is how long a statement waits on a locked database before
// SQLITE_BUSY. Callers bounding a write with their own deadline must allow
// at least this long, or a legitimate lock wait reads as a failure.
const BusyTimeout = 30 * time.Second

var pragmas = map[string]string{
	"foreign_keys":  "ON",
	"journal_mode":  "WAL",
	"page_size":     "4096",
	"temp_store":    "MEMORY",
	"cache_size":    "-8000",
	"synchronous":   "NORMAL",
	"secure_delete": "ON",
	"busy_timeout":  strconv.FormatInt(BusyTimeout.Milliseconds(), 10),
}

//go:embed migrations/*.sql
var FS embed.FS

// connEntry holds a shared database connection pair and its reference
// count. db is the single-connection writer (SetMaxOpenConns(1) — see the
// comment in Connect for why). readDB is a separate, read-only pool that
// WAL allows to run concurrently with the writer without touching the
// corruption-prone write path; it is only ever nil-safe-equal to db itself
// when opening a genuinely separate read-only handle isn't possible (see
// ConnectRead). Both handles share one refCount and are opened/closed
// together so Connect/ConnectRead/Release callers don't have to reason
// about two independent lifetimes.
type connEntry struct {
	db       *sql.DB
	readDB   *sql.DB
	refCount int
	pathLock *pathLock
	absPath  string
}

// pathLock serializes database lifecycle operations for one path. refs holds
// one reference for every Connect/Release operation that has acquired the
// path lock, plus one while the path has a pooled connection. Keeping the
// pooled reference prevents a new operation from observing an old pathLock
// while another operation is still finishing with it.
type pathLock struct {
	mu   sync.Mutex
	refs int
}

var (
	pool   = make(map[string]*connEntry)
	poolMu sync.Mutex

	// lifecycleMu gates the pool-wide ResetPool barrier. Normal operations
	// only hold it long enough to register themselves; ResetPool blocks new
	// operations and waits for all registered operations to finish before it
	// snapshots the pool. lifecycleResetters gives queued resets preference
	// during barrier handoff, so normal operations cannot enter between them.
	lifecycleMu        sync.Mutex
	lifecycleCond      = sync.NewCond(&lifecycleMu)
	lifecycleActive    int
	lifecycleResetting bool
	lifecycleResetters int

	// pathLocks contains only paths with an active opener, waiter, or pooled
	// connection. pathLocksMu protects both the map and each pathLock's refs;
	// the pathLock mutex itself serializes the slow open/close lifecycle for
	// one path. poolMu only guards pool bookkeeping, never slow database work.
	pathLocks   = make(map[string]*pathLock)
	pathLocksMu sync.Mutex

	// Lock-order audit: lifecycleMu is not held while another lifecycle
	// operation runs. For path operations, pathLocksMu is released before
	// pathLock.mu is acquired, and pathLock.mu precedes poolMu. ResetPool
	// releases poolMu before taking any pathLock.mu, so it cannot invert the
	// normal pathLock.mu-to-poolMu order. Registry reference updates never
	// hold poolMu, and final path-lock decrements happen after path unlock.

	// onReadDBOpen is a test seam for simulating a lazily opened reader
	// that cannot establish its first connection. Nil outside tests.
	onReadDBOpen func(dbPath string) (*sql.DB, error)

	// onOpenNew runs after a cache miss, before the slow open+migrate
	// work, for a database path not yet in the pool. Test seam only:
	// lets tests pause one path mid-open to prove unrelated paths
	// don't serialize behind it. Nil outside tests.
	onOpenNew func(absPath string)

	// onCloseEntry runs while the path lock is held, immediately before a
	// pooled entry is closed. Test seam only; nil outside tests.
	onCloseEntry func(absPath string)

	// onPathLockAcquired runs after a path-lock registry reference is acquired.
	// Test seam only; nil outside tests.
	onPathLockAcquired func(absPath string)

	// onResetBarrierAcquired runs after a ResetPool owns the lifecycle barrier
	// and all active operations have drained. Test seam only; nil outside tests.
	onResetBarrierAcquired func()

	// onLifecycleOperationWaiting runs before a normal lifecycle operation
	// waits behind resetters. Test seam only; nil outside tests.
	onLifecycleOperationWaiting func()
)

func acquirePathLock(absPath string) *pathLock {
	pathLocksMu.Lock()
	lock := pathLocks[absPath]
	if lock == nil {
		lock = &pathLock{}
		pathLocks[absPath] = lock
	}
	lock.refs++
	pathLocksMu.Unlock()
	if onPathLockAcquired != nil {
		onPathLockAcquired(absPath)
	}
	return lock
}

func retainPathLock(pathLock *pathLock) {
	pathLocksMu.Lock()
	pathLock.refs++
	pathLocksMu.Unlock()
}

func releasePathLock(absPath string, pathLock *pathLock) {
	pathLocksMu.Lock()
	pathLock.refs--
	if pathLock.refs == 0 && pathLocks[absPath] == pathLock {
		delete(pathLocks, absPath)
	}
	pathLocksMu.Unlock()
}

func beginLifecycleOperation() {
	lifecycleMu.Lock()
	for lifecycleResetting || lifecycleResetters != 0 {
		if onLifecycleOperationWaiting != nil {
			onLifecycleOperationWaiting()
		}
		lifecycleCond.Wait()
	}
	lifecycleActive++
	lifecycleMu.Unlock()
}

func endLifecycleOperation() {
	lifecycleMu.Lock()
	lifecycleActive--
	if lifecycleActive == 0 {
		lifecycleCond.Broadcast()
	}
	lifecycleMu.Unlock()
}

func beginPoolReset() {
	lifecycleMu.Lock()
	lifecycleResetters++
	for lifecycleResetting {
		lifecycleCond.Wait()
	}
	lifecycleResetters--
	lifecycleResetting = true
	for lifecycleActive != 0 {
		lifecycleCond.Wait()
	}
	lifecycleMu.Unlock()
}

func endPoolReset() {
	lifecycleMu.Lock()
	lifecycleResetting = false
	lifecycleCond.Broadcast()
	lifecycleMu.Unlock()
}

// Connect opens a SQLite database connection for the given data
// directory and runs migrations. If a connection to the same database
// file already exists, the existing connection is returned with its
// reference count incremented. Callers should pair each Connect with
// [ReleaseConn] using the returned *sql.DB when they no longer need it.
//
// Connect only ever returns the single-connection WRITER handle. Callers
// that also want a concurrent read-only handle for hot, standalone read
// paths (list/grep/call-tree queries that don't need read-your-own-write
// consistency with a subsequent write in the same call) should additionally
// call [ConnectRead] with the same dataDir — it shares this entry's
// refCount, so one [ReleaseConn] is needed for each returned handle.
//
// Migration policy can be narrowed per call with [WithMayMigrate] (a dev
// build pointed at a shared data directory passes false); the default is to
// migrate.
func Connect(ctx context.Context, dataDir string, migrationOpts ...MigrateOption) (*sql.DB, error) {
	entry, err := connect(ctx, dataDir, migrationOpts...)
	if err != nil {
		return nil, err
	}
	return entry.db, nil
}

// ConnectRead opens (or reuses) a read-only connection pool for the same
// database Connect would open, sharing its reference count. WAL mode lets
// readers run fully concurrently with the single writer connection instead
// of queuing behind it — this is the point of keeping the two separate.
//
// dataDir must have already been (or be about to be) passed to Connect;
// calling ConnectRead alone still opens the writer underneath (migrations
// must run before anything reads), it just returns the reader handle. Every
// call increments the shared refCount, so a caller that calls both Connect
// and ConnectRead for the same dataDir must call [ReleaseConn] with the
// returned handles the same number of times it called either one (once per
// Connect/ConnectRead pair, not once per function).
func ConnectRead(ctx context.Context, dataDir string, migrationOpts ...MigrateOption) (*sql.DB, error) {
	entry, err := connect(ctx, dataDir, migrationOpts...)
	if err != nil {
		return nil, err
	}
	return entry.readDB, nil
}

// connect is the shared implementation behind Connect and ConnectRead: it
// opens (or reuses) the pool entry for dataDir, running migrations exactly
// once, and increments the entry's refCount once per call — so callers that
// want BOTH a writer and a reader handle for the same dataDir must call
// [ReleaseConn] twice (once per Connect/ConnectRead call they made), matching
// the existing "one Connect, one Release" contract extended to ConnectRead.
func connect(ctx context.Context, dataDir string, migrationOpts ...MigrateOption) (*connEntry, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("data.dir is not set")
	}
	beginLifecycleOperation()
	defer endLifecycleOperation()

	dbPath := storepath.DataDirPath(dataDir, storepath.ActiveFilename)

	// Resolve to an absolute path so that different relative paths to
	// the same file share a single connection.
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		absPath = dbPath
	}

	// Serialize lifecycle operations of THIS path only. poolMu is deliberately
	// NOT held across the open/ping/migrate work below: it used to be, and
	// one global lock made every unrelated dataDir in the process
	// queue up behind a single fresh database's full migration run —
	// under -race this serialized dozens of parallel tests' fresh
	// t.TempDir() databases badly enough to eat a whole package's
	// 10-minute timeout (task #637). Same-path callers still serialize
	// here, so the "one connection per database file" guarantee below
	// is unchanged.
	pathMu := acquirePathLock(absPath)
	pathMu.mu.Lock()
	defer func() {
		pathMu.mu.Unlock()
		releasePathLock(absPath, pathMu)
	}()

	poolMu.Lock()
	if entry, ok := pool[absPath]; ok {
		entry.refCount++
		poolMu.Unlock()
		return entry, nil
	}
	poolMu.Unlock()

	if onOpenNew != nil {
		onOpenNew(absPath)
	}

	// Resolve the migration decision inputs BEFORE the destination file is
	// created or opened: the legacy-store import must run first, so a fresh
	// Connect over a data dir that only holds the legacy rush.db imports it
	// into the store-v1 destination instead of migrating an empty file past
	// it. ImportLegacyStore acquires the store-v1 import lock itself (never
	// re-acquire it here) and is a self-contained no-op when the destination
	// already exists or no legacy source exists; with MayMigrate false the
	// permission gate also suppresses the import.
	migrateFS, err := migrationFS()
	if err != nil {
		return nil, fmt.Errorf("failed to locate embedded migrations: %w", err)
	}
	migrateOpts := newMigrateOptions(migrationOpts, dbPath)
	migrateOpts.MigrationLockPath = filepath.Join(dataDir, "migrate.lock")
	if !migrateOpts.MayMigrate {
		if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
			pending, err := embeddedVersions(migrateFS)
			if err != nil {
				return nil, fmt.Errorf("failed to apply migrations: %w", err)
			}
			return nil, fmt.Errorf("failed to apply migrations: %w", &SchemaMigrationNotAllowedError{Pending: pending})
		}
	}
	if migrateOpts.MayMigrate {
		if _, err := ImportLegacyStore(ctx, dataDir, LegacyImportOptions{Schema: migrateFS}); err != nil {
			return nil, fmt.Errorf("legacy import: %w", err)
		}
	}

	conn, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}

	// Serialize all access through a single connection. SQLite
	// serializes writes at the file level anyway, and allowing multiple
	// pool connections to interleave writes/checkpoints (especially
	// under concurrent sub-agents) has caused WAL/header desync
	// resulting in SQLITE_NOTADB (26) on the next open.
	conn.SetMaxOpenConns(1)

	if err = conn.PingContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	// The migration decision is a cross-process critical section: every
	// process that starts against this data directory reads the same
	// version table, and without mutual exclusion two of them racing a
	// deploy both see the same pending ALTER and the second applies it
	// twice ("duplicate column name"). The lock is taken even when no
	// migration is needed, because the version read that produces that
	// decision is part of the sequence being protected.
	if err := migrateLocked(ctx, conn, dbPath, migrateFS, migrateOpts); err != nil {
		conn.Close()
		slog.Error("Failed to apply migrations", "error", err)
		return nil, fmt.Errorf("failed to apply migrations: %w", err)
	}

	// Open the read-only pool AFTER migrations have run on the writer, on
	// the same absolute path. WAL mode (already set on the writer above,
	// and inherited file-wide) means these reader connections observe a
	// consistent snapshot without ever blocking on — or being blocked
	// by — the writer's single connection. mode=ro additionally guards
	// against a read path accidentally issuing a write: it will fail
	// fast with SQLITE_READONLY instead of silently taking the writer's
	// place.
	//
	// If the read-only pool fails to open for any reason, we don't fail
	// Connect/ConnectRead over it — we fall back to routing reads through
	// the writer connection (readDB = conn), which is exactly today's
	// behavior. A degraded-to-serialized read path is strictly better
	// than refusing to start.
	openReader := openReadDB
	if onReadDBOpen != nil {
		openReader = onReadDBOpen
	}
	readConn, err := openReader(dbPath)
	if err == nil {
		pingCtx, cancel := context.WithTimeout(ctx, time.Second)
		err = readConn.PingContext(pingCtx)
		cancel()
		if err != nil {
			_ = readConn.Close()
		}
	}
	if err != nil {
		slog.Warn("Failed to connect read-only pool, reads will serialize with writes", "error", err)
		readConn = conn
	}

	// Keep the path lock registered for as long as this pool entry exists.
	// This reference is acquired before publishing the entry so a concurrent
	// Release cannot remove the registry entry between those two operations.
	retainPathLock(pathMu)
	entry := &connEntry{db: conn, readDB: readConn, refCount: 1, pathLock: pathMu, absPath: absPath}
	poolMu.Lock()
	pool[absPath] = entry
	poolMu.Unlock()
	return entry, nil
}

// Release decrements the reference count for the database at the given
// data directory. When the count reaches zero the underlying connections
// (writer and, if separate, reader) are closed and removed from the pool.
//
// A caller that obtained both a writer (Connect) and a reader (ConnectRead)
// handle for the same dataDir must call ReleaseConn once per call it made to
// either function — the shared refCount is decremented once per ReleaseConn,
// symmetric with connect()'s "increment once per call" contract.
func Release(dataDir string) error {
	beginLifecycleOperation()
	defer endLifecycleOperation()

	return releasePath(dataDir)
}

// ReleaseConn decrements the reference count for the pooled generation that
// owns conn. The returned *sql.DB is the release token: if that generation
// was forcibly removed and a new generation was opened for the same path,
// ReleaseConn is a no-op and cannot affect the new generation.
//
// Callers should prefer ReleaseConn over Release. Release is retained for
// compatibility with older callers, but a path-only release cannot identify
// a generation after ReleaseAll or ResetPool and must not be used for a
// cleanup that can outlive either operation.
func ReleaseConn(conn *sql.DB) error {
	if conn == nil {
		return nil
	}
	beginLifecycleOperation()
	defer endLifecycleOperation()

	poolMu.Lock()
	var entry *connEntry
	for _, candidate := range pool {
		if candidate.db == conn || candidate.readDB == conn {
			entry = candidate
			break
		}
	}
	poolMu.Unlock()
	if entry == nil {
		return nil
	}

	return releaseEntry(entry.absPath, entry)
}

func releasePath(dataDir string) error {
	dbPath := storepath.DataDirPath(dataDir, storepath.ActiveFilename)
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		absPath = dbPath
	}

	return releaseEntry(absPath, nil)
}

func releaseEntry(absPath string, expected *connEntry) error {
	pathMu := acquirePathLock(absPath)
	pathMu.mu.Lock()
	defer func() {
		pathMu.mu.Unlock()
		releasePathLock(absPath, pathMu)
	}()

	poolMu.Lock()
	entry, ok := pool[absPath]
	if !ok || (expected != nil && entry != expected) {
		poolMu.Unlock()
		return nil
	}

	return releaseEntryLocked(absPath, entry)
}

func releaseEntryLocked(absPath string, entry *connEntry) error {
	entry.refCount--
	if entry.refCount > 0 {
		poolMu.Unlock()
		return nil
	}

	// refCount reached zero: remove from pool while holding the mutex to
	// prevent concurrent Connect() from finding and incrementing it, then
	// close the actual connections while pathMu is held. A waiter may already
	// have acquired pathMu's registry reference; keeping the mutex until close
	// completes prevents it from opening a new generation while the old one is
	// still being torn down.
	delete(pool, absPath)
	poolMu.Unlock()

	if onCloseEntry != nil {
		onCloseEntry(absPath)
	}
	closeErr := closeEntry(entry)
	releasePathLock(absPath, entry.pathLock)
	return closeErr
}

// ReleaseAll forcibly closes the pooled connection for dataDir and removes
// it from the pool, regardless of its current reference count. A no-op
// (returns nil) if no entry exists for dataDir, matching Release's own
// behavior in that case.
//
// Intended for tests that need to guarantee a specific dataDir's connection
// is fully closed at teardown, even when the number of paired Connect/
// ConnectRead calls a caller made isn't fully known or a cooperating
// caller's own cleanup may have left references outstanding on purpose.
// App.Shutdown's forced-shutdown path is exactly such a case: it
// deliberately skips calling Release at all (to avoid closing a DB out from
// under a still-active live writer in real CLI/server use, where the
// process exits immediately after and the OS reclaims the handle) — a test
// binary does NOT exit after one test, so relying on a single paired
// Release call in that scenario silently leaves the entry's refCount above
// zero forever, leaking the underlying file handle for the rest of the test
// binary's life. On Windows specifically, that leaked handle then makes
// t.TempDir()'s own RemoveAll cleanup fail (unlike POSIX, Windows will not
// delete a file that still has an open handle).
//
// Scoped to dataDir's own absolute path only — safe to call from a single
// test's cleanup even when other tests use the pool concurrently (e.g. via
// t.Parallel()) for DIFFERENT dataDirs, unlike ResetPool which tears down
// every pooled entry process-wide.
func ReleaseAll(dataDir string) error {
	beginLifecycleOperation()
	defer endLifecycleOperation()

	dbPath := storepath.DataDirPath(dataDir, storepath.ActiveFilename)
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		absPath = dbPath
	}

	pathMu := acquirePathLock(absPath)
	pathMu.mu.Lock()

	poolMu.Lock()
	entry, ok := pool[absPath]
	if !ok {
		poolMu.Unlock()
		pathMu.mu.Unlock()
		releasePathLock(absPath, pathMu)
		return nil
	}
	delete(pool, absPath)
	// Zero the refCount before unlocking (found by the sixth @oh review
	// pass): this entry is already unlinked from the pool, so nothing can
	// legitimately observe it again through Connect/Release — but zeroing
	// it defensively means a caller that (incorrectly) still held a
	// reference to this now-detached entry and later calls Release on it
	// cannot under-decrement a DIFFERENT, freshly-created entry for the
	// same dataDir path.
	entry.refCount = 0
	poolMu.Unlock()

	if onCloseEntry != nil {
		onCloseEntry(absPath)
	}
	closeErr := closeEntry(entry)
	pathMu.mu.Unlock()
	releasePathLock(absPath, pathMu)
	releasePathLock(absPath, entry.pathLock)
	return closeErr
}

// closeEntry closes both handles in entry, tolerating readDB == db (the
// fallback path in connect() when the read-only pool failed to open, or in
// principle any future caller that intentionally shares one *sql.DB between
// both roles) by only closing the reader when it is a distinct handle.
func closeEntry(entry *connEntry) error {
	// Drop the batch prepared-statement cache (pda_batch_prepare.go) before
	// the pool handles die; its close errors are merged without masking the
	// DB close errors.
	dropErr := pdaDropBatchStmts(entry.db)
	var readErr error
	if entry.readDB != nil && entry.readDB != entry.db {
		readErr = entry.readDB.Close()
	}
	writeErr := entry.db.Close()
	if writeErr != nil {
		return writeErr
	}
	if dropErr != nil {
		return dropErr
	}
	return readErr
}

// ResetPool establishes a pool-wide lifecycle barrier, closes all pooled
// connections, and clears the pool. It waits for in-flight Connect/Release
// operations, so it returns only after no opener can publish a new entry.
// This is intended for use in tests to ensure a clean state between test
// cases.
func ResetPool() {
	type pooledEntry struct {
		path  string
		entry *connEntry
	}

	// Establish a pool-wide barrier before observing pool. Every Connect,
	// Release, and ReleaseAll operation registers with this gate, so no
	// cache-miss opener can publish an entry after this snapshot or while the
	// entries are being closed.
	beginPoolReset()
	defer endPoolReset()

	if onResetBarrierAcquired != nil {
		onResetBarrierAcquired()
	}

	entries := make([]pooledEntry, 0)
	poolMu.Lock()
	for path, entry := range pool {
		delete(pool, path)
		entries = append(entries, pooledEntry{path: path, entry: entry})
	}
	poolMu.Unlock()

	for _, pooled := range entries {
		pooled.entry.pathLock.mu.Lock()
		if onCloseEntry != nil {
			onCloseEntry(pooled.path)
		}
		closeEntry(pooled.entry) //nolint:errcheck
		pooled.entry.pathLock.mu.Unlock()
		releasePathLock(pooled.path, pooled.entry.pathLock)
	}
}

// Migrate applies the embedded schema migrations to conn.
//
// It is the un-pooled, un-locked entry point for callers that hand us a
// connection they already own and serialize (the SDK's in-memory database).
// Paths that own a data directory go through Connect, which wraps the whole
// read-versions/decide/apply sequence in a cross-process lock.
//
// Goose's legacy package-level API stores the selected dialect in a mutable
// global. That API is not safe when independent clients initialize databases
// concurrently. A Provider owns its dialect, filesystem, and migration
// operations, so each database gets an isolated migration boundary.
func Migrate(ctx context.Context, conn *sql.DB) error {
	fsys, err := migrationFS()
	if err != nil {
		return err
	}
	return migrate(ctx, conn, "", fsys, MigrateOptions{MayMigrate: true})
}
