package hooks

import (
	"context"
	"testing"
)

func TestFire_NoHooks(t *testing.T) {
	d := NewDispatcher(nil, ".")
	result := d.Fire(context.Background(), PreEdit, Payload{})
	if result.Blocked {
		t.Error("expected not blocked with nil config")
	}
}

func TestFire_Success(t *testing.T) {
	config := HookConfig{
		PreEdit: {{Command: "true"}}, // exit 0
	}
	d := NewDispatcher(config, ".")
	result := d.Fire(context.Background(), PreEdit, Payload{ToolName: "edit_file"})
	if result.Blocked {
		t.Error("expected not blocked for exit 0")
	}
}

func TestFire_BlockedExitCode2(t *testing.T) {
	config := HookConfig{
		PreEdit: {{Command: "echo 'forbidden edit' >&2; exit 2"}},
	}
	d := NewDispatcher(config, ".")
	result := d.Fire(context.Background(), PreEdit, Payload{ToolName: "edit_file"})
	if !result.Blocked {
		t.Error("expected blocked for exit 2")
	}
	if result.Error != "forbidden edit" {
		t.Errorf("unexpected error: %q", result.Error)
	}
}

func TestFire_NonBlockingFailure(t *testing.T) {
	config := HookConfig{
		PostEdit: {{Command: "exit 1"}}, // non-blocking
	}
	d := NewDispatcher(config, ".")
	result := d.Fire(context.Background(), PostEdit, Payload{})
	if result.Blocked {
		t.Error("exit 1 should not block")
	}
}

func TestHasHooks(t *testing.T) {
	d := NewDispatcher(HookConfig{PreBash: {{Command: "echo"}}}, ".")
	if !d.HasHooks(PreBash) {
		t.Error("expected true for PreBash")
	}
	if d.HasHooks(PostCommit) {
		t.Error("expected false for PostCommit")
	}
}

func TestParseHookConfig(t *testing.T) {
	raw := map[string]interface{}{
		"hooks": map[string]interface{}{
			"pre_edit": []interface{}{
				map[string]interface{}{"command": "./lint.sh"},
			},
			"unknown_event": []interface{}{
				map[string]interface{}{"command": "bad"},
			},
		},
	}
	config := ParseHookConfig(raw)
	if len(config[PreEdit]) != 1 {
		t.Errorf("expected 1 pre_edit handler, got %d", len(config[PreEdit]))
	}
	if config[PreEdit][0].Command != "./lint.sh" {
		t.Errorf("unexpected command: %s", config[PreEdit][0].Command)
	}
	// Unknown events should be skipped.
	if len(config) != 1 {
		t.Errorf("expected 1 event in config, got %d", len(config))
	}
}

func TestFire_MultipleHandlers(t *testing.T) {
	config := HookConfig{
		PreEdit: {
			{Command: "true"},                              // passes
			{Command: "echo 'blocked' >&2; exit 2"},       // blocks
		},
	}
	d := NewDispatcher(config, ".")
	result := d.Fire(context.Background(), PreEdit, Payload{})
	if !result.Blocked {
		t.Error("second handler should block")
	}
}

func TestNilDispatcher(t *testing.T) {
	var d *Dispatcher
	result := d.Fire(context.Background(), PreEdit, Payload{})
	if result.Blocked {
		t.Error("nil dispatcher should not block")
	}
	if d.HasHooks(PreEdit) {
		t.Error("nil dispatcher should have no hooks")
	}
}
