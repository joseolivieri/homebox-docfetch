package discovery

import "testing"

func TestRenderTemplates(t *testing.T) {
	e := NewEngine(Options{}, nil)
	tmpls := []string{"{manufacturer} {modelNumber} user manual filetype:pdf", "{name} datasheet"}
	got := e.renderTemplates(Item{Manufacturer: "Acme", ModelNumber: "W-1", Name: "Widget"}, tmpls)
	if got[0] != "Acme W-1 user manual filetype:pdf" || got[1] != "Widget datasheet" {
		t.Fatalf("bad render: %#v", got)
	}
}

func TestClassOf(t *testing.T) {
	e := NewEngine(Options{Classes: []DocClass{
		{Name: "manual", Keywords: []string{"manual", "guide"}},
		{Name: "parts", Keywords: []string{"parts", "exploded"}},
	}}, nil)
	parts := &Candidate{URL: "https://x.com/dishwasher-parts-list.pdf", Title: "Parts List"}
	if got := e.classOf(parts, "manual"); got != "parts" {
		t.Fatalf("expected parts, got %q", got)
	}
	man := &Candidate{URL: "https://x.com/owner-manual.pdf", Title: "User Guide"}
	if got := e.classOf(man, "manual"); got != "manual" {
		t.Fatalf("expected manual, got %q", got)
	}
}

func TestClearWinner(t *testing.T) {
	// exactly one model-matching PDF -> clear winner
	cs := []Candidate{
		{URL: "a", IsPDF: true, ModelMatch: false},
		{URL: "b", IsPDF: true, ModelMatch: true},
		{URL: "c", IsPDF: false, ModelMatch: true},
	}
	w, ok := cs[1], false
	if got, gok := clearWinner(cs); gok {
		w, ok = *got, true
	}
	if !ok || w.URL != "b" {
		t.Fatalf("expected winner b, got %v ok=%v", w.URL, ok)
	}

	// two model-matching PDFs -> ambiguous, no clear winner
	cs2 := []Candidate{{IsPDF: true, ModelMatch: true}, {IsPDF: true, ModelMatch: true}}
	if _, ok := clearWinner(cs2); ok {
		t.Fatal("expected ambiguity")
	}
}

func TestModelGate(t *testing.T) {
	e := NewEngine(Options{RequireModel: true}, nil)
	res := &Result{Best: &Candidate{ModelMatch: false}, Confidence: 0.9}
	if e.applyModelGate(res, true).Confidence != 0 {
		t.Fatal("expected confidence zeroed when model required but absent")
	}
	res2 := &Result{Best: &Candidate{ModelMatch: true}, Confidence: 0.9}
	if e.applyModelGate(res2, true).Confidence != 0.9 {
		t.Fatal("expected confidence preserved on model match")
	}
	// name-only item (requireModel=false) keeps confidence despite no model match
	res3 := &Result{Best: &Candidate{ModelMatch: false}, Confidence: 0.9}
	if e.applyModelGate(res3, false).Confidence != 0.9 {
		t.Fatal("expected confidence preserved when model not required")
	}
}

func TestNorm(t *testing.T) {
	if norm("W-1 000!") != "w1000" {
		t.Fatalf("norm mismatch: %q", norm("W-1 000!"))
	}
}
