package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestUpsertGetAndPersist(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "docfetch.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	rec := &Record{
		EntityID: "e1",
		Name:     "Widget",
		MetaHash: MetaHash("Acme", "W-1", "Widget"),
		Status:   StatusNew,
	}
	if err := s.Upsert(ctx, rec); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, err := s.Get(ctx, "e1")
	if err != nil || got == nil {
		t.Fatalf("get: %v got=%v", err, got)
	}
	if got.MetaHash != rec.MetaHash || got.Status != StatusNew {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}
	if got.FirstSeen.IsZero() {
		t.Fatal("first_seen should be set")
	}

	// Update status; first_seen must be preserved.
	firstSeen := got.FirstSeen
	got.Status = StatusAttached
	got.DocSHA256 = "abc"
	if err := s.Upsert(ctx, got); err != nil {
		t.Fatalf("upsert2: %v", err)
	}
	s.Close()

	// Reopen: state must survive restart.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got2, err := s2.Get(ctx, "e1")
	if err != nil || got2 == nil {
		t.Fatalf("get after reopen: %v", err)
	}
	if got2.Status != StatusAttached || got2.DocSHA256 != "abc" {
		t.Fatalf("update not persisted: %+v", got2)
	}
	if !got2.FirstSeen.Equal(firstSeen) {
		t.Fatalf("first_seen changed: %v != %v", got2.FirstSeen, firstSeen)
	}

	miss, err := s2.Get(ctx, "nope")
	if err != nil || miss != nil {
		t.Fatalf("expected nil for missing, got %v (%v)", miss, err)
	}

	byStatus, err := s2.ListByStatus(ctx, StatusAttached)
	if err != nil || len(byStatus) != 1 {
		t.Fatalf("list by status: %v n=%d", err, len(byStatus))
	}
}
