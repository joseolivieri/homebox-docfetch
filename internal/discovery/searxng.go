package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// searxResult is one hit from SearXNG's JSON API.
type searxResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

type searxResponse struct {
	Results []searxResult `json:"results"`
}

// searxng queries the configured SearXNG instance for one query string.
// Requires the instance to have the JSON output format enabled
// (search.formats: [html, json] in searxng settings).
func (e *Engine) searxng(ctx context.Context, query string) ([]searxResult, error) {
	q := url.Values{}
	q.Set("q", query)
	q.Set("format", "json")
	endpoint := strings.TrimRight(e.opt.SearxngURL, "/") + "/search?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("searxng request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("searxng http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out searxResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode searxng json (is the json format enabled?): %w", err)
	}
	return out.Results, nil
}
