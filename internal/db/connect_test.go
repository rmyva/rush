package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConnect_SharesConnectionForSameDataDir(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()

	conn1, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)

	conn2, err := Connect(context.Background(), dataDir)
	require.NoError(t, err)

	require.Same(t, conn1, conn2, "should return the same *sql.DB for the same data dir")

	// Releasing once should not close the connection.
	require.NoError(t, Release(dataDir))
	require.NoError(t, conn1.PingContext(context.Background()), "connection should still be usable after partial release")

	// Releasing again should close it.
	require.NoError(t, Release(dataDir))
	require.Error(t, conn1.PingContext(context.Background()), "connection should be closed after final release")
}

func TestConnect_SeparateConnectionsForDifferentDataDirs(t *testing.T) {
	t.Cleanup(ResetPool)

	dir1 := t.TempDir()
	dir2 := t.TempDir()

	conn1, err := Connect(context.Background(), dir1)
	require.NoError(t, err)

	conn2, err := Connect(context.Background(), dir2)
	require.NoError(t, err)

	require.NotSame(t, conn1, conn2, "different data dirs should get different connections")

	require.NoError(t, Release(dir1))
	require.NoError(t, Release(dir2))
}

func TestRelease_NoopForUnknownDataDir(t *testing.T) {
	t.Cleanup(ResetPool)

	require.NoError(t, Release("/nonexistent/path"), "releasing unknown data dir should not error")
}

type failingReadPoolDriver struct{}

func (failingReadPoolDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("simulated reader connection failure")
}

func init() {
	sql.Register("failing-read-pool-test", failingReadPoolDriver{})
}

func TestConnectRead_FallsBackWhenLazyPoolCannotConnect(t *testing.T) {
	t.Cleanup(ResetPool)
	originalOpenReadDB := onReadDBOpen
	onReadDBOpen = func(string) (*sql.DB, error) {
		return sql.Open("failing-read-pool-test", "")
	}
	t.Cleanup(func() { onReadDBOpen = originalOpenReadDB })

	dataDir := t.TempDir()
	ctx := context.Background()
	writer, err := Connect(ctx, dataDir)
	require.NoError(t, err)
	defer Release(dataDir) //nolint:errcheck

	reader, err := ConnectRead(ctx, dataDir)
	require.NoError(t, err)
	defer Release(dataDir) //nolint:errcheck
	require.Same(t, writer, reader, "unconnectable lazy read pool must fall back to the writer")
}

// TestConnectRead_SeparateHandleSharesRefCount verifies the M-5 read/write
// pool split's basic contract: ConnectRead returns a DIFFERENT *sql.DB from
// Connect's writer for the same dataDir (so reads and writes really are on
// separate connections/pools), but both handles are torn down together —
// one Release per Connect/ConnectRead call, exactly like two Connect calls
// would behave today.
func TestConnectRead_SeparateHandleSharesRefCount(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()
	ctx := context.Background()

	writer, err := Connect(ctx, dataDir)
	require.NoError(t, err)

	reader, err := ConnectRead(ctx, dataDir)
	require.NoError(t, err)

	require.NotSame(t, writer, reader, "the read-only pool must be a distinct *sql.DB from the writer")
	require.NoError(t, reader.PingContext(ctx))

	// A second ConnectRead call for the same dataDir must reuse the same
	// reader handle, mirroring Connect's writer-sharing behavior.
	reader2, err := ConnectRead(ctx, dataDir)
	require.NoError(t, err)
	require.Same(t, reader, reader2, "ConnectRead should share one reader handle per data dir")

	// Three Connect/ConnectRead calls were made (writer, reader, reader2);
	// three Releases are needed to fully tear down.
	require.NoError(t, Release(dataDir))
	require.NoError(t, writer.PingContext(ctx), "writer must still be usable after partial release")
	require.NoError(t, Release(dataDir))
	require.NoError(t, writer.PingContext(ctx), "writer must still be usable after second partial release")

	require.NoError(t, Release(dataDir))
	require.Error(t, writer.PingContext(ctx), "writer should be closed after the final release")
	require.Error(t, reader.PingContext(ctx), "reader should be closed after the final release")
}

