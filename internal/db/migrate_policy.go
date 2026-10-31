package db

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/PHPCraftdream/rush/internal/filelock"
	"github.com/pressly/goose/v3"
)

// SchemaDivergedError reports a database that this binary cannot open safely
// in either direction: it carries applied versions that no longer exist in
// the binary AND embedded versions that were never applied. That combination
// means the file was written by a different lineage of the binary, so neither
// migrating forward nor leaving it alone preserves the schema the code
// expects. Recovery is manual: restore the binary that applied the unknown
// versions, or repair the database by hand.
type SchemaDivergedError struct {
	Unknown []int64
	Pending []int64
}

func (e *SchemaDivergedError) Error() string {
	return fmt.Sprintf(
		"schema diverged: unknown versions %s are not in this binary and pending versions %s are not applied; "+
			"restore the binary that applied the unknown versions or fix the database manually "+
			"(see docs/plans/2026-10-01-shared-data-dir.md)",
		formatVersions(e.Unknown), formatVersions(e.Pending),
	)
}

// SchemaMigrationNotAllowedError reports embedded migrations that were left
// unapplied because the caller asked not to migrate (WithMayMigrate(false)).
// It is the dev-build path: a disposable data directory is required instead.
type SchemaMigrationNotAllowedError struct {
	Pending []int64
}

func (e *SchemaMigrationNotAllowedError) Error() string {
	return fmt.Sprintf(
		"migration not allowed: pending versions %s; point --data-dir at a disposable directory",
		formatVersions(e.Pending),
	)
}

