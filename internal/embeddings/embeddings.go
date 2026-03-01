// Package embeddings provides semantic search over project memory using
// Ollama's embedding API and a local SQLite store with cosine similarity.
package embeddings

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "modernc.org/sqlite"
	"github.com/seedhire/mantis/internal/ollama"
)

const (
	// DefaultModel is the embedding model used by default (free via Ollama).
	DefaultModel = "nomic-embed-text"
	dbFileName   = "embeddings.db"
)

// Chunk represents a stored text chunk with its embedding.
type Chunk struct {
	ID        string
	Source    string  // e.g. "session", "decision", "brain"
	Text      string
	CreatedAt time.Time
	Score     float64 // populated during search
}

// Store manages the embeddings database.
type Store struct {
	db     *sql.DB
	client *ollama.Client
	model  string
	dim    int // embedding dimension, detected on first embed
}

// Open creates or opens the embeddings database in the given .mantis/ directory.
func Open(mantisDir string, client *ollama.Client) (*Store, error) {
	dbPath := filepath.Join(mantisDir, dbFileName)
	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open embeddings db: %w", err)
	}

	// Create schema.
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chunks (
			id         TEXT PRIMARY KEY,
			source     TEXT NOT NULL,
			text       TEXT NOT NULL,
			embedding  TEXT NOT NULL,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_chunks_source ON chunks(source);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	return &Store{
		db:     db,
		client: client,
		model:  DefaultModel,
	}, nil
}

// Close releases the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// Embed generates an embedding vector for the given text.
func (s *Store) Embed(ctx context.Context, text string) ([]float64, error) {
	vec, err := s.client.Embed(ctx, s.model, text)
	if err != nil {
		return nil, err
	}
	if s.dim == 0 {
		s.dim = len(vec)
	}
	return vec, nil
}

// Add embeds and stores a text chunk.
func (s *Store) Add(ctx context.Context, id, source, text string) error {
	vec, err := s.Embed(ctx, text)
	if err != nil {
		return fmt.Errorf("embed %q: %w", id, err)
	}

	vecJSON, err := json.Marshal(vec)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(
		`INSERT OR REPLACE INTO chunks (id, source, text, embedding) VALUES (?, ?, ?, ?)`,
		id, source, text, string(vecJSON),
	)
	return err
}

// Search finds the top-k most similar chunks to the query text.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]Chunk, error) {
	if limit <= 0 {
		limit = 3
	}

	queryVec, err := s.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	rows, err := s.db.Query(`SELECT id, source, text, embedding, created_at FROM chunks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []Chunk
	for rows.Next() {
		var c Chunk
		var vecJSON string
		var createdAt sql.NullTime
		if err := rows.Scan(&c.ID, &c.Source, &c.Text, &vecJSON, &createdAt); err != nil {
			continue
		}
		if createdAt.Valid {
			c.CreatedAt = createdAt.Time
		}

		var vec []float64
		if err := json.Unmarshal([]byte(vecJSON), &vec); err != nil {
			continue
		}

		c.Score = cosineSimilarity(queryVec, vec)
		results = append(results, c)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if len(results) > limit {
		results = results[:limit]
	}

	return results, nil
}

// SearchBySource searches only chunks from a specific source.
func (s *Store) SearchBySource(ctx context.Context, query, source string, limit int) ([]Chunk, error) {
	if limit <= 0 {
		limit = 3
	}

	queryVec, err := s.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	rows, err := s.db.Query(`SELECT id, source, text, embedding, created_at FROM chunks WHERE source = ?`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []Chunk
	for rows.Next() {
		var c Chunk
		var vecJSON string
		var createdAt sql.NullTime
		if err := rows.Scan(&c.ID, &c.Source, &c.Text, &vecJSON, &createdAt); err != nil {
			continue
		}
		if createdAt.Valid {
			c.CreatedAt = createdAt.Time
		}

		var vec []float64
		if err := json.Unmarshal([]byte(vecJSON), &vec); err != nil {
			continue
		}

		c.Score = cosineSimilarity(queryVec, vec)
		results = append(results, c)
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	if len(results) > limit {
		results = results[:limit]
	}

	return results, nil
}

// Count returns the total number of stored chunks.
func (s *Store) Count() int {
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&count)
	return count
}

// IndexBrainFiles embeds and indexes all brain files for semantic search.
func (s *Store) IndexBrainFiles(ctx context.Context, mantisDir string) error {
	files := []struct {
		name   string
		source string
	}{
		{"BRAIN.md", "brain"},
		{"DECISIONS.log", "decision"},
		{"REJECTED.md", "rejected"},
		{"CONVENTIONS.md", "conventions"},
	}

	for _, f := range files {
		path := filepath.Join(mantisDir, f.name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue // File doesn't exist yet.
		}
		text := string(data)
		if len(text) < 10 {
			continue // Skip near-empty files.
		}

		// Split large files into chunks of ~500 chars for better retrieval.
		chunks := splitIntoChunks(text, 500)
		for i, chunk := range chunks {
			id := fmt.Sprintf("%s-%d", f.source, i)
			if err := s.Add(ctx, id, f.source, chunk); err != nil {
				return fmt.Errorf("index %s chunk %d: %w", f.name, i, err)
			}
		}
	}

	return nil
}

// cosineSimilarity computes the cosine similarity between two vectors.
func cosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}

	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}

	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}

// splitIntoChunks splits text into chunks of approximately maxChars,
// breaking at paragraph boundaries where possible.
func splitIntoChunks(text string, maxChars int) []string {
	if len(text) <= maxChars {
		return []string{text}
	}

	var chunks []string
	remaining := text
	for len(remaining) > 0 {
		if len(remaining) <= maxChars {
			chunks = append(chunks, remaining)
			break
		}

		// Try to break at a paragraph boundary.
		chunk := remaining[:maxChars]
		breakIdx := -1
		for i := len(chunk) - 1; i > maxChars/2; i-- {
			if chunk[i] == '\n' && i+1 < len(chunk) && chunk[i+1] == '\n' {
				breakIdx = i + 1
				break
			}
		}
		if breakIdx < 0 {
			// Fall back to newline.
			for i := len(chunk) - 1; i > maxChars/2; i-- {
				if chunk[i] == '\n' {
					breakIdx = i + 1
					break
				}
			}
		}
		if breakIdx < 0 {
			breakIdx = maxChars
		}

		chunks = append(chunks, remaining[:breakIdx])
		remaining = remaining[breakIdx:]
	}

	return chunks
}