// TestConnectRead_ReaderSeesWriterCommits confirms the reader pool isn't
// just a distinct connection but actually observes the writer's committed
// data through WAL — i.e. it is a real, usable read replica of the same
// database file, not an accidentally-empty separate database.
func TestConnectRead_ReaderSeesWriterCommits(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()
	ctx := context.Background()

	writer, err := Connect(ctx, dataDir)
	require.NoError(t, err)
	defer Release(dataDir) //nolint:errcheck

	reader, err := ConnectRead(ctx, dataDir)
	require.NoError(t, err)
	defer Release(dataDir) //nolint:errcheck

	_, err = writer.ExecContext(ctx,
		`INSERT INTO sessions (id, title, message_count, prompt_tokens, completion_tokens, cost, created_at, updated_at)
		 VALUES ('m5-test-session', 'm5', 0, 0, 0, 0, 100, 100)`)
	require.NoError(t, err)

	var title string
	require.NoError(t, reader.QueryRowContext(ctx,
		`SELECT title FROM sessions WHERE id = 'm5-test-session'`).Scan(&title))
	require.Equal(t, "m5", title)

	// The reader pool must actually refuse writes (mode=ro) rather than
	// silently succeeding and risking the WAL/header desync corruption
	// SetMaxOpenConns(1) on the writer exists to prevent (see the doc
	// comment on connect() in connect.go). If this ever starts passing, the
	// read pool has stopped being read-only and the corruption risk this
	// whole design avoids is back.
	_, err = reader.ExecContext(ctx,
		`INSERT INTO sessions (id, title, message_count, prompt_tokens, completion_tokens, cost, created_at, updated_at)
		 VALUES ('m5-should-fail', 'nope', 0, 0, 0, 0, 100, 100)`)
	require.Error(t, err, "the read-only pool must reject writes")
}

// TestWriteDoesNotSerializeBehindLongRead is the M-5 regression guard: on a
// file-backed (non-:memory:) database, a long-running read transaction on
// the reader pool must NOT block a concurrent write on the writer pool.
// Before the read/write split, every read and write shared the single
// SetMaxOpenConns(1) writer connection, so a slow read (e.g.
// GetCallTreeActivityBatch over a large tree, or deep-offset transcript
// pagination) would queue every other DB operation — including agent
// message writes — behind it for as long as the read took. WAL mode is
// designed to let readers and the one writer proceed concurrently; this
// test proves the split actually achieves that instead of just adding an
// unused second pool.
func TestWriteDoesNotSerializeBehindLongRead(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()
	ctx := context.Background()

	writer, err := Connect(ctx, dataDir)
	require.NoError(t, err)
	defer Release(dataDir) //nolint:errcheck

	reader, err := ConnectRead(ctx, dataDir)
	require.NoError(t, err)
	defer Release(dataDir) //nolint:errcheck

	_, err = writer.ExecContext(ctx,
		`INSERT INTO sessions (id, title, message_count, prompt_tokens, completion_tokens, cost, created_at, updated_at)
		 VALUES ('long-read-session', 'seed', 0, 0, 0, 0, 100, 100)`)
	require.NoError(t, err)

	// Start a long-lived read transaction on the reader pool and hold it
	// open past the write's deadline below.
	readTx, err := reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer readTx.Rollback() //nolint:errcheck

	var title string
	require.NoError(t, readTx.QueryRowContext(ctx,
		`SELECT title FROM sessions WHERE id = 'long-read-session'`).Scan(&title))

	const holdDuration = 3 * time.Second
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case <-release:
		case <-time.After(holdDuration):
		}
		readTx.Rollback() //nolint:errcheck
	}()
	defer func() {
		close(release)
		wg.Wait()
	}()

	// While the read transaction is still open, the writer must be able to
	// complete a write well within the transaction's hold time. A
	// generous-but-bounded deadline (half the hold duration) fails the test
	// if the write is queuing behind the read instead of running
	// concurrently with it.
	writeCtx, cancel := context.WithTimeout(ctx, holdDuration/2)
	defer cancel()

	start := time.Now()
	_, err = writer.ExecContext(writeCtx,
		`UPDATE sessions SET title = 'updated' WHERE id = 'long-read-session'`)
	elapsed := time.Since(start)

	require.NoError(t, err, "write should complete without waiting for the concurrent long read to finish")
	require.Less(t, elapsed, holdDuration/2,
		"write took %s — looks like it queued behind the open read transaction instead of running concurrently", elapsed)
}

