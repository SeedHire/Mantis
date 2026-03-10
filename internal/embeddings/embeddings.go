// Package embeddings provides semantic search over project memory using
// Ollama's embedding API and a local SQLite store.
//
// Storage: float32 binary BLOBs (4× smaller than JSON).
// Retrieval: hybrid BM25 (FTS5) + cosine similarity fused via RRF.
// Indexing: section-aware chunking + SHA-256 content hashing (skip unchanged).
package embeddings

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/seedhire/mantis/internal/ollama"
	_ "modernc.org/sqlite"
)

const (
	// DefaultModel is the embedding model used by default (free via Ollama).
	DefaultModel = "nomic-embed-text"
	dbFileName   = "embeddings.db"
	rrfK         = 60 // RRF constant: score = Σ 1/(rrfK + rank_i)
)

// Chunk represents a stored text chunk with its embedding.
type Chunk struct {
	ID           string
	Source       string // "brain" | "decision" | "rejected" | "conventions" | "session"
	SectionLabel string // section header this chunk came from
	Text         string
	CreatedAt    time.Time
	Score        float64 // RRF score populated during search
}

// Store manages the embeddings database.
type Store struct {
	db     *sql.DB
	client *ollama.Client
	model  string
	dim    int
	dimMu  sync.RWMutex // guards dim: Lock/Unlock for the first write, RLock/RUnlock for reads
}

// Open creates or opens the embeddings database.
// Automatically migrates old schemas (TEXT embedding → BLOB) by dropping the DB;
// embeddings.db is purely derived data and is rebuilt on first index.
func Open(mantisDir string, client *ollama.Client) (*Store, error) {
	dbPath := filepath.Join(mantisDir, dbFileName)

	if needsMigration(dbPath) {
		_ = os.Remove(dbPath) // stale schema — drop and rebuild
	}

	db, err := sql.Open("sqlite", dbPath+"?_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open embeddings db: %w", err)
	}

	if err := createSchema(db); err != nil {
		db.Close()
		return nil, err
	}

	return &Store{
		db:     db,
		client: client,
		model:  DefaultModel,
	}, nil
}

// needsMigration returns true if the DB exists but uses the old TEXT-embedding schema.
func needsMigration(dbPath string) bool {
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return false
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return false
	}
	defer db.Close()

	rows, err := db.Query("PRAGMA table_info(chunks)")
	if err != nil {
		return false
	}
	defer rows.Close()

	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt interface{}
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			continue
		}
		if name == "content_hash" {
			return false // new schema already present
		}
	}
	return true // old schema, migrate
}

func createSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS chunks (
			id            TEXT PRIMARY KEY,
			source        TEXT NOT NULL,
			section_label TEXT NOT NULL DEFAULT '',
			content_hash  TEXT NOT NULL DEFAULT '',
			text          TEXT NOT NULL,
			embedding     BLOB,
			created_at    DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_chunks_source ON chunks(source);
		CREATE INDEX IF NOT EXISTS idx_chunks_hash   ON chunks(content_hash);

		-- FTS5 for BM25 full-text search.
		CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
			id UNINDEXED,
			text,
			content=chunks,
			content_rowid=rowid,
			tokenize='unicode61'
		);

		-- Keep FTS in sync.
		CREATE TRIGGER IF NOT EXISTS chunks_ai AFTER INSERT ON chunks BEGIN
			INSERT INTO chunks_fts(rowid, id, text) VALUES (new.rowid, new.id, new.text);
		END;
		CREATE TRIGGER IF NOT EXISTS chunks_au AFTER UPDATE ON chunks BEGIN
			INSERT INTO chunks_fts(chunks_fts, rowid, id, text) VALUES ('delete', old.rowid, old.id, old.text);
			INSERT INTO chunks_fts(rowid, id, text) VALUES (new.rowid, new.id, new.text);
		END;
		CREATE TRIGGER IF NOT EXISTS chunks_ad AFTER DELETE ON chunks BEGIN
			INSERT INTO chunks_fts(chunks_fts, rowid, id, text) VALUES ('delete', old.rowid, old.id, old.text);
		END;
	`)
	if err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	return nil
}

// Close releases the database connection.
func (s *Store) Close() error { return s.db.Close() }

// Embed generates an embedding vector for the given text.
func (s *Store) Embed(ctx context.Context, text string) ([]float64, error) {
	vec, err := s.client.Embed(ctx, s.model, text)
	if err != nil {
		return nil, err
	}
	// Write-lock only on first call when dim is unset; subsequent calls do a
	// cheap RLock read and skip the write. This prevents a data race under -race.
	s.dimMu.RLock()
	alreadySet := s.dim != 0
	s.dimMu.RUnlock()
	if !alreadySet {
		s.dimMu.Lock()
		if s.dim == 0 { // double-check under write lock
			s.dim = len(vec)
		}
		s.dimMu.Unlock()
	}
	return vec, nil
}

// contentHash returns a 16-char hex hash used for skip-if-unchanged guard.
func contentHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return fmt.Sprintf("%x", h[:8])
}

// encodeVec converts []float64 to little-endian float32 binary (4× smaller than JSON).
func encodeVec(v []float64) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(float32(f)))
	}
	return b
}

// decodeVec converts little-endian float32 binary back to []float64.
func decodeVec(b []byte) []float64 {
	n := len(b) / 4
	v := make([]float64, n)
	for i := range v {
		bits := binary.LittleEndian.Uint32(b[i*4:])
		v[i] = float64(math.Float32frombits(bits))
	}
	return v
}

// Add embeds and stores a text chunk, skipping if content hash is unchanged.
func (s *Store) Add(ctx context.Context, id, source, sectionLabel, text string) error {
	hash := contentHash(text)

	// Skip re-embedding if hash matches stored value.
	var existing string
	scanErr := s.db.QueryRow(`SELECT content_hash FROM chunks WHERE id = ?`, id).Scan(&existing)
	if scanErr == nil && existing == hash {
		return nil
	}

	vec, err := s.Embed(ctx, text)
	if err != nil {
		return fmt.Errorf("embed %q: %w", id, err)
	}

	_, err = s.db.Exec(
		`INSERT OR REPLACE INTO chunks (id, source, section_label, content_hash, text, embedding)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, source, sectionLabel, hash, text, encodeVec(vec),
	)
	return err
}

// Search finds the top-k most similar chunks using hybrid BM25+cosine RRF.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]Chunk, error) {
	if limit <= 0 {
		limit = 5
	}
	return s.SearchHybrid(ctx, query, limit)
}

