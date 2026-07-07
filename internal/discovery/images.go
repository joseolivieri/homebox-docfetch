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

// ImageResult is one hit from SearXNG's image category.
type ImageResult struct {
	Title  string `json:"title"`
	URL    string `json:"url"`     // page URL
	ImgSrc string `json:"img_src"` // direct image URL
}

// SearchImages queries SearXNG's image category.
func (e *Engine) SearchImages(ctx context.Context, query string) ([]ImageResult, error) {
	e.limiter.wait(ctx)
	q := url.Values{}
	q.Set("q", query)
	q.Set("format", "json")
	q.Set("categories", "images")
	endpoint := strings.TrimRight(e.opt.SearxngURL, "/") + "/search?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("searxng images http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		Results []ImageResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out.Results, nil
}

// BestProductImage picks and downloads a plausible official product image for
// the subject. Rules-only: prefer results whose title/url mention the subject
// tokens, skip obvious junk hosts, sanity-check content type and size. Returns
// nil bytes when nothing qualifies (caller skips the photo — no junk images).
func (e *Engine) BestProductImage(ctx context.Context, subject string, maxBytes int64) (data []byte, mime string, srcURL string, err error) {
	results, err := e.SearchImages(ctx, subject)
	if err != nil {
		return nil, "", "", err
	}
	probe := norm(subject)
	for _, r := range results {
		if r.ImgSrc == "" {
			continue
		}
		hay := norm(r.Title) + norm(r.URL) + norm(r.ImgSrc)
		if probe != "" && !strings.Contains(hay, firstToken(probe)) {
			continue
		}
		if isJunkImageHost(r.ImgSrc) {
			continue
		}
		b, m, derr := e.downloadImage(ctx, r.ImgSrc, maxBytes)
		if derr != nil || len(b) < 5_000 { // tiny images are thumbnails/icons
			continue
		}
		return b, m, r.ImgSrc, nil
	}
	return nil, "", "", nil
}

func (e *Engine) downloadImage(ctx context.Context, u string, maxBytes int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(ct, "image/") {
		return nil, "", fmt.Errorf("not an image: %s (%s)", resp.Status, ct)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(b)) > maxBytes {
		return nil, "", fmt.Errorf("image exceeds %d bytes", maxBytes)
	}
	return b, ct, nil
}

func isJunkImageHost(u string) bool {
	l := strings.ToLower(u)
	for _, bad := range []string{"pinterest.", "ebay", "auctions.", "yimg.jp", "mercari", "aliexpress", "alibaba", "etsy.", "reddit", "fbcdn", "instagram"} {
		if strings.Contains(l, bad) {
			return true
		}
	}
	return false
}

// firstToken returns the first ~8 chars of the normalized subject — enough to
// require topical relevance without demanding an exact match.
func firstToken(probe string) string {
	if len(probe) > 8 {
		return probe[:8]
	}
	return probe
}