// TestConcurrentReadersAndWriter_WriteLatencyStable is a lightweight load
// check (not a formal benchmark) for the M-5 split: with N readers
// continuously issuing reads against the reader pool, M writers issuing
// writes against the writer pool should see write latency stay roughly flat
// as N grows, instead of degrading linearly the way a single shared
// connection would (every extra reader adds to the same queue every writer
// sits behind).
func TestConcurrentReadersAndWriter_WriteLatencyStable(t *testing.T) {
	t.Cleanup(ResetPool)

	dataDir := t.TempDir()
	ctx := context.Background()

	writer, err := Connect(ctx, dataDir)
	require.NoError(t, err)
	defer Release(dataDir) //nolint:errcheck

	reader, err := ConnectRead(ctx, dataDir)
	require.NoError(t, err)
	defer Release(dataDir) //nolint:errcheck

	_, err = writer.ExecContext(ctx,
		`INSERT INTO sessions (id, title, message_count, prompt_tokens, completion_tokens, cost, created_at, updated_at)
		 VALUES ('load-session', 'seed', 0, 0, 0, 0, 100, 100)`)
	require.NoError(t, err)

	measureWriteLatency := func(readerCount int) time.Duration {
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for range readerCount {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-stop:
						return
					default:
					}
					var title string
					_ = reader.QueryRowContext(ctx,
						`SELECT title FROM sessions WHERE id = 'load-session'`).Scan(&title)
				}
			}()
		}
		// Let the readers ramp up before measuring.
		time.Sleep(50 * time.Millisecond)

		const writes = 20
		start := time.Now()
		for i := range writes {
			_, err := writer.ExecContext(ctx,
				`UPDATE sessions SET message_count = ? WHERE id = 'load-session'`, i)
			require.NoError(t, err)
		}
		elapsed := time.Since(start)

		close(stop)
		wg.Wait()
		return elapsed / writes
	}

	// Best of three, and the two series interleaved rather than run one
	// after the other. Both details attack the same failure: this measures
	// wall-clock on a machine whose load it does not control, and it flaked
	// when the whole package ran under -race. Scheduler interference can
	// only ever make a sample SLOWER, so the minimum of several is the
	// closest estimate of the real cost; interleaving stops the first
	// series from being systematically luckier than the second.
	const samples = 3
	baseline := time.Duration(math.MaxInt64)
	loaded := time.Duration(math.MaxInt64)
	for range samples {
		baseline = min(baseline, measureWriteLatency(0))
		loaded = min(loaded, measureWriteLatency(16))
	}

	t.Logf("avg write latency (best of %d): 0 readers=%s, 16 readers=%s", samples, baseline, loaded)

	// Allow generous headroom (this runs on shared CI hardware and SQLite
	// driver overhead varies) — the point is proving there's no gross
	// linear-with-reader-count blowup, not pinning an exact ratio. Before
	// the split (single shared connection), adding concurrent readers to
	// the same queue the writer sits in would multiply latency roughly
	// with reader count; a bounded constant-factor slowdown here is the
	// signature of readers and the writer actually running concurrently.
	require.Less(t, loaded, baseline*10+20*time.Millisecond,
		"write latency degraded too much under concurrent read load (baseline=%s, loaded=%s) — reads may be serializing with writes again", baseline, loaded)
}
