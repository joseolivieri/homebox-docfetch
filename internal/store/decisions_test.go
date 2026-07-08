package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestDecisionLedger(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	d := &Decision{EntityID: "e1", EntityName: "Anker 737", Stage: "brand-site",
		ChosenURL: "https://a.com/m.pdf", Confidence: 0.9, UsedLLM: true,
		Candidates: `[{"u":"https://a.com/m.pdf","s":9}]`, Outcome: "review"}
	if err := s.RecordDecision(ctx, d); err != nil {
		t.Fatal(err)
	}

	// Label it rejected; URL must show up in the rejected set.
	n, err := s.LabelDecisions(ctx, "e1", "https://a.com/m.pdf", LabelRejected, "ntfy")
	if err != nil || n != 1 {
		t.Fatalf("label: n=%d err=%v", n, err)
	}
	rej, err := s.RejectedURLs(ctx, "e1", "manual")
	if err != nil || !rej["https://a.com/m.pdf"] {
		t.Fatalf("rejected set = %v, err=%v", rej, err)
	}

	// Second label pass is a no-op (only unlabeled rows are touched).
	if n, _ := s.LabelDecisions(ctx, "e1", "https://a.com/m.pdf", LabelConfirmed, "age"); n != 0 {
		t.Fatalf("relabel touched %d rows", n)
	}

	latest, err := s.LatestDecision(ctx, "e1", "manual")
	if err != nil || latest == nil || latest.Label != LabelRejected || !latest.UsedLLM {
		t.Fatalf("latest = %+v, err=%v", latest, err)
	}

	out, lab, err := s.DecisionStats(ctx, time.Now().Add(-time.Hour))
	if err != nil || out["review"] != 1 || lab[LabelRejected] != 1 {
		t.Fatalf("stats out=%v lab=%v err=%v", out, lab, err)
	}
}
