package semantic

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite/vec" // registers vec0 extension via init()

	"github.com/charmbracelet/crush/internal/symbols"
)

// Store manages chunk and vector storage in the project's SQLite database.
type Store struct {
	db  *sql.DB
	cfg EmbeddingConfig
}

// NewStore creates a semantic store backed by the given database. It
// ensures the vec0 virtual table exists. The dimension must match the
// configured embedding model.
func NewStore(ctx context.Context, db *sql.DB, cfg EmbeddingConfig) (*Store, error) {
	s := &Store{db: db, cfg: cfg}
	if err := s.ensureVecTable(ctx); err != nil {
		return nil, fmt.Errorf("init vec table: %w", err)
	}
	return s, nil
}

// vecDimensionPattern pulls N out of an existing chunk_vectors DDL's
// "embedding float[N]" column so a dimension change can be detected.
var vecDimensionPattern = regexp.MustCompile(`float\[(\d+)\]`)

// ensureVecTable creates the chunk_vectors virtual table if it does not
// already exist. This cannot be done in a goose migration because vec0
// is only available after the extension registers at runtime.
func (s *Store) ensureVecTable(ctx context.Context) error {
	// CREATE VIRTUAL TABLE IF NOT EXISTS silently keeps whatever dimension
	// the table was first created with, so a changed embedding dimension
	// would be ignored and every subsequent insert would mismatch. Fail
	// with something actionable instead.
	var existingDDL string
	switch err := s.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'chunk_vectors'`,
	).Scan(&existingDDL); {
	case err == nil:
		if m := vecDimensionPattern.FindStringSubmatch(existingDDL); m != nil {
			if dim, convErr := strconv.Atoi(m[1]); convErr == nil && dim != s.cfg.Dimension {
				return fmt.Errorf(
					"chunk_vectors was built for dimension %d but the configured embedding dimension is %d; drop the chunks and chunk_vectors tables to re-index at the new dimension",
					dim, s.cfg.Dimension)
			}
		}
	case errors.Is(err, sql.ErrNoRows):
		// Not created yet; the statement below makes it.
	default:
		return fmt.Errorf("inspect chunk_vectors: %w", err)
	}

	query := fmt.Sprintf(`
		CREATE VIRTUAL TABLE IF NOT EXISTS chunk_vectors USING vec0(
			chunk_id INTEGER PRIMARY KEY,
			embedding float[%d] distance_metric=cosine,
			kind TEXT,
			updated_at INTEGER
		)`, s.cfg.Dimension)
	_, err := s.db.ExecContext(ctx, query)
	if err != nil {
		return fmt.Errorf("create chunk_vectors table: %w", err)
	}

	// Trigger to cascade deletes from chunks to chunk_vectors.
	_, err = s.db.ExecContext(ctx, `
		CREATE TRIGGER IF NOT EXISTS chunks_delete_cascade_vectors
		AFTER DELETE ON chunks
		BEGIN
			DELETE FROM chunk_vectors WHERE chunk_id = old.chunk_id;
		END`)
	if err != nil {
		return fmt.Errorf("create cascade trigger: %w", err)
	}

	return nil
}

// Chunk represents a piece of content ready for embedding.
type Chunk struct {
	SessionID sql.NullString
	Path      sql.NullString
	Symbol    sql.NullString
	StartLine int
	EndLine   int
	Content   string
	FileHash  sql.NullString
}

// SearchResult is a single hit from a KNN search.
type SearchResult struct {
	ChunkID   int64
	Path      string
	Symbol    string
	StartLine int
	EndLine   int
	Content   string
	Score     float64
}

// IndexFile extracts symbols from a file, chunks them, generates
// embeddings, and stores both chunks and vectors. It skips files whose
// hash hasn't changed since the last index.
func (s *Store) IndexFile(ctx context.Context, extractor *symbols.Extractor, path string) (indexed int, skipped bool, err error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return 0, false, fmt.Errorf("read file: %w", err)
	}

	hash := fileHash(src)

	// Skip only when the stored chunks came from the same file contents
	// *and* the same embedding model at the same dimension. Hash alone is
	// not enough: switching models leaves every file's hash unchanged, so
	// the whole index would be skipped and silently keep vectors from the
	// old model, which search then compares against new-model queries.
	var (
		existingHash  sql.NullString
		existingModel string
		existingDim   int
	)
	err = s.db.QueryRowContext(ctx,
		`SELECT file_hash, model, dim FROM chunks WHERE path = ? LIMIT 1`, path).
		Scan(&existingHash, &existingModel, &existingDim)
	if err == nil &&
		existingHash.Valid && existingHash.String == hash &&
		existingModel == s.cfg.Model &&
		existingDim == s.cfg.Dimension {
		return 0, true, nil
	}

	result := extractor.ExtractFile(path)

	var chunks []Chunk
	if result.Fallback || len(result.Symbols) == 0 {
		chunks = append(chunks, Chunk{
			Path:      sql.NullString{String: path, Valid: true},
			Content:   string(src),
			FileHash:  sql.NullString{String: hash, Valid: true},
			StartLine: 0,
			EndLine:   strings.Count(string(src), "\n"),
		})
	} else {
		lines := strings.Split(string(src), "\n")
		for _, sym := range result.Symbols {
			start := sym.StartLine
			end := sym.EndLine
			if end >= len(lines) {
				end = len(lines) - 1
			}
			content := strings.Join(lines[start:end+1], "\n")
			if strings.TrimSpace(content) == "" {
				continue
			}
			chunks = append(chunks, Chunk{
				Path:      sql.NullString{String: path, Valid: true},
				Symbol:    sql.NullString{String: sym.Name, Valid: true},
				StartLine: start,
				EndLine:   end,
				Content:   content,
				FileHash:  sql.NullString{String: hash, Valid: true},
			})
		}
	}

	if len(chunks) == 0 {
		return 0, false, nil
	}

	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Content
	}

	client := NewClient(s.cfg)
	embeddings, err := client.Embed(ctx, texts)
	if err != nil {
		return 0, false, fmt.Errorf("generate embeddings: %w", err)
	}
	// embeddings is indexed positionally against chunks below, so a short
	// response would panic rather than fail.
	if len(embeddings) != len(chunks) {
		return 0, false, fmt.Errorf("embedding count mismatch: got %d for %d chunks", len(embeddings), len(chunks))
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Delete old chunks for this path before inserting new ones.
	_, err = tx.ExecContext(ctx, `DELETE FROM chunks WHERE path = ?`, path)
	if err != nil {
		return 0, false, fmt.Errorf("delete old chunks: %w", err)
	}

	now := time.Now().UnixMilli()
	insertChunk := `INSERT INTO chunks (session_id, path, symbol, start_line, end_line, content, file_hash, model, dim)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	insertVec := `INSERT INTO chunk_vectors (chunk_id, embedding, kind, updated_at) VALUES (?, ?, 'code', ?)`

	for i, chunk := range chunks {
		res, err := tx.ExecContext(ctx, insertChunk,
			chunk.SessionID, chunk.Path, chunk.Symbol,
			chunk.StartLine, chunk.EndLine, chunk.Content,
			chunk.FileHash, s.cfg.Model, s.cfg.Dimension)
		if err != nil {
			return 0, false, fmt.Errorf("insert chunk: %w", err)
		}

		chunkID, err := res.LastInsertId()
		if err != nil {
			return 0, false, fmt.Errorf("get chunk id: %w", err)
		}

		vecBytes := float32ToBytes(embeddings[i])
		_, err = tx.ExecContext(ctx, insertVec, chunkID, vecBytes, now)
		if err != nil {
			return 0, false, fmt.Errorf("insert vector: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("commit: %w", err)
	}

	return len(chunks), false, nil
}

// Search performs a KNN search over code chunks, returning the top-k
// results ordered by cosine similarity.
func (s *Store) Search(ctx context.Context, query []float32, k int) ([]SearchResult, error) {
	if k <= 0 {
		k = 10
	}

	vecBytes := float32ToBytes(query)
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.chunk_id, COALESCE(c.path, ''), COALESCE(c.symbol, ''),
		       c.start_line, c.end_line, c.content, v.distance
		FROM chunk_vectors v
		JOIN chunks c ON c.chunk_id = v.chunk_id
		WHERE v.embedding MATCH ? AND k = ?
		ORDER BY v.distance
		LIMIT ?`, vecBytes, k, k)
	if err != nil {
		return nil, fmt.Errorf("knn search: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var distance float64
		if err := rows.Scan(&r.ChunkID, &r.Path, &r.Symbol,
			&r.StartLine, &r.EndLine, &r.Content, &distance); err != nil {
			return nil, fmt.Errorf("scan result: %w", err)
		}
		r.Score = 1.0 - distance // convert distance to similarity
		results = append(results, r)
	}

	return results, rows.Err()
}

// ChunkCount returns the total number of indexed chunks.
func (s *Store) ChunkCount(ctx context.Context) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunks`).Scan(&count)
	return count, err
}

func fileHash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// float32ToBytes converts a float32 slice to a little-endian byte slice
// for vec0 storage.
func float32ToBytes(vec []float32) []byte {
	b := make([]byte, len(vec)*4)
	for i, v := range vec {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(v))
	}
	return b
}
