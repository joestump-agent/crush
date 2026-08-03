package semantic

import (
	"context"
	"database/sql"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/charmbracelet/crush/internal/symbols"
)

// newTestDB creates an in-memory SQLite database with the chunks table
// and vec0 virtual table for testing.
func newTestDB(t *testing.T, dim int) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)

	_, err = db.Exec(`PRAGMA foreign_keys = ON`)
	require.NoError(t, err)

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY)`)
	require.NoError(t, err)

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chunks (
			chunk_id   INTEGER PRIMARY KEY,
			session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE,
			path       TEXT,
			symbol     TEXT,
			start_line INTEGER,
			end_line   INTEGER,
			content    TEXT NOT NULL,
			file_hash  TEXT,
			model      TEXT NOT NULL,
			dim        INTEGER NOT NULL
		) STRICT`)
	require.NoError(t, err)

	cfg := EmbeddingConfig{Dimension: dim}
	store := &Store{db: db, cfg: cfg}
	require.NoError(t, store.ensureVecTable())

	return db
}

func float32Slice(vals ...float32) []byte {
	b := make([]byte, len(vals)*4)
	for i, v := range vals {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(v))
	}
	return b
}

func TestCascadeDelete(t *testing.T) {
	t.Parallel()
	db := newTestDB(t, 4)
	defer db.Close()

	ctx := context.Background()

	// Insert a session, chunk, and vector.
	_, err := db.ExecContext(ctx, `INSERT INTO sessions (id) VALUES ('sess-1')`)
	require.NoError(t, err)

	res, err := db.ExecContext(ctx,
		`INSERT INTO chunks (session_id, path, content, model, dim) VALUES ('sess-1', 'test.go', 'func Foo() {}', 'test-model', 4)`)
	require.NoError(t, err)
	chunkID, _ := res.LastInsertId()

	_, err = db.ExecContext(ctx,
		`INSERT INTO chunk_vectors (chunk_id, embedding, kind, updated_at) VALUES (?, ?, 'code', 1000)`,
		chunkID, float32Slice(0.1, 0.2, 0.3, 0.4))
	require.NoError(t, err)

	// Verify vector exists.
	var count int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunk_vectors`).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)

	// Delete the session — cascade should remove chunk and vector.
	_, err = db.ExecContext(ctx, `DELETE FROM sessions WHERE id = 'sess-1'`)
	require.NoError(t, err)

	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks`).Scan(&count)
	require.NoError(t, err)
	assert.Zero(t, count, "chunks should be cascade-deleted")

	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunk_vectors`).Scan(&count)
	require.NoError(t, err)
	assert.Zero(t, count, "vectors should be cascade-deleted via trigger")
}

func TestFileHashInvalidation(t *testing.T) {
	t.Parallel()
	db := newTestDB(t, 4)
	defer db.Close()

	cfg := EmbeddingConfig{Dimension: 4, Model: "test"}
	store := &Store{db: db, cfg: cfg}

	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "sample.go")
	require.NoError(t, os.WriteFile(path, []byte("package main\nfunc Hello() {}\n"), 0o644))

	hash := fileHash([]byte("package main\nfunc Hello() {}\n"))

	// Insert a chunk with this hash.
	_, err := db.Exec(`INSERT INTO chunks (path, content, file_hash, model, dim) VALUES (?, 'content', ?, 'test', 4)`, path, hash)
	require.NoError(t, err)

	extractor := symbols.NewExtractor()
	indexed, skipped, err := store.IndexFile(context.Background(), extractor, path)
	require.NoError(t, err)
	assert.True(t, skipped, "should skip unchanged file")
	assert.Zero(t, indexed)
}

func TestFloat32ToBytes(t *testing.T) {
	t.Parallel()
	vec := []float32{1.0, -1.0, 0.5}
	b := float32ToBytes(vec)
	assert.Len(t, b, 12)

	// Verify round-trip.
	for i, v := range vec {
		got := math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
		assert.Equal(t, v, got)
	}
}

func TestChunkCount(t *testing.T) {
	t.Parallel()
	db := newTestDB(t, 4)
	defer db.Close()

	cfg := EmbeddingConfig{Dimension: 4, Model: "test"}
	store := &Store{db: db, cfg: cfg}

	ctx := context.Background()

	count, err := store.ChunkCount(ctx)
	require.NoError(t, err)
	assert.Zero(t, count)

	_, err = db.ExecContext(ctx,
		`INSERT INTO chunks (path, content, model, dim) VALUES ('a.go', 'content', 'test', 4)`)
	require.NoError(t, err)

	count, err = store.ChunkCount(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
}

func TestSearchEmpty(t *testing.T) {
	t.Parallel()
	db := newTestDB(t, 4)
	defer db.Close()

	cfg := EmbeddingConfig{Dimension: 4, Model: "test"}
	store := &Store{db: db, cfg: cfg}

	results, err := store.Search(context.Background(), []float32{0.1, 0.2, 0.3, 0.4}, 5)
	require.NoError(t, err)
	assert.Empty(t, results)
}

func TestNewStore(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer db.Close()

	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS sessions (id TEXT PRIMARY KEY)`)
	require.NoError(t, err)

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chunks (
			chunk_id   INTEGER PRIMARY KEY,
			session_id TEXT REFERENCES sessions(id) ON DELETE CASCADE,
			path       TEXT,
			symbol     TEXT,
			start_line INTEGER,
			end_line   INTEGER,
			content    TEXT NOT NULL,
			file_hash  TEXT,
			model      TEXT NOT NULL,
			dim        INTEGER NOT NULL
		) STRICT`)
	require.NoError(t, err)

	cfg := EmbeddingConfig{Dimension: 768, Model: "test"}
	store, err := NewStore(db, cfg)
	require.NoError(t, err)
	assert.NotNil(t, store)

	// Verify vec table was created.
	var name string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='chunk_vectors'`).Scan(&name)
	require.NoError(t, err)
	assert.Equal(t, "chunk_vectors", name)
}
