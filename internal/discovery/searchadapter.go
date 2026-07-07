package discovery

import (
	"context"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/enrich"
)

// Search exposes raw SearXNG results (rate-limited) for the enrich engine,
// satisfying enrich.Searcher.
func (e *Engine) Search(ctx context.Context, query string) ([]enrich.SearchResult, error) {
	e.limiter.wait(ctx)
	rs, err := e.searxng(ctx, query)
	if err != nil {
		return nil, err
	}
	out := make([]enrich.SearchResult, 0, len(rs))
	for _, r := range rs {
		out = append(out, enrich.SearchResult{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return out, nil
}