// SearchHybrid merges BM25 (FTS5) and cosine similarity rankings via RRF.
// Final score = Σ 1/(rrfK + rank_i) across both lists.
func (s *Store) SearchHybrid(ctx context.Context, query string, limit int) ([]Chunk, error) {
	if limit <= 0 {
		limit = 5
	}

	chunkByID := map[string]Chunk{}
	bm25Rank := map[string]int{}

	// ── BM25 via FTS5 (top 20 candidates) ────────────────────────────────────
	// BUG-11: skip FTS5 entirely when the query yields an empty MATCH string
	// (all words ≤ 2 chars); avoids a silent SQLite error that drops BM25 results.
	if ftsQ := ftsQuery(query); ftsQ != "" {
		ftsRows, ftsErr := s.db.QueryContext(ctx,
			`SELECT c.id, c.source, c.section_label, c.text, c.embedding, c.created_at
			 FROM chunks_fts f
			 JOIN chunks c ON c.id = f.id
			 WHERE chunks_fts MATCH ?
			 ORDER BY rank
			 LIMIT 20`,
			ftsQ,
		)
		if ftsErr == nil {
			defer ftsRows.Close()
			rank := 0
			for ftsRows.Next() {
				var c Chunk
				var embBlob []byte
				var createdAt sql.NullTime
				if scanErr := ftsRows.Scan(&c.ID, &c.Source, &c.SectionLabel, &c.Text, &embBlob, &createdAt); scanErr != nil {
					continue
				}
				if createdAt.Valid {
					c.CreatedAt = createdAt.Time
				}
				bm25Rank[c.ID] = rank
				rank++
				chunkByID[c.ID] = c
			}
			// A4: surface truncation from context cancel so callers can handle partial results.
			if err := ftsRows.Err(); err != nil && !errors.Is(err, context.Canceled) {
				return nil, fmt.Errorf("fts scan: %w", err)
			}
		}
	}

	// ── Cosine similarity (capped scan) ──────────────────────────────────────
	// Skip embedding call if BM25 already saturated the result set — saves
	// one round-trip to the Ollama embed endpoint for exact-keyword queries.
	// Otherwise scan up to 200 rows (adaptive: fewer when BM25 is rich).
	queryVec, embedErr := s.Embed(ctx, query)
	cosRank := map[string]int{}

	cosScanLimit := 200
	if len(bm25Rank) >= limit*2 {
		cosScanLimit = 50 // BM25 already dominant; cheap supplemental pass
	}

	if embedErr == nil {
		cosRows, cosErr := s.db.QueryContext(ctx,
			`SELECT id, source, section_label, text, embedding, created_at FROM chunks
			 ORDER BY created_at DESC LIMIT ?`, cosScanLimit)
		if cosErr == nil {
			defer cosRows.Close()
			type entry struct {
				id    string
				score float64
			}
			var scores []entry

			for cosRows.Next() {
				var c Chunk
				var embBlob []byte
				var createdAt sql.NullTime
				if scanErr := cosRows.Scan(&c.ID, &c.Source, &c.SectionLabel, &c.Text, &embBlob, &createdAt); scanErr != nil {
					continue
				}
				if createdAt.Valid {
					c.CreatedAt = createdAt.Time
				}
				if len(embBlob) > 0 {
					c.Score = cosineSimilarity(queryVec, decodeVec(embBlob))
				}
				scores = append(scores, entry{c.ID, c.Score})
				if _, exists := chunkByID[c.ID]; !exists {
					chunkByID[c.ID] = c
				}
			}
			// A4: surface non-cancel errors; context.Canceled means partial results are expected.
			if err := cosRows.Err(); err != nil && !errors.Is(err, context.Canceled) {
				return nil, fmt.Errorf("cosine scan: %w", err)
			}

			sort.Slice(scores, func(i, j int) bool { return scores[i].score > scores[j].score })
			for i, e := range scores {
				cosRank[e.id] = i
			}
		}
	}

	// ── RRF fusion ────────────────────────────────────────────────────────────
	rrfScore := map[string]float64{}
	for id, r := range bm25Rank {
		rrfScore[id] += 1.0 / float64(rrfK+r)
	}
	for id, r := range cosRank {
		rrfScore[id] += 1.0 / float64(rrfK+r)
	}

	var results []Chunk
	for _, c := range chunkByID {
		c.Score = rrfScore[c.ID]
		results = append(results, c)
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })

	if len(results) > limit {
		results = results[:limit]
	}
	return results, nil
}

// SearchBySource searches only chunks from a specific source.
func (s *Store) SearchBySource(ctx context.Context, query, source string, limit int) ([]Chunk, error) {
	if limit <= 0 {
		limit = 5
	}
	chunks, err := s.SearchHybrid(ctx, query, limit*3)
	if err != nil {
		return nil, err
	}
	var filtered []Chunk
	for _, c := range chunks {
		if c.Source == source {
			filtered = append(filtered, c)
		}
		if len(filtered) >= limit {
			break
		}
	}
	return filtered, nil
}

// Count returns the total number of stored chunks, or 0 on error.
func (s *Store) Count() int {
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&count)
	return count
}

// IndexBrainFiles re-indexes all brain files using section-aware chunking.
// Content hashing ensures unchanged chunks are skipped efficiently.
func (s *Store) IndexBrainFiles(ctx context.Context, mantisDir string) error {
	files := []struct {
		name  string
		src   string
		split func(string) []sectionChunk
	}{
		{"BRAIN.md", "brain", splitBrainMD},
		{"DECISIONS.log", "decision", splitDecisionsLog},
		{"REJECTED.md", "rejected", splitRejectedMD},
		{"CONVENTIONS.md", "conventions", splitConventionsMD},
	}

	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(mantisDir, f.name))
		if err != nil {
			continue // file doesn't exist yet
		}
		text := string(data)
		if len(text) < 10 {
			continue
		}
		for i, sc := range f.split(text) {
			id := fmt.Sprintf("%s-%d", f.src, i)
			if err := s.Add(ctx, id, f.src, sc.label, sc.text); err != nil {
				return fmt.Errorf("index %s chunk %d: %w", f.name, i, err)
			}
		}
	}
	return nil
}

