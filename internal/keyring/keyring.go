// Package keyring manages multiple API keys with automatic rotation and cooldown.
package keyring

import (
	"fmt"
	"sync"
	"time"
)

// KeyRing holds a set of API keys and tracks which ones are rate-limited.
type KeyRing struct {
	mu     sync.Mutex
	keys   []keyEntry
	active int // index of current key
}

type keyEntry struct {
	Key       string
	Label     string    // optional user label (e.g. "personal", "work")
	CoolUntil time.Time // rate-limited until this time
}

// NewKeyRing creates a KeyRing from a list of API keys.
// Returns nil if no keys are provided.
func NewKeyRing(keys []string) *KeyRing {
	if len(keys) == 0 {
		return nil
	}
	entries := make([]keyEntry, len(keys))
	for i, k := range keys {
		entries[i] = keyEntry{
			Key:   k,
			Label: fmt.Sprintf("key #%d", i+1),
		}
	}
	return &KeyRing{keys: entries, active: 0}
}

// NewKeyRingLabeled creates a KeyRing from keys and their corresponding labels.
// If labels is shorter than keys, auto-generates labels for the extras.
// Returns nil if no keys are provided.
func NewKeyRingLabeled(keys []string, labels []string) *KeyRing {
	if len(keys) == 0 {
		return nil
	}
	entries := make([]keyEntry, len(keys))
	for i, k := range keys {
		lbl := fmt.Sprintf("key #%d", i+1)
		if i < len(labels) && labels[i] != "" {
			lbl = labels[i]
		}
		entries[i] = keyEntry{Key: k, Label: lbl}
	}
	return &KeyRing{keys: entries, active: 0}
}

// Current returns the current active key.
func (kr *KeyRing) Current() string {
	if kr == nil || len(kr.keys) == 0 {
		return ""
	}
	kr.mu.Lock()
	defer kr.mu.Unlock()
	return kr.keys[kr.active].Key
}

// ActiveIndex returns the 1-based index of the current active key.
func (kr *KeyRing) ActiveIndex() int {
	if kr == nil {
		return 0
	}
	kr.mu.Lock()
	defer kr.mu.Unlock()
	return kr.active + 1
}

// MarkRateLimited marks the current key as cooling down for the given duration
// and rotates to the next available key.
func (kr *KeyRing) MarkRateLimited(duration time.Duration) {
	if kr == nil {
		return
	}
	kr.mu.Lock()
	defer kr.mu.Unlock()
	kr.keys[kr.active].CoolUntil = time.Now().Add(duration)
}

// Rotate finds the next available key (skipping cooled-down ones).
// Returns the key string, the 1-based index, and true if a key was found.
// Returns "", 0, false if all keys are exhausted.
func (kr *KeyRing) Rotate() (string, int, bool) {
	if kr == nil || len(kr.keys) <= 1 {
		return "", 0, false
	}
	kr.mu.Lock()
	defer kr.mu.Unlock()

	now := time.Now()
	for i := 1; i < len(kr.keys); i++ {
		idx := (kr.active + i) % len(kr.keys)
		if now.After(kr.keys[idx].CoolUntil) {
			kr.active = idx
			return kr.keys[idx].Key, idx + 1, true
		}
	}
	return "", 0, false
}

// Count returns the total number of keys.
func (kr *KeyRing) Count() int {
	if kr == nil {
		return 0
	}
	return len(kr.keys)
}

// Available returns the number of keys not currently cooling down.
func (kr *KeyRing) Available() int {
	if kr == nil {
		return 0
	}
	kr.mu.Lock()
	defer kr.mu.Unlock()

	now := time.Now()
	count := 0
	for _, k := range kr.keys {
		if now.After(k.CoolUntil) {
			count++
		}
	}
	return count
}

// KeyStatus holds display info about a single key.
type KeyStatus struct {
	Index       int
	MaskedKey   string
	Label       string
	IsActive    bool
	IsAvailable bool
	CoolRemain  time.Duration
}

// Status returns display information about all keys.
func (kr *KeyRing) Status() []KeyStatus {
	if kr == nil {
		return nil
	}
	kr.mu.Lock()
	defer kr.mu.Unlock()

	now := time.Now()
	out := make([]KeyStatus, len(kr.keys))
	for i, k := range kr.keys {
		masked := maskKey(k.Key)
		avail := now.After(k.CoolUntil)
		var remain time.Duration
		if !avail {
			remain = k.CoolUntil.Sub(now)
		}
		out[i] = KeyStatus{
			Index:       i + 1,
			MaskedKey:   masked,
			Label:       k.Label,
			IsActive:    i == kr.active,
			IsAvailable: avail,
			CoolRemain:  remain,
		}
	}
	return out
}

// maskKey shows the last 4 characters of a key, prefixed with "...".
func maskKey(key string) string {
	if len(key) <= 4 {
		return key
	}
	return "..." + key[len(key)-4:]
}
