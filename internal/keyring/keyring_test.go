package keyring

import (
	"testing"
	"time"
)

func TestNewKeyRingNil(t *testing.T) {
	kr := NewKeyRing(nil)
	if kr != nil {
		t.Fatal("expected nil for empty keys")
	}
	if kr.Current() != "" {
		t.Fatal("expected empty string from nil keyring")
	}
	if kr.Count() != 0 {
		t.Fatal("expected 0 count from nil keyring")
	}
}

func TestSingleKey(t *testing.T) {
	kr := NewKeyRing([]string{"key1"})
	if kr.Current() != "key1" {
		t.Fatalf("expected key1, got %s", kr.Current())
	}
	if kr.Count() != 1 {
		t.Fatalf("expected 1 key, got %d", kr.Count())
	}
	if kr.Available() != 1 {
		t.Fatalf("expected 1 available, got %d", kr.Available())
	}
	// Can't rotate with single key.
	_, _, ok := kr.Rotate()
	if ok {
		t.Fatal("should not be able to rotate with single key")
	}
}

func TestRotation(t *testing.T) {
	kr := NewKeyRing([]string{"key1", "key2", "key3"})
	if kr.Current() != "key1" {
		t.Fatalf("expected key1, got %s", kr.Current())
	}

	// Mark key1 as rate limited and rotate.
	kr.MarkRateLimited(15 * time.Minute)
	newKey, idx, ok := kr.Rotate()
	if !ok {
		t.Fatal("expected successful rotation")
	}
	if newKey != "key2" {
		t.Fatalf("expected key2, got %s", newKey)
	}
	if idx != 2 {
		t.Fatalf("expected index 2, got %d", idx)
	}

	// Mark key2 as rate limited and rotate.
	kr.MarkRateLimited(15 * time.Minute)
	newKey, idx, ok = kr.Rotate()
	if !ok {
		t.Fatal("expected successful rotation")
	}
	if newKey != "key3" {
		t.Fatalf("expected key3, got %s", newKey)
	}
	if idx != 3 {
		t.Fatalf("expected index 3, got %d", idx)
	}

	// Mark key3 as rate limited — all exhausted.
	kr.MarkRateLimited(15 * time.Minute)
	_, _, ok = kr.Rotate()
	if ok {
		t.Fatal("expected rotation to fail when all keys exhausted")
	}
	if kr.Available() != 0 {
		t.Fatalf("expected 0 available, got %d", kr.Available())
	}
}

func TestStatus(t *testing.T) {
	kr := NewKeyRing([]string{"sk-abcd1234", "sk-efgh5678"})
	statuses := kr.Status()
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(statuses))
	}
	if !statuses[0].IsActive {
		t.Fatal("expected first key to be active")
	}
	if statuses[0].MaskedKey != "...1234" {
		t.Fatalf("expected ...1234, got %s", statuses[0].MaskedKey)
	}
	if !statuses[1].IsAvailable {
		t.Fatal("expected second key to be available")
	}
}

func TestMaskKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"ab", "ab"},
		{"abcd", "abcd"},
		{"abcde", "...bcde"},
		{"sk-very-long-key-1234", "...1234"},
	}
	for _, tt := range tests {
		got := maskKey(tt.in)
		if got != tt.want {
			t.Errorf("maskKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
