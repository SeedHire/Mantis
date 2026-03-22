package intel

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FileChurn holds temporal intelligence for a single file.
type FileChurn struct {
	Path            string
	Commits         int
	Authors         int
	AuthorNames     []string
	LinesAdded      int
	LinesDeleted    int
	ChurnScore      float64 // (added + deleted) / total commits — instability signal
	DaysSinceChange int
	LastAuthor      string
}

// CoupledFile represents two files that frequently change together.
type CoupledFile struct {
	FileA     string
	FileB     string
	CoChanges int     // times changed in the same commit
	Coupling  float64 // coChanges / min(commitsA, commitsB)
}

// TemporalStats holds aggregate temporal intelligence for the project.
type TemporalStats struct {
	Files    []FileChurn
	Coupling []CoupledFile
	Since    time.Time
}

// temporalCache caches results to avoid re-spawning git processes on repeat calls.
var temporalCache struct {
	mu     sync.RWMutex
	stats  *TemporalStats
	root   string
	days   int
	expiry time.Time
}

const temporalCacheTTL = 2 * time.Minute

// Temporal analyzes git history for the given project root.
// lookbackDays controls how far back to look (default 90).
// Results are cached for 2 minutes to avoid redundant git calls.
func Temporal(root string, lookbackDays int) (*TemporalStats, error) {
	if lookbackDays <= 0 {
		lookbackDays = 90
	}

	// Check cache.
	temporalCache.mu.RLock()
	if temporalCache.stats != nil && temporalCache.root == root &&
		temporalCache.days == lookbackDays && time.Now().Before(temporalCache.expiry) {
		cached := temporalCache.stats
		temporalCache.mu.RUnlock()
		return cached, nil
	}
	temporalCache.mu.RUnlock()

	since := time.Now().AddDate(0, 0, -lookbackDays)
	sinceStr := since.Format("2006-01-02")

	files, err := analyzeChurn(root, sinceStr)
	if err != nil {
		return nil, fmt.Errorf("churn analysis: %w", err)
	}

	coupling, err := analyzeCoupling(root, sinceStr, files)
	if err != nil {
		// Non-fatal: coupling is bonus data.
		coupling = nil
	}

	result := &TemporalStats{
		Files:    files,
		Coupling: coupling,
		Since:    since,
	}

	// Store in cache.
	temporalCache.mu.Lock()
	temporalCache.stats = result
	temporalCache.root = root
	temporalCache.days = lookbackDays
	temporalCache.expiry = time.Now().Add(temporalCacheTTL)
	temporalCache.mu.Unlock()

	return result, nil
}

// InvalidateTemporalCache forces the next Temporal call to re-run git.
func InvalidateTemporalCache() {
	temporalCache.mu.Lock()
	temporalCache.stats = nil
	temporalCache.mu.Unlock()
}

// Hotspots returns the top N files ranked by churn × author diversity.
func Hotspots(stats *TemporalStats, limit int) []FileChurn {
	scored := make([]FileChurn, len(stats.Files))
	copy(scored, stats.Files)

	sort.Slice(scored, func(i, j int) bool {
		si := scored[i].ChurnScore * float64(scored[i].Authors)
		sj := scored[j].ChurnScore * float64(scored[j].Authors)
		return si > sj
	})

	if limit > 0 && limit < len(scored) {
		scored = scored[:limit]
	}
	return scored
}

// Risky returns files with high churn but low bus factor (few authors).
func Risky(stats *TemporalStats, limit int) []FileChurn {
	var risky []FileChurn
	for _, f := range stats.Files {
		if f.Commits >= 3 && f.Authors <= 1 {
			risky = append(risky, f)
		}
	}
	sort.Slice(risky, func(i, j int) bool {
		return risky[i].ChurnScore > risky[j].ChurnScore
	})
	if limit > 0 && limit < len(risky) {
		risky = risky[:limit]
	}
	return risky
}

// CouplingFor returns files that frequently change with the given path.
func CouplingFor(stats *TemporalStats, path string, limit int) []CoupledFile {
	var result []CoupledFile
	for _, c := range stats.Coupling {
		if c.FileA == path || c.FileB == path {
			result = append(result, c)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Coupling > result[j].Coupling
	})
	if limit > 0 && limit < len(result) {
		result = result[:limit]
	}
	return result
}

