package notes

import (
	"strings"
	"testing"
	"time"
)

func TestTarget(t *testing.T) {
	if got := Target("[pdf](https://x/a.pdf)"); got != "https://x/a.pdf" {
		t.Fatalf("md link: %q", got)
	}
	if got := Target("see https://x/b.pdf."); got != "https://x/b.pdf" {
		t.Fatalf("bare url: %q", got)
	}
	if got := Target("no url here"); got != "" {
		t.Fatalf("want empty, got %q", got)
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
	if strings.Count(got, Begin) != 1 || strings.Count(got, End) != 1 {
		t.Fatalf("duplicate markers: %q", got)
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

func TestBreadcrumbLine(t *testing.T) {
	ts := time.Date(2026, 7, 17, 15, 4, 0, 0, time.Local)
	got := BreadcrumbLine(7, ts, "https://portal.example/", "e1")
	want := "docfetch: 7 updates · last 2026-07-17 15:04 — [log](https://portal.example/log/e1)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if got := BreadcrumbLine(1, time.Time{}, "", "e1"); got != "docfetch: 1 update" {
		t.Fatalf("singular/no-portal/no-ts: %q", got)
	}
}
