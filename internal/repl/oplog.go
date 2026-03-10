package repl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileOperation records a single file write performed by Mantis during a session.
type FileOperation struct {
	Timestamp   time.Time `json:"timestamp"`
	BatchID     int       `json:"batchId"`  // groups all files from one turn/response
	AbsPath     string    `json:"path"`     // absolute path for reliable restoration
	Op          string    `json:"op"`       // "create" | "write"
	PrevContent string    `json:"prev"`     // empty for "create"
}

// OperationLog is a session-scoped, JSON-persisted log of every file write.
// Enables /undo to atomically restore the previous state of the last batch.
//
// Thread-safe: all public methods are guarded by a mutex.
type OperationLog struct {
	mu        sync.Mutex
	logPath   string
	ops       []FileOperation
	nextBatch int // auto-incremented per writePendingChanges call
}

// NewOperationLog creates a log backed by .mantis/OPERATION_LOG.json.
// Loads any existing log so the undo history survives a binary restart.
func NewOperationLog(mantisDir string) *OperationLog {
	l := &OperationLog{logPath: filepath.Join(mantisDir, "OPERATION_LOG.json")}
	if data, err := os.ReadFile(l.logPath); err == nil {
		_ = json.Unmarshal(data, &l.ops)
		// Resume batch counter from the highest recorded batch ID.
		for _, op := range l.ops {
			if op.BatchID >= l.nextBatch {
				l.nextBatch = op.BatchID + 1
			}
		}
	}
	return l
}

// NextBatch returns a unique batch ID for the current write call.
// All files written in one extractAndWriteFiles* call share the same batch ID.
func (l *OperationLog) NextBatch() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	id := l.nextBatch
	l.nextBatch++
	return id
}

// Record appends a file operation and flushes to disk.
func (l *OperationLog) Record(batchID int, absPath, op, prevContent string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, FileOperation{
		Timestamp:   time.Now(),
		BatchID:     batchID,
		AbsPath:     absPath,
		Op:          op,
		PrevContent: prevContent,
	})
	l.flush()
}

// UndoLastBatch reverts all file operations from the most recent batch.
// New files are deleted; modified files are restored to their previous content.
// Returns a human-readable summary line per restored file.
func (l *OperationLog) UndoLastBatch() ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.ops) == 0 {
		return nil, fmt.Errorf("nothing to undo")
	}

	// Find the most recent batch ID.
	maxBatch := l.ops[len(l.ops)-1].BatchID

	// Partition: ops to undo vs ops to keep.
	var toUndo []FileOperation
	var remaining []FileOperation
	for _, op := range l.ops {
		if op.BatchID == maxBatch {
			toUndo = append(toUndo, op)
		} else {
			remaining = append(remaining, op)
		}
	}

	// Undo in reverse order (last write first) for correct multi-edit restoration.
	var summary []string
	for i := len(toUndo) - 1; i >= 0; i-- {
		op := toUndo[i]
		rel := op.AbsPath // show shorter path if possible
		if wd, err := os.Getwd(); err == nil {
			if r, err := filepath.Rel(wd, op.AbsPath); err == nil {
				rel = r
			}
		}
		var err error
		if op.Op == "create" {
			err = os.Remove(op.AbsPath)
			if err == nil {
				summary = append(summary, fmt.Sprintf("deleted    %s", rel))
			}
		} else {
			err = os.WriteFile(op.AbsPath, []byte(op.PrevContent), 0o644)
			if err == nil {
				summary = append(summary, fmt.Sprintf("restored   %s", rel))
			}
		}
		if err != nil {
			summary = append(summary, fmt.Sprintf("⚠ %s: %v", rel, err))
		}
	}

	l.ops = remaining
	l.flush()
	return summary, nil
}

// Len returns the total number of recorded operations.
func (l *OperationLog) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.ops)
}

// MaxBatchID returns the most recently recorded batch ID, or -1 if empty.
func (l *OperationLog) MaxBatchID() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.ops) == 0 {
		return -1
	}
	return l.ops[len(l.ops)-1].BatchID
}

func (l *OperationLog) flush() {
	data, err := json.MarshalIndent(l.ops, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(l.logPath), 0o755)
	_ = os.WriteFile(l.logPath, data, 0o644)
}