// analyzeChurn uses git log --numstat to compute per-file churn.
func analyzeChurn(root, since string) ([]FileChurn, error) {
	// Get per-commit file changes with author info.
	out, err := exec.Command("git", "-C", root, "log",
		"--since="+since,
		"--format=COMMIT|%an",
		"--numstat",
	).Output()
	if err != nil {
		return nil, err
	}

	type fileStats struct {
		commits    map[string]bool // commit hashes (using author as proxy)
		authors    map[string]bool
		added      int
		deleted    int
		lastAuthor string
	}

	stats := make(map[string]*fileStats)
	var currentAuthor string
	commitID := 0

	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "COMMIT|") {
			currentAuthor = strings.TrimPrefix(line, "COMMIT|")
			commitID++
			continue
		}

		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}

		// Skip binary files (git shows - for binary).
		if parts[0] == "-" || parts[1] == "-" {
			continue
		}

		added, _ := strconv.Atoi(parts[0])
		deleted, _ := strconv.Atoi(parts[1])

		// Handle renames: numstat shows "N M old => new" or "N M {prefix/old => prefix/new}".
		// Join remaining parts to get the full path, then extract the new name.
		path := strings.Join(parts[2:], " ")
		if idx := strings.Index(path, " => "); idx != -1 {
			path = path[idx+4:]
		}
		// Strip curly-brace rename syntax: "{old => new}/suffix" → "new/suffix"
		if strings.Contains(path, "}") {
			path = strings.ReplaceAll(path, "}", "")
			path = strings.ReplaceAll(path, "{", "")
		}
		path = strings.TrimSpace(path)

		fs, ok := stats[path]
		if !ok {
			fs = &fileStats{
				commits: make(map[string]bool),
				authors: make(map[string]bool),
			}
			stats[path] = fs
		}
		key := fmt.Sprintf("%d", commitID)
		fs.commits[key] = true
		fs.authors[currentAuthor] = true
		fs.added += added
		fs.deleted += deleted
		fs.lastAuthor = currentAuthor
	}

	// Batch-fetch last-modified timestamps for all files in one git call.
	now := time.Now()
	trackedPaths := make(map[string]bool, len(stats))
	for p := range stats {
		trackedPaths[p] = true
	}
	lastModified := batchLastModified(root, trackedPaths, now)

	var files []FileChurn
	for path, fs := range stats {
		commitCount := len(fs.commits)
		churn := float64(fs.added+fs.deleted) / float64(max(commitCount, 1))

		var authorNames []string
		for a := range fs.authors {
			authorNames = append(authorNames, a)
		}
		sort.Strings(authorNames)

		daysSince := -1
		if d, ok := lastModified[path]; ok {
			daysSince = d
		}

		files = append(files, FileChurn{
			Path:            path,
			Commits:         commitCount,
			Authors:         len(fs.authors),
			AuthorNames:     authorNames,
			LinesAdded:      fs.added,
			LinesDeleted:    fs.deleted,
			ChurnScore:      churn,
			DaysSinceChange: daysSince,
			LastAuthor:      fs.lastAuthor,
		})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].ChurnScore > files[j].ChurnScore
	})

	return files, nil
}

// analyzeCoupling finds files that frequently change in the same commit.
func analyzeCoupling(root, since string, files []FileChurn) ([]CoupledFile, error) {
	// Get files-per-commit.
	out, err := exec.Command("git", "-C", root, "log",
		"--since="+since,
		"--format=COMMIT",
		"--name-only",
	).Output()
	if err != nil {
		return nil, err
	}

	// Parse into per-commit file lists.
	var commits [][]string
	var current []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "COMMIT" {
			if len(current) > 1 {
				commits = append(commits, current)
			}
			current = nil
			continue
		}
		if line != "" {
			current = append(current, line)
		}
	}
	if len(current) > 1 {
		commits = append(commits, current)
	}

	// Build file commit counts for coupling ratio.
	fileCommits := make(map[string]int)
	for _, f := range files {
		fileCommits[f.Path] = f.Commits
	}

	// Count co-occurrences.
	type pair struct{ a, b string }
	coChange := make(map[pair]int)

	for _, commitFiles := range commits {
		// Only consider commits with ≤ 20 files (large commits are noisy).
		if len(commitFiles) > 20 {
			continue
		}
		for i := 0; i < len(commitFiles); i++ {
			for j := i + 1; j < len(commitFiles); j++ {
				a, b := commitFiles[i], commitFiles[j]
				if a > b {
					a, b = b, a
				}
				coChange[pair{a, b}]++
			}
		}
	}

	var result []CoupledFile
	for p, count := range coChange {
		if count < 2 {
			continue
		}
		minCommits := fileCommits[p.a]
		if c := fileCommits[p.b]; c < minCommits && c > 0 {
			minCommits = c
		}
		if minCommits == 0 {
			minCommits = 1
		}
		coupling := float64(count) / float64(minCommits)
		if coupling > 1.0 {
			coupling = 1.0
		}

		result = append(result, CoupledFile{
			FileA:     p.a,
			FileB:     p.b,
			CoChanges: count,
			Coupling:  coupling,
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Coupling > result[j].Coupling
	})

	// Keep top 50 coupling pairs.
	if len(result) > 50 {
		result = result[:50]
	}

	return result, nil
}

// batchLastModified fetches last-change timestamps for all files in a single
// git log call, returning a map of path → days since last change.
// This replaces N individual `git log -1 -- <file>` calls with one pass.
func batchLastModified(root string, trackedPaths map[string]bool, now time.Time) map[string]int {
	result := make(map[string]int, len(trackedPaths))

	// Single git log with commit timestamps + file names.
	out, err := exec.Command("git", "-C", root, "log",
		"--format=TS|%at",
		"--name-only",
	).Output()
	if err != nil {
		return result
	}

	// We only care about the FIRST (most recent) timestamp per file.
	var currentTS int64
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "TS|") {
			ts, err := strconv.ParseInt(strings.TrimPrefix(line, "TS|"), 10, 64)
			if err == nil {
				currentTS = ts
			}
			continue
		}
		// Only record files we actually care about, and only the first occurrence.
		if trackedPaths[line] {
			if _, already := result[line]; !already && currentTS > 0 {
				hours := int(now.Sub(time.Unix(currentTS, 0)).Hours())
				result[line] = (hours + 12) / 24
			}
		}
	}
	return result
}

func daysSinceLastChange(root, path string, now time.Time) int {
	out, err := exec.Command("git", "-C", root, "log", "-1", "--format=%at", "--", path).Output()
	if err != nil {
		return -1
	}
	ts, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return -1
	}
	hours := int(now.Sub(time.Unix(ts, 0)).Hours())
	return (hours + 12) / 24 // round to nearest day
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
