// Package hooks provides an event dispatcher that runs user-configured shell
// commands in response to lifecycle events (tool use, edits, commits, etc.).
//
// Configuration lives in .mantisrc.yml under the `hooks:` key:
//
//	hooks:
//	  pre_edit:
//	    - command: "./scripts/lint-before-edit.sh"
//	  post_commit:
//	    - command: "notify-send 'Mantis committed'"
//
// Each handler receives a JSON payload on stdin describing the event.
// Exit code 0 = success, exit code 2 = blocking error (stderr fed back to the
// model as an error message). Any other non-zero exit code is logged but not blocking.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Event identifies a lifecycle event.
type Event string

const (
	SessionStart    Event = "session_start"
	SessionEnd      Event = "session_end"
	PreToolUse      Event = "pre_tool_use"
	PostToolUse     Event = "post_tool_use"
	PreEdit         Event = "pre_edit"
	PostEdit        Event = "post_edit"
	PreBash         Event = "pre_bash"
	PostBash        Event = "post_bash"
	PreCommit       Event = "pre_commit"
	PostCommit      Event = "post_commit"
	UserPromptSubmit Event = "user_prompt_submit"
	Stop            Event = "stop"
)

// allEvents lists every known event for validation.
var allEvents = map[Event]bool{
	SessionStart: true, SessionEnd: true,
	PreToolUse: true, PostToolUse: true,
	PreEdit: true, PostEdit: true,
	PreBash: true, PostBash: true,
	PreCommit: true, PostCommit: true,
	UserPromptSubmit: true, Stop: true,
}

// Payload is the JSON data sent to hook commands on stdin.
type Payload struct {
	Event    Event       `json:"event"`
	ToolName string      `json:"tool_name,omitempty"`
	Args     interface{} `json:"args,omitempty"`
	Output   string      `json:"output,omitempty"`
	Message  string      `json:"message,omitempty"`
}

// HookResult is the outcome of running a hook.
type HookResult struct {
	Blocked bool   // true if exit code == 2
	Error   string // stderr content (for blocked hooks)
}

// Handler is a single hook handler definition from config.
type Handler struct {
	Command string `yaml:"command"`
}

// HookConfig maps event names to handler lists. Parsed from .mantisrc.yml.
type HookConfig map[Event][]Handler

// Dispatcher manages event hooks and fires them.
type Dispatcher struct {
	hooks   HookConfig
	root    string // project root for command execution
	timeout time.Duration
}

// NewDispatcher creates a hook dispatcher from config.
func NewDispatcher(hooks HookConfig, root string) *Dispatcher {
	return &Dispatcher{
		hooks:   hooks,
		root:    root,
		timeout: 30 * time.Second,
	}
}

// Fire runs all handlers for the given event. Returns a blocking result if any
// handler exits with code 2. Non-blocking failures are silently ignored.
func (d *Dispatcher) Fire(ctx context.Context, event Event, payload Payload) HookResult {
	if d == nil || d.hooks == nil {
		return HookResult{}
	}
	handlers, ok := d.hooks[event]
	if !ok || len(handlers) == 0 {
		return HookResult{}
	}

	payload.Event = event
	data, _ := json.Marshal(payload)

	for _, h := range handlers {
		result := d.runHandler(ctx, h, data)
		if result.Blocked {
			return result
		}
	}
	return HookResult{}
}

// HasHooks returns true if there are any handlers for the given event.
func (d *Dispatcher) HasHooks(event Event) bool {
	if d == nil || d.hooks == nil {
		return false
	}
	return len(d.hooks[event]) > 0
}

// runHandler executes a single hook command.
func (d *Dispatcher) runHandler(ctx context.Context, h Handler, stdinData []byte) HookResult {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-c", h.Command)
	cmd.Dir = d.root
	cmd.Stdin = bytes.NewReader(stdinData)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if exitErr.ExitCode() == 2 {
				errMsg := strings.TrimSpace(stderr.String())
				if errMsg == "" {
					errMsg = fmt.Sprintf("hook %q blocked the action (exit 2)", h.Command)
				}
				return HookResult{Blocked: true, Error: errMsg}
			}
		}
		// Non-blocking error — log but continue.
	}
	return HookResult{}
}

// ParseHookConfig extracts hook configuration from a raw YAML map.
// Expected format: hooks: { event_name: [{command: "..."}] }
func ParseHookConfig(raw map[string]interface{}) HookConfig {
	config := make(HookConfig)
	hooksRaw, ok := raw["hooks"]
	if !ok {
		return config
	}
	hooksMap, ok := hooksRaw.(map[string]interface{})
	if !ok {
		return config
	}

	for eventName, handlersRaw := range hooksMap {
		event := Event(eventName)
		if !allEvents[event] {
			continue // skip unknown events
		}
		handlersList, ok := handlersRaw.([]interface{})
		if !ok {
			continue
		}
		for _, hRaw := range handlersList {
			hMap, ok := hRaw.(map[string]interface{})
			if !ok {
				continue
			}
			if cmdRaw, ok := hMap["command"]; ok {
				if cmdStr, ok := cmdRaw.(string); ok {
					config[event] = append(config[event], Handler{Command: cmdStr})
				}
			}
		}
	}
	return config
}