// ── Section-aware splitters ───────────────────────────────────────────────────

type sectionChunk struct {
	label string
	text  string
}

func splitBrainMD(text string) []sectionChunk {
	return splitOnHeaders(text, regexp.MustCompile(`(?m)^## `))
}

func splitDecisionsLog(text string) []sectionChunk {
	return splitOnPattern(text, regexp.MustCompile(`(?m)^\[`))
}

func splitRejectedMD(text string) []sectionChunk {
	return splitOnPattern(text, regexp.MustCompile(`(?m)^- \*\*`))
}

func splitConventionsMD(text string) []sectionChunk {
	return splitOnHeaders(text, regexp.MustCompile(`(?m)^## `))
}

// splitOnHeaders splits text at `## Heading` lines, using the heading as the label.
func splitOnHeaders(text string, re *regexp.Regexp) []sectionChunk {
	lines := strings.Split(text, "\n")
	var chunks []sectionChunk
	var current strings.Builder
	label := "intro"

	for _, line := range lines {
		if re.MatchString(line) {
			if current.Len() > 20 {
				chunks = append(chunks, sectionChunk{
					label: label,
					text:  strings.TrimSpace(current.String()),
				})
			}
			current.Reset()
			label = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			current.WriteString(line + "\n")
		} else {
			current.WriteString(line + "\n")
		}
	}
	if current.Len() > 20 {
		chunks = append(chunks, sectionChunk{label: label, text: strings.TrimSpace(current.String())})
	}

	if len(chunks) == 0 {
		for i, c := range splitIntoChunks(text, 800) {
			chunks = append(chunks, sectionChunk{label: fmt.Sprintf("chunk-%d", i), text: c})
		}
	}
	return chunks
}

// splitOnPattern splits text at lines matching the pattern (e.g. timestamps, bullets).
func splitOnPattern(text string, re *regexp.Regexp) []sectionChunk {
	lines := strings.Split(text, "\n")
	var chunks []sectionChunk
	var current strings.Builder
	label := "entry"

	for _, line := range lines {
		if re.MatchString(line) && current.Len() > 5 {
			chunks = append(chunks, sectionChunk{label: label, text: strings.TrimSpace(current.String())})
			current.Reset()
			label = strings.TrimSpace(line)
			if len(label) > 60 {
				label = label[:60]
			}
		}
		current.WriteString(line + "\n")
	}
	if current.Len() > 20 {
		chunks = append(chunks, sectionChunk{label: label, text: strings.TrimSpace(current.String())})
	}

	if len(chunks) == 0 {
		for i, c := range splitIntoChunks(text, 800) {
			chunks = append(chunks, sectionChunk{label: fmt.Sprintf("chunk-%d", i), text: c})
		}
	}
	return chunks
}

// ── Utilities ─────────────────────────────────────────────────────────────────

// ftsQuery escapes the user query for FTS5 MATCH syntax.
func ftsQuery(q string) string {
	words := strings.Fields(strings.ToLower(q))
	if len(words) == 0 {
		return ""
	}
	r := strings.NewReplacer(`"`, ``, `*`, ``, `(`, ``, `)`, ``, `-`, ` `)
	var parts []string
	for _, w := range words {
		w = strings.TrimSpace(r.Replace(w))
		if len(w) > 2 {
			parts = append(parts, `"`+w+`"`)
		}
	}
	if len(parts) == 0 {
		return `"` + words[0] + `"`
	}
	return strings.Join(parts, " AND ")
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
// breaking at paragraph or newline boundaries.
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
		chunk := remaining[:maxChars]
		breakIdx := -1
		for i := len(chunk) - 1; i > maxChars/2; i-- {
			if chunk[i] == '\n' && i+1 < len(chunk) && chunk[i+1] == '\n' {
				breakIdx = i + 1
				break
			}
		}
		if breakIdx < 0 {
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
