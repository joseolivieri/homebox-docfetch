package discovery

import (
	"context"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/enrich"
)

// Search exposes raw SearXNG results (rate-limited) for the enrich engine and
// curation extras, satisfying enrich.Searcher. Results are stably reordered
// region-first: other-country-market URLs sink to the back, so consumers that
// take the top N (warranty candidates, enrich corroboration) see the
// configured region's sources first.
func (e *Engine) Search(ctx context.Context, query string) ([]enrich.SearchResult, error) {
	e.limiter.wait(ctx)
	rs, err := e.searxng(ctx, query)
	if err != nil {
		return nil, err
	}
	out := make([]enrich.SearchResult, 0, len(rs))
	var deferred []enrich.SearchResult
	for _, r := range rs {
		sr := enrich.SearchResult{Title: r.Title, URL: r.URL, Snippet: r.Content}
		if isCountrySpecificURL(r.URL, e.opt.Region) {
			deferred = append(deferred, sr)
		} else {
			out = append(out, sr)
		}
	}
	return append(out, deferred...), nil
}
