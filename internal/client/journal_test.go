package client

import "testing"

// TestSeedCursor is CANT-165's seam: a fresh journal accepts a cursor its
// caller chose, and refuses once anything is already in the way of that
// choice meaning what it says.
func TestSeedCursor(t *testing.T) {
	t.Run("a fresh journal accepts it", func(t *testing.T) {
		j := NewJournal()
		if err := j.SeedCursor(42); err != nil {
			t.Fatalf("SeedCursor: %v", err)
		}
		s := j.Snapshot()
		if !s.HasCursor || s.Cursor != 42 {
			t.Errorf("snapshot = %+v, want cursor 42", s)
		}
	})

	t.Run("refuses a negative cursor", func(t *testing.T) {
		j := NewJournal()
		if err := j.SeedCursor(-1); err == nil {
			t.Fatal("SeedCursor(-1) succeeded, want a refusal")
		}
		if s := j.Snapshot(); s.HasCursor {
			t.Errorf("a refused SeedCursor left a cursor set: %+v", s)
		}
	})

	t.Run("refuses a second call", func(t *testing.T) {
		j := NewJournal()
		if err := j.SeedCursor(10); err != nil {
			t.Fatal(err)
		}
		if err := j.SeedCursor(20); err == nil {
			t.Fatal("a second SeedCursor succeeded, want a refusal")
		}
		if s := j.Snapshot(); s.Cursor != 10 {
			t.Errorf("cursor = %d, want the first call's 10 to stand", s.Cursor)
		}
	})

	t.Run("refuses once a wipe has happened", func(t *testing.T) {
		j := NewJournal()
		j.mu.Lock()
		j.resetLocked()
		j.wipes++
		j.mu.Unlock()
		if err := j.SeedCursor(10); err == nil {
			t.Fatal("SeedCursor after a wipe succeeded, want a refusal")
		}
	})
}
