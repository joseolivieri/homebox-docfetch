package llm

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestLiveExtractIdentity hits OpenRouter for real. Skipped unless
// OPENROUTER_API_KEY is set.
func TestLiveExtractIdentity(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("set OPENROUTER_API_KEY to run")
	}
	c := New("https://openrouter.ai/api/v1", key, "meta-llama/llama-3.1-8b-instruct")

	cands := []Candidate{
		{Title: "PlayStation Portal Remote Player | PlayStation (US)", URL: "https://www.playstation.com/en-us/accessories/playstation-portal-remote-player/", Snippet: "Stream games from your PS5 with PlayStation Portal remote player CFI-Y1000"},
		{Title: "Sony PlayStation Portal review", URL: "https://www.theverge.com/23949358/sony-playstation-portal-review", Snippet: "Sony's PlayStation Portal (model CFI-Y1000) is a remote play handheld"},
		{Title: "PlayStation Portal - Wikipedia", URL: "https://en.wikipedia.org/wiki/PlayStation_Portal", Snippet: "The PlayStation Portal is a remote play device by Sony Interactive Entertainment, model CFI-Y1000"},
	}
	id, err := c.ExtractIdentity(context.Background(), "PlayStation Portal", cands)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	t.Logf("identity: %+v", id)
	if !strings.Contains(strings.ToLower(id.Manufacturer), "sony") {
		t.Fatalf("expected Sony manufacturer, got %q", id.Manufacturer)
	}
	if !strings.Contains(strings.ToUpper(id.ModelNumber), "CFI-Y1000") {
		t.Fatalf("expected CFI-Y1000 model, got %q", id.ModelNumber)
	}
}
