package llm

import (
	"context"
	"os"
	"testing"
)

// TestLiveRerank hits OpenRouter for real. Skipped unless OPENROUTER_API_KEY is
// set. Validates end-to-end: request shape, JSON extraction, index/confidence.
func TestLiveRerank(t *testing.T) {
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("set OPENROUTER_API_KEY to run live rerank test")
	}
	model := os.Getenv("RERANK_MODEL")
	if model == "" {
		model = "meta-llama/llama-3.1-8b-instruct"
	}
	c := New("https://openrouter.ai/api/v1", key, model)

	cands := []Candidate{
		{Title: "Random blog about widgets", URL: "https://blog.example.com/widgets", Snippet: "opinions"},
		{Title: "Acme W-1000 User Manual (PDF)", URL: "https://acme.com/support/W-1000-manual.pdf", Snippet: "Official user guide for the Acme W-1000"},
		{Title: "Buy Acme W-1000 on ShopSite", URL: "https://shopsite.com/p/acme-w-1000", Snippet: "in stock"},
	}
	idx, conf, err := c.Rerank(context.Background(), "Acme W-1000 Widget", cands)
	if err != nil {
		t.Fatalf("rerank: %v", err)
	}
	t.Logf("best=%d confidence=%.2f -> %s", idx, conf, func() string {
		if idx >= 0 {
			return cands[idx].URL
		}
		return "(none)"
	}())
	if idx != 1 {
		t.Fatalf("expected the official PDF (index 1), got %d", idx)
	}
	if conf <= 0 || conf > 1 {
		t.Fatalf("confidence out of range: %v", conf)
	}
}
