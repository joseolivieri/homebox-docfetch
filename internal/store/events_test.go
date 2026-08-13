package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestEventSignalDedupe(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.AppendEvent(ctx, &Event{
			EntityID: "e1", Kind: EvDocReject, URL: "https://x/doc.pdf", Actor: ActorUser,
		}); err != nil {
			t.Fatal(err)
		}
	}
	urls, err := s.EventURLs(ctx, "e1", EvDocReject)
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 1 || urls[0] != "https://x/doc.pdf" {
		t.Fatalf("want single deduped url, got %v", urls)
	}
	evs, _ := s.Events(ctx, "e1", 0)
	if len(evs) != 1 {
		t.Fatalf("signal dedupe failed: %d rows", len(evs))
	}
}

func TestEventAuditNotDeduped(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := s.AppendEvent(ctx, &Event{EntityID: "e1", Kind: EvDocAttach, URL: "https://x/doc.pdf"}); err != nil {
			t.Fatal(err)
		}
	}
	evs, _ := s.Events(ctx, "e1", 0)
	if len(evs) != 2 {
		t.Fatalf("audit events must append, got %d rows", len(evs))
	}
}

func TestPruneKeepsSignals(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	old := time.Now().Add(-48 * time.Hour)
	_ = s.AppendEvent(ctx, &Event{EntityID: "e1", Kind: EvDocAttach, URL: "https://x/a.pdf", Ts: old})
	_ = s.AppendEvent(ctx, &Event{EntityID: "e1", Kind: EvDocReject, URL: "https://x/b.pdf", Ts: old})
	n, err := s.PruneEvents(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 pruned, got %d", n)
	}
	urls, _ := s.EventURLs(ctx, "e1", EvDocReject)
	if len(urls) != 1 {
		t.Fatal("signal event must survive pruning")
	}
}

func TestMetaRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if v, err := s.GetMeta(ctx, "change_signal"); err != nil || v != "" {
		t.Fatalf("empty get: v=%q err=%v", v, err)
	}
	if err := s.SetMeta(ctx, "change_signal", "42|t1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMeta(ctx, "change_signal", "43|t2"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.GetMeta(ctx, "change_signal"); v != "43|t2" {
		t.Fatalf("want overwrite, got %q", v)
	}
}

func TestEventsFilterAndOrder(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	_ = s.AppendEvent(ctx, &Event{EntityID: "e1", Kind: EvIntakeCreated})
	_ = s.AppendEvent(ctx, &Event{EntityID: "e2", Kind: EvIntakeCreated})
	_ = s.AppendEvent(ctx, &Event{EntityID: "e1", Kind: EvDocAttach})
	all, _ := s.Events(ctx, "", 0)
	if len(all) != 3 || all[0].Kind != EvDocAttach {
		t.Fatalf("want 3 newest-first, got %d first=%q", len(all), all[0].Kind)
	}
	e1, _ := s.Events(ctx, "e1", 0)
	if len(e1) != 2 {
		t.Fatalf("entity filter: want 2, got %d", len(e1))
	}
}

func TestFactsRoundTripAndSupersede(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	if err := s.PutFact(ctx, &Fact{EntityID: "e1", Kind: FactGTIN, Value: "09520123456788",
		Confidence: 1, Source: SourceBarcode}); err != nil {
		t.Fatal(err)
	}
	// Same fact observed again must not duplicate.
	if err := s.PutFact(ctx, &Fact{EntityID: "e1", Kind: FactGTIN, Value: "09520123456788",
		Confidence: 1, Source: SourceBarcode}); err != nil {
		t.Fatal(err)
	}
	fs, _ := s.Facts(ctx, "e1", FactGTIN)
	if len(fs) != 1 {
		t.Fatalf("facts must be idempotent, got %d rows", len(fs))
	}
	if v, _ := s.FactValue(ctx, "e1", FactGTIN); v != "09520123456788" {
		t.Fatalf("FactValue = %q", v)
	}

	// Highest confidence wins the single-value read.
	_ = s.PutFact(ctx, &Fact{EntityID: "e1", Kind: FactProductType, Value: "guess", Confidence: 0.4, Source: SourceVision})
	_ = s.PutFact(ctx, &Fact{EntityID: "e1", Kind: FactProductType, Value: "hose timer", Confidence: 1, Source: SourceUser})
	if v, _ := s.FactValue(ctx, "e1", FactProductType); v != "hose timer" {
		t.Fatalf("highest-confidence value should win, got %q", v)
	}

	// A correction supersedes and is never re-read.
	if err := s.SupersedeFacts(ctx, "e1", FactProductType); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.FactValue(ctx, "e1", FactProductType); v != "" {
		t.Fatalf("superseded facts must not be returned, got %q", v)
	}
	if v, _ := s.FactValue(ctx, "e1", FactGTIN); v == "" {
		t.Fatal("superseding one kind must not affect another")
	}
	if v, _ := s.FactValue(ctx, "nobody", FactGTIN); v != "" {
		t.Fatalf("missing fact should read empty, got %q", v)
	}
}
