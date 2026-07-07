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

func TestAppendToUserOnlyNotes(t *testing.T) {
	out := Append("just my own text", Line("manual attached"))
	if !strings.HasPrefix(out, "just my own text") {
		t.Fatalf("user text must stay first: %q", out)
	}
	if !strings.Contains(out, "manual attached") {
		t.Fatal("line missing")
	}
}
