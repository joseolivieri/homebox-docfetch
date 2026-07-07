package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
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
	if e.opt.Language != "" {
		q.Set("language", e.opt.Language)
	}
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

// ImageCandidate is a downloaded, rules-prefiltered product-image candidate
// ready for vision ranking.
type ImageCandidate struct {
	Data  []byte
	Mime  string
	Src   string
	Title string
}

// ProductImageCandidates gathers up to max downloadable image candidates for
// the subject: junk hosts skipped, loose topical token match, size sanity.
// Rules only prefilter here — the caller ranks with the vision model and
// applies its confidence threshold (no junk attach on a weak field).
func (e *Engine) ProductImageCandidates(ctx context.Context, subject string, max int, maxBytes int64) ([]ImageCandidate, error) {
	results, err := e.SearchImages(ctx, subject)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		// transient-empty retry, same rationale as the text search
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
		if results, err = e.SearchImages(ctx, subject); err != nil {
			return nil, err
		}
	}
	tokens := subjectTokens(subject)
	var out []ImageCandidate
	for _, r := range results {
		if len(out) >= max {
			break
		}
		if r.ImgSrc == "" || isJunkImageHost(r.ImgSrc) {
			continue
		}
		hay := norm(r.Title) + norm(r.URL) + norm(r.ImgSrc)
		if !anyToken(hay, tokens) {
			continue
		}
		b, m, derr := e.downloadImage(ctx, r.ImgSrc, maxBytes)
		if derr != nil || len(b) < 5_000 { // tiny images are thumbnails/icons
			continue
		}
		out = append(out, ImageCandidate{Data: b, Mime: m, Src: r.ImgSrc, Title: r.Title})
	}
	return out, nil
}

func (e *Engine) downloadImage(ctx context.Context, u string, maxBytes int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", browserUA)
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

// subjectTokens splits the subject into normalized words (>=3 chars). A
// candidate is topical if it contains ANY of them — a loose prefilter; the
// vision ranker + confidence threshold do the real quality filtering.
func subjectTokens(subject string) []string {
	var out []string
	for _, w := range strings.Fields(subject) {
		if n := norm(w); len(n) >= 3 {
			out = append(out, n)
		}
	}
	return out
}

func anyToken(hay string, tokens []string) bool {
	if len(tokens) == 0 {
		return true
	}
	for _, t := range tokens {
		if strings.Contains(hay, t) {
			return true
		}
	}
	return false
}
