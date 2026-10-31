package history

import (
	"context"
	"database/sql"
	"testing"

	"github.com/PHPCraftdream/rush/internal/db"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

type recordingDBTX struct {
	db.DBTX
	queryContextSQL    []string
	queryRowContextSQL []string
}

func (r *recordingDBTX) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	r.queryContextSQL = append(r.queryContextSQL, query)
	return r.DBTX.QueryContext(ctx, query, args...)
}

func (r *recordingDBTX) QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row {
	r.queryRowContextSQL = append(r.queryRowContextSQL, query)
	return r.DBTX.QueryRowContext(ctx, query, args...)
}

func TestListLatestSessionFilesRanksWithinSessionAndPath(t *testing.T) {
	ctx := context.Background()
	database, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	_, err = database.ExecContext(ctx, `CREATE TABLE files (
		id TEXT PRIMARY KEY, session_id TEXT NOT NULL, path TEXT NOT NULL,
		content TEXT NOT NULL, version INTEGER NOT NULL, created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL
	)`)
	require.NoError(t, err)
	for _, file := range []struct {
		id, sessionID, content string
		version, createdAt     int64
	}{
		{id: "requested-v2", sessionID: "requested", content: "latest by version", version: 2, createdAt: 100},
		{id: "requested-v1", sessionID: "requested", content: "max timestamp but older version", version: 1, createdAt: 200},
		{id: "other-v3", sessionID: "other", content: "other session", version: 3, createdAt: 300},
	} {
		_, err := database.ExecContext(ctx, `INSERT INTO files VALUES (?, ?, ?, ?, ?, ?, ?)`,
			file.id, file.sessionID, "/shared", file.content, file.version, file.createdAt, file.createdAt)
		require.NoError(t, err)
	}

	files, err := db.New(database).ListLatestSessionFiles(ctx, "requested")
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "requested-v2", files[0].ID)
	require.Equal(t, "latest by version", files[0].Content)
}

func TestCreateVersionUsesNextPathVersion(t *testing.T) {
	ctx := context.Background()
	database, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	_, err = database.ExecContext(ctx, `CREATE TABLE sessions (id TEXT PRIMARY KEY)`)
	require.NoError(t, err)
	_, err = database.ExecContext(ctx, `INSERT INTO sessions VALUES ('prior-session'), ('new-session')`)
	require.NoError(t, err)
	_, err = database.ExecContext(ctx, `CREATE TABLE files (
		id TEXT PRIMARY KEY, session_id TEXT NOT NULL, path TEXT NOT NULL,
		content TEXT NOT NULL, version INTEGER NOT NULL, created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL, FOREIGN KEY(session_id) REFERENCES sessions(id),
		UNIQUE(path, session_id, version)
	)`)
	require.NoError(t, err)
	recorder := &recordingDBTX{DBTX: database}
	q := db.New(recorder)
	service := NewService(q, database).(*service)

	for _, tc := range []struct {
		name string
		seed []int64
		want int64
	}{
		{name: "empty", want: 0},
		{name: "one", seed: []int64{0}, want: 1},
		{name: "many", seed: []int64{0, 1, 2, 3}, want: 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := database.ExecContext(ctx, `DELETE FROM files`)
			require.NoError(t, err)
			for i, version := range tc.seed {
				_, err := database.ExecContext(ctx, `INSERT INTO files VALUES (?, ?, ?, ?, ?, ?, ?)`,
					string(rune('a'+i)), "prior-session", "/target", "content", version, i, i)
				require.NoError(t, err)
			}

			recorder.queryContextSQL = nil
			recorder.queryRowContextSQL = nil
			file, err := service.CreateVersion(ctx, "new-session", "/target", "new content")
			require.NoError(t, err)
			require.Equal(t, tc.want, file.Version)

			require.Empty(t, recorder.queryContextSQL, "CreateVersion must not fetch a list of file snapshots")
			require.Len(t, recorder.queryRowContextSQL, 1, "CreateVersion should perform one scalar lookup")
			require.Contains(t, recorder.queryRowContextSQL[0], "COALESCE(MAX(version) + 1, 0)")
			require.Contains(t, recorder.queryRowContextSQL[0], "WHERE path = ?")
		})
	}
}