func formatVersions(versions []int64) string {
	if len(versions) == 0 {
		return "[]"
	}
	parts := make([]string, 0, len(versions))
	for _, v := range versions {
		parts = append(parts, strconv.FormatInt(v, 10))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// MigrateOptions controls how migrate reconciles a database with the
// migrations embedded in this binary.
type MigrateOptions struct {
	// MayMigrate reports whether pending migrations may be applied.
	MayMigrate bool
	// MigrationLockPath names the cross-process OS lock that must be held
	// for the whole "read versions, decide, apply" sequence. Empty means no
	// lock is taken, which is the correct default for a caller that already
	// serializes access (an in-memory database) or has no data directory.
	MigrationLockPath string
	// dbPath is the database file the option set was built for. It is only
	// used to describe the migration to the test seam.
	dbPath string
	// PreMigrationChecks run, in order and fail-fast, inside the
	// "pending && MayMigrate" branch while the decision lock is still held,
	// immediately before anything is applied. Empty/nil keeps behavior
	// byte-identical for existing callers. R12-H will register the
	// live-host gate (CheckLiveHosts) here for the destructive async_jobs
	// cutover migration.
	PreMigrationChecks []PreMigrationCheck
}

// PreMigrationCheck is a gate run before pending migrations are applied.
// Returning an error aborts the migration with nothing applied.
type PreMigrationCheck func(ctx context.Context, conn *sql.DB, dbPath string) error

// MigrateOption customizes MigrateOptions.
type MigrateOption func(*MigrateOptions)

// WithMayMigrate sets whether pending migrations may be applied.
func WithMayMigrate(v bool) MigrateOption {
	return func(o *MigrateOptions) { o.MayMigrate = v }
}

// WithPreMigrationChecks appends gates that run before any pending
// migration is applied (while the decision lock is still held).
func WithPreMigrationChecks(checks ...PreMigrationCheck) MigrateOption {
	return func(o *MigrateOptions) { o.PreMigrationChecks = append(o.PreMigrationChecks, checks...) }
}

// WithMigrationLockPath sets the cross-process lock guarding the migration
// decision. Pass an empty path to opt out of locking entirely.
func WithMigrationLockPath(p string) MigrateOption {
	return func(o *MigrateOptions) { o.MigrationLockPath = p }
}

func newMigrateOptions(opts []MigrateOption, dbPath string) MigrateOptions {
	o := MigrateOptions{MayMigrate: true, dbPath: dbPath}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// onVersionsRead, if set, runs after the applied and embedded versions have
// been read but before any decision is taken or migration applied, while the
// migrate.lock (if any) is still held. Test seam only; nil outside tests.
var onVersionsRead func(dbPath string)

// migrationFS returns the embedded schema migrations rooted at their own
// directory, so callers see the bare migration filenames.
func migrationFS() (fs.FS, error) {
	return fs.Sub(FS, "migrations")
}

// migrate reads the applied and embedded migration versions, decides what to
// do about the difference, and applies pending migrations when allowed.
//
// The decision matrix (docs/plans/2026-10-01-shared-data-dir.md §2) is:
//
//	unknown and pending -> SchemaDivergedError, nothing is touched
//	unknown only        -> warn once and open without migrating
//	pending, !MayMigrate-> SchemaMigrationNotAllowedError, nothing is touched
//	pending, MayMigrate -> apply with out-of-order migrations enabled
//	neither             -> no-op
//
// Callers that need the decision to be atomic across processes must pass
// opts.MigrationLockPath and hold that lock for this call.
func migrate(ctx context.Context, conn *sql.DB, dbPath string, fsys fs.FS, opts MigrateOptions) error {
	provider, err := newMigrationProvider(conn, fsys, providerOptions()...)
	if err != nil {
		return fmt.Errorf("failed to initialize goose provider: %w", err)
	}

	applied, err := appliedVersions(ctx, conn, provider)
	if err != nil {
		return err
	}
	embedded, err := embeddedVersions(fsys)
	if err != nil {
		return err
	}
	if onVersionsRead != nil {
		onVersionsRead(dbPath)
	}

	unknown := subtractVersions(applied, embedded)
	pending := subtractVersions(embedded, applied)

	switch {
	case len(unknown) > 0 && len(pending) > 0:
		return &SchemaDivergedError{Unknown: unknown, Pending: pending}
	case len(unknown) > 0:
		// The database is ahead of this binary. Goose itself would ignore
		// those rows (it only raises dbMax), so the writer may continue opening and
		// writing without applying migrations. This warn-only
		// "open without migrating" policy is NOT an old-binary fence: an
		// older binary treats this binary's versions the same way, so a
		// rolling upgrade is made safe by operational quiescence, not by
		// this policy.
		slog.Warn("Database contains unknown migration versions, opening without migrating",
			"unknown", formatVersions(unknown))
		return nil
	case len(pending) > 0 && !opts.MayMigrate:
		return &SchemaMigrationNotAllowedError{Pending: pending}
	case len(pending) > 0:
		// Gates run here, before anything is applied, while the decision
		// lock is still held.
		for i, check := range opts.PreMigrationChecks {
			if err := check(ctx, conn, dbPath); err != nil {
				return fmt.Errorf("pre-migration check %d failed: %w", i+1, err)
			}
		}
		// A second provider is deliberately built for the apply: the one
		// above exists to answer questions, and out-of-order applies need
		// the relaxed version check that may only be enabled here.
		upProvider, err := newMigrationProvider(conn, fsys, goose.WithAllowOutofOrder(true))
		if err != nil {
			return fmt.Errorf("failed to initialize goose provider: %w", err)
		}
		if _, err := upProvider.Up(ctx); err != nil {
			return fmt.Errorf("failed to apply migrations: %w", err)
		}
		return nil
	default:
		return nil
	}
}

// migrateLocked runs migrate while holding the cross-process OS lock named by
// opts.MigrationLockPath, so the whole "read versions, decide, apply" sequence
// is atomic with respect to other processes. The lock is taken even when no
// migration turns out to be needed, because the version read that produces
// that decision is part of the sequence. An empty lock path means the caller
// already serializes access, and no lock is taken.
func migrateLocked(ctx context.Context, conn *sql.DB, dbPath string, fsys fs.FS, opts MigrateOptions) error {
	if opts.MigrationLockPath == "" {
		return migrate(ctx, conn, dbPath, fsys, opts)
	}
	lock, err := filelock.AcquireFileLock(opts.MigrationLockPath)
	if err != nil {
		return fmt.Errorf("failed to acquire migration lock: %w", err)
	}
	defer func() { _ = lock.Release() }()
	return migrate(ctx, conn, dbPath, fsys, opts)
}

// providerOptions returns the goose provider options shared by every provider
// this package builds. The logger is silenced under `go test` because goose's
// default logger writes to stderr for every migration file.
func providerOptions() []goose.ProviderOption {
	if testing.Testing() {
		return []goose.ProviderOption{goose.WithLogger(goose.NopLogger())}
	}
	return nil
}

func newMigrationProvider(conn *sql.DB, fsys fs.FS, extra ...goose.ProviderOption) (*goose.Provider, error) {
	opts := append(providerOptions(), extra...)
	return goose.NewProvider(goose.DialectSQLite3, conn, fsys, opts...)
}

// appliedVersions returns every migration version recorded as applied in the
// database.
//
// goose's Provider.Status is not enough on its own: it only iterates the
// migrations it found on the embedded filesystem, so a version that exists
// only in the database is reported as neither applied nor pending — it is
// invisible. The version table is therefore read directly as well, which is
// what makes unknown-version detection possible.
func appliedVersions(ctx context.Context, conn *sql.DB, provider *goose.Provider) ([]int64, error) {
	// Status also (re)creates the version table on a fresh database, so it
	// must run before the direct read below.
	status, err := provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read migration status: %w", err)
	}
	seen := make(map[int64]struct{}, len(status)+8)
	versions := make([]int64, 0, len(status)+8)
	add := func(v int64) {
		// goose writes a version-0 row as the "table exists, nothing applied
		// yet" sentinel the moment it initializes the version table, so it
		// is never a real migration and must not read as unknown.
		if v <= 0 {
			return
		}
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		versions = append(versions, v)
	}
	for _, s := range status {
		if s.State == goose.StateApplied {
			add(s.Source.Version)
		}
	}

	rows, err := conn.QueryContext(ctx,
		"SELECT version_id FROM goose_db_version WHERE is_applied = 1")
	if err != nil {
		return nil, fmt.Errorf("failed to read applied migration versions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("failed to read applied migration versions: %w", err)
		}
		add(v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read applied migration versions: %w", err)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	return versions, nil
}

// embeddedVersions returns the versions of the *.sql migrations found at the
// root of fsys. Non-migration files are ignored, matching how goose itself
// collects sources.
func embeddedVersions(fsys fs.FS) ([]int64, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("failed to list embedded migrations: %w", err)
	}
	versions := make([]int64, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		if !ok {
			continue
		}
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to parse migration version from %q: %w", entry.Name(), err)
		}
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	return versions, nil
}

// subtractVersions returns from minus minus, sorted ascending.
func subtractVersions(from, minus []int64) []int64 {
	skip := make(map[int64]struct{}, len(minus))
	for _, v := range minus {
		skip[v] = struct{}{}
	}
	out := make([]int64, 0, len(from))
	for _, v := range from {
		if _, ok := skip[v]; !ok {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
