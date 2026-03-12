package repl

// EventLog records a chronological stream of agent events to .mantis/EVENTS.jsonl.
// Each event is a newline-delimited JSON object (one per line), inspired by the
// OpenHands event-sourced state model.
//
// Event types:
//   "user_message"   — raw user input
//   "model_response" — full model output
//   "file_write"     — file written to disk (path + byte count)
//   "pipeline_run"   — pipeline stage summary (plan/code/test)
//   "slash_command"  — slash command invoked

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AgentEvent is one entry in the event stream.
type AgentEvent struct {
	Timestamp time.Time       `json:"ts"`
	SessionID string          `json:"sid"`
	Turn      int             `json:"turn"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
}

// EventLog writes AgentEvent records to EVENTS.jsonl (append-only, newline-delimited).
// Thread-safe: all writes are serialized through a mutex.
type EventLog struct {
	mu        sync.Mutex
	path      string
	sessionID string
}

// NewEventLog opens (or creates) the event log file in mantisDir.
func NewEventLog(mantisDir, sessionID string) *EventLog {
	return &EventLog{
		path:      filepath.Join(mantisDir, "EVENTS.jsonl"),
		sessionID: sessionID,
	}
}

// Record appends a typed event with arbitrary payload to the log.
// payload must be JSON-serialisable; errors are silently ignored (best-effort).
func (el *EventLog) Record(turn int, eventType string, payload interface{}) {
	if el == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ev := AgentEvent{
		Timestamp: time.Now(),
		SessionID: el.sessionID,
		Turn:      turn,
		Type:      eventType,
		Data:      json.RawMessage(raw),
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}

	el.mu.Lock()
	defer el.mu.Unlock()

	f, err := os.OpenFile(el.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s\n", line)
}

// RecordUserMessage logs a user_message event.
func (el *EventLog) RecordUserMessage(turn int, text string) {
	el.Record(turn, "user_message", map[string]string{"text": text})
}

// RecordModelResponse logs a model_response event.
func (el *EventLog) RecordModelResponse(turn int, model, tier, text string) {
	el.Record(turn, "model_response", map[string]string{
		"model": model, "tier": tier, "text": truncateEvent(text, 2000),
	})
}

// RecordFileWrite logs a file_write event.
func (el *EventLog) RecordFileWrite(turn int, path string, byteCount int) {
	el.Record(turn, "file_write", map[string]interface{}{
		"path": path, "bytes": byteCount,
	})
}

// RecordSlashCommand logs a slash_command event.
func (el *EventLog) RecordSlashCommand(turn int, cmd string) {
	el.Record(turn, "slash_command", map[string]string{"cmd": cmd})
}

// RecordCompaction logs a context compaction event.
func (el *EventLog) RecordCompaction(turn, turnsCompressed, tokensBefore, tokensAfter int) {
	el.Record(turn, "compaction", map[string]int{
		"turns_compressed": turnsCompressed,
		"tokens_before":    tokensBefore,
		"tokens_after":     tokensAfter,
	})
}

// ── Replay ────────────────────────────────────────────────────────────────────

// ReplaySession reads EVENTS.jsonl and prints all events matching sessionID
// (or all events if sessionID is empty) as a human-readable narrative.
func ReplaySession(mantisDir, sessionID string) {
	path := filepath.Join(mantisDir, "EVENTS.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("  no event log found at %s\n", path)
		return
	}

	lines := splitLines(string(data))
	if len(lines) == 0 {
		fmt.Println("  event log is empty")
		return
	}

	shown := 0
	for _, line := range lines {
		if line == "" {
			continue
		}
		var ev AgentEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if sessionID != "" && ev.SessionID != sessionID {
			continue
		}

		ts := ev.Timestamp.Format("15:04:05")
		switch ev.Type {
		case "user_message":
			var d map[string]string
			_ = json.Unmarshal(ev.Data, &d)
			fmt.Printf("\033[38;5;214m[%s turn%d] user:\033[0m %s\n", ts, ev.Turn, truncateEvent(d["text"], 120))
		case "model_response":
			var d map[string]string
			_ = json.Unmarshal(ev.Data, &d)
			fmt.Printf("\033[38;5;244m[%s turn%d] %s (%s):\033[0m %s\n",
				ts, ev.Turn, d["model"], d["tier"], truncateEvent(d["text"], 120))
		case "file_write":
			var d map[string]interface{}
			_ = json.Unmarshal(ev.Data, &d)
			fmt.Printf("\033[38;5;43m[%s turn%d] wrote:\033[0m %v (%v bytes)\n",
				ts, ev.Turn, d["path"], d["bytes"])
		case "slash_command":
			var d map[string]string
			_ = json.Unmarshal(ev.Data, &d)
			fmt.Printf("\033[38;5;244m[%s turn%d] cmd:\033[0m %s\n", ts, ev.Turn, d["cmd"])
		case "pipeline_run":
			var d map[string]string
			_ = json.Unmarshal(ev.Data, &d)
			fmt.Printf("\033[38;5;220m[%s turn%d] pipeline:\033[0m %s\n", ts, ev.Turn, d["summary"])
		case "compaction":
			var d map[string]int
			_ = json.Unmarshal(ev.Data, &d)
			fmt.Printf("\033[38;5;141m[%s turn%d] compacted:\033[0m %d turns (%dK → %dK tokens)\n",
				ts, ev.Turn, d["turns_compressed"], d["tokens_before"]/1000, d["tokens_after"]/1000)
		}
		shown++
	}
	if shown == 0 {
		fmt.Printf("  no events found for session %q\n", sessionID)
	}
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func truncateEvent(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
