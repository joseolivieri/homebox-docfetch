package notes

import (
	"strings"
	"testing"
)

func TestAppendCreatesBlock(t *testing.T) {
	out := Append("", Line("created via photo intake"))
	if !strings.Contains(out, Begin) || !strings.Contains(out, End) || !strings.Contains(out, "#### docfetch") {
		t.Fatalf("block not created: %q", out)
	}
}

func TestAppendPreservesUserTextAboveAndBelow(t *testing.T) {
	existing := "user note above\n\n" + Begin + "\n#### docfetch\n- 2026-01-01 first\n" + End + "\nuser note below"
	out := Append(existing, "- 2026-01-02 second")
	if !strings.HasPrefix(out, "user note above") {
		t.Fatalf("lost leading user text: %q", out)
	}
	if !strings.HasSuffix(out, "user note below") {
		t.Fatalf("lost trailing user text: %q", out)
	}
	fi := strings.Index(out, "first")
	si := strings.Index(out, "second")
	ei := strings.Index(out, End)
	if fi < 0 || si < 0 || si < fi || si > ei {
		t.Fatalf("second line not appended inside block after first: %q", out)
	}
	if strings.Count(out, Begin) != 1 || strings.Count(out, End) != 1 {
		t.Fatalf("duplicate markers: %q", out)
	}
}

func TestRejectedURLs(t *testing.T) {
	body := Append("", Line("manual attached (0.90) "+MDLink("pdf", "https://ok.com/a.pdf")),
		Line("rejected "+MDLink("link", "https://bad.com/wrong.pdf")),
		Line("rejected https://bare.com/x.pdf"))
	got := RejectedURLs(body)
	want := map[string]bool{"https://bad.com/wrong.pdf": true, "https://bare.com/x.pdf": true}
	if len(got) != 2 {
		t.Fatalf("got %v", got)
	}
	for _, u := range got {
		if !want[u] {
			t.Fatalf("unexpected url %q in %v", u, got)
		}
	}
	// The attached (non-rejected) line must not leak in.
	for _, u := range got {
		if u == "https://ok.com/a.pdf" {
			t.Fatal("attached url misread as rejected")
		}
	}
	if RejectedURLs("no block here") != nil {
		t.Fatal("expected nil without a docfetch block")
	}
}

func TestAppendToUserOnlyNotes(t *testing.T) {
	out := Append("just my own text", Line("manual attached"))
	if !strings.HasPrefix(out, "just my own text") {
		t.Fatalf("user text must stay first: %q", out)
	}
	if !strings.Contains(out, "manual attached") {
		t.Fatal("line missing")
	}
}

func TestAppendPrunesToBudget(t *testing.T) {
	long := strings.Repeat("x", 60)
	out := "user text stays\n\n" + Begin + "\n" + header + "\n" +
		"- 2026-01-01 qr [link](https://keep.me/qr)\n" + End
	// Push way past the budget with plain audit lines.
	for i := 0; i < 30; i++ {
		out = Append(out, Line("meta filler "+long))
	}
	if len(out) > MaxNotes {
		t.Fatalf("notes exceed budget: %d > %d", len(out), MaxNotes)
	}
	if !strings.HasPrefix(out, "user text stays") {
		t.Fatal("user text must survive pruning")
	}
	if len(QRURLs(out)) != 1 {
		t.Fatalf("semantic qr line must survive audit pruning: %q", out)
	}
	// Newest audit line should still be present (oldest were dropped).
	if !strings.Contains(out, "meta filler") {
		t.Fatal("newest audit lines should remain")
	}
}

func TestBreadcrumbReplacesBlockKeepsUserText(t *testing.T) {
	existing := "my own note\n\n" + Begin + "\n#### docfetch\n- 2026-07-01 created via photo intake\n- 2026-07-02 qr [link](https://x/p)\n" + End + "\ntrailing user text"
	got := Breadcrumb(existing, "docfetch: manual ✓ — [log](http://p/log/e1)")
	if !strings.HasPrefix(got, "my own note") || !strings.HasSuffix(got, "trailing user text") {
		t.Fatalf("user text lost: %q", got)
	}
	if strings.Contains(got, "photo intake") || strings.Contains(got, "qr [link]") {
		t.Fatalf("old block lines must be replaced: %q", got)
	}
	if !strings.Contains(got, "- docfetch: manual ✓ — [log](http://p/log/e1)") {
		t.Fatalf("breadcrumb line missing: %q", got)
	}
}

func TestBreadcrumbCreatesBlock(t *testing.T) {
	got := Breadcrumb("", "docfetch: searching")
	if !strings.Contains(got, Begin) || !strings.Contains(got, "- docfetch: searching") {
		t.Fatalf("block not created: %q", got)
	}
}

func TestBreadcrumbIdempotent(t *testing.T) {
	once := Breadcrumb("user text", "docfetch: manual ✓")
	twice := Breadcrumb(once, "docfetch: manual ✓")
	if once != twice {
		t.Fatalf("breadcrumb must be stable:\n%q\n%q", once, twice)
	}
}
