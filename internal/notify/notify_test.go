package notify

import (
	"testing"
	"time"
)

func TestNotifier_EnableDisable(t *testing.T) {
	n := New()
	if !n.IsEnabled() {
		t.Error("should be enabled by default")
	}
	n.SetEnabled(false)
	if n.IsEnabled() {
		t.Error("should be disabled after SetEnabled(false)")
	}
	n.SetEnabled(true)
	if !n.IsEnabled() {
		t.Error("should be enabled after SetEnabled(true)")
	}
}

func TestNotifier_NotifyIfSlow_BelowThreshold(t *testing.T) {
	n := New()
	n.SetMinDelay(1 * time.Minute)
	// Should not panic or send notification for fast tasks.
	n.NotifyIfSlow("test", "body", 5*time.Second)
}

func TestNotifier_Disabled(t *testing.T) {
	n := New()
	n.SetEnabled(false)
	// Should not panic when disabled.
	n.Notify("test", "body")
}

func TestBell(t *testing.T) {
	// Just verify it doesn't panic.
	Bell()
}
