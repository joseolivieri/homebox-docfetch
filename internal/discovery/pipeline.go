package discovery

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// BrandResolver maps a manufacturer to its official website domain (satisfied
// by *llm.Client.BrandDomain). Optional — nil disables the brand-site stage.
type BrandResolver interface {
	BrandDomain(ctx context.Context, manufacturer string) (string, error)
}

// SetBrandResolver enables the brand-site pipeline stage.
func (e *Engine) SetBrandResolver(b BrandResolver) { e.brands = b }

// brandDomain resolves + caches the official domain for a manufacturer.
func (e *Engine) brandDomain(ctx context.Context, manufacturer string) string {
	m := strings.ToLower(strings.TrimSpace(manufacturer))
	if m == "" || e.brands == nil {
		return ""
	}
	e.brandMu.Lock()
	defer e.brandMu.Unlock()
	if e.brandCache == nil {
		e.brandCache = map[string]string{}
	}
	if d, ok := e.brandCache[m]; ok {
		return d
	}
	d, err := e.brands.BrandDomain(ctx, manufacturer)
	if err != nil {
		log.Printf("brand domain lookup failed for %q: %v", manufacturer, err)
		return "" // not cached; retry next item
	}
	e.brandCache[m] = d
	if d != "" {
		log.Printf("brand domain: %q -> %s", manufacturer, d)
	}
	return d
}

// brandSiteCandidates searches the manufacturer's own site (no filetype filter
// — official support pages are HTML and often not SEO-indexed as PDFs), then
// follows the top support pages and harvests the PDF links they reference.
// Official-domain results are the most trustworthy source we have.
func (e *Engine) brandSiteCandidates(ctx context.Context, it Item) []Candidate {
	domain := e.brandDomain(ctx, it.Manufacturer)
	if domain == "" {
		return nil
	}
	key := strings.TrimSpace(it.ModelNumber)
	if key == "" {
		key = it.Name
	}
	e.limiter.wait(ctx)
	results, err := e.searxng(ctx, "site:"+domain+" "+key+" manual")
	if err != nil || len(results) == 0 {
		// site: operator support varies by engine; try a plain scoped query
		e.limiter.wait(ctx)
		results, err = e.searxng(ctx, domain+" "+key+" manual")
		if err != nil {
			return nil
		}
	}
	log.Printf("brand-site %s %q -> %d results", domain, key, len(results))

	var cands []Candidate
	pagesFollowed := 0
	for _, r := range results {
		if !strings.Contains(strings.ToLower(r.URL), domain) {
			continue
		}
		// Identity gate: a brand-domain page must reference THIS product in
		// its URL/TITLE before anything from it earns full trust (ModelMatch +
		// page-follow). Site search with a bare model number returns unrelated
		// products from the same maker (observed live: the Anker Prime 96K
		// page for a C30i query), and page-follow from a wrong page poisons
		// everything downstream.
		strongID := identityMatch(it, r.URL+" "+r.Title)
		if !strongID {
			// Weaker tier: the model only appears in the SNIPPET (manufacturer
			// URLs often carry internal doc numbers — whirlpool.com's
			// owners-manual-W10903644 never names the model in its URL). Keep
			// it as an Official candidate WITHOUT ModelMatch: the model gate
			// holds it back from blind attach, and skim-promote content-reads
			// it before any third-party re-host gets a chance.
			if !identityMatch(it, r.URL+" "+r.Title+" "+r.Content) {
				continue
			}
			if strings.HasSuffix(strings.ToLower(r.URL), ".pdf") {
				cands = append(cands, Candidate{
					Title: r.Title, URL: r.URL,
					Snippet:  truncate(r.Content, e.opt.MaxSnippetChars),
					Official: true, Score: 3,
				})
			}
			continue // no page-follow from weakly-matched pages
		}
		if strings.HasSuffix(strings.ToLower(r.URL), ".pdf") {
			cands = append(cands, Candidate{
				Title: r.Title, URL: r.URL,
				Snippet:  truncate(r.Content, e.opt.MaxSnippetChars),
				Official: true, ModelMatch: true, Score: 3,
			})
			continue
		}
		// HTML page on the brand domain: candidate itself (link fallback) and
		// a source of direct PDF links. ModelMatch records that the identity
		// gate passed.
		cands = append(cands, Candidate{
			Title: r.Title, URL: r.URL,
			Snippet:  truncate(r.Content, e.opt.MaxSnippetChars),
			Official: true, ModelMatch: true, IsHTML: true, Score: 1,
		})
		if pagesFollowed < 2 {
			pagesFollowed++
			for _, pdfURL := range e.pdfLinksFrom(ctx, r.URL) {
				cands = append(cands, Candidate{
					Title:    "PDF linked from " + r.Title,
					URL:      pdfURL,
					Snippet:  "linked from official support page",
					Official: true, ModelMatch: true, Score: 4,
				})
			}
		}
		if len(cands) >= e.opt.MaxCandidates {
			break
		}
	}
	return dedupeByURL(cands, e.opt.MaxCandidates)
}

// identityMatch reports whether a URL/title haystack references the item.
// With a model number known, the model must appear (precise). Without one,
// any distinctive product-name token (>=4 chars, not the manufacturer name)
// counts — looser, but generic-word false positives are bounded by the
// downstream verify/link gates.
func identityMatch(it Item, hay string) bool {
	h := norm(hay)
	if m := norm(it.ModelNumber); m != "" {
		return strings.Contains(h, m)
	}
	mfr := norm(it.Manufacturer)
	for _, w := range strings.Fields(it.Name) {
		t := norm(w)
		if len(t) < 4 || (mfr != "" && strings.Contains(mfr, t)) {
			continue
		}
		if strings.Contains(h, t) {
			return true
		}
	}
	return false
}

var (
	hrefRe     = regexp.MustCompile(`(?i)href\s*=\s*["']([^"']+\.pdf[^"']*)["']`)
	legalDocRe = regexp.MustCompile(`(?i)statement|policy|terms|privacy|compliance|conduct|msa_|warranty-policy|declaration|conformity`)
)

// buildKeepRe compiles the page-follow keep-set from every class's keywords, so
// a class the operator adds (e.g. "parts") is automatically harvested from
// support pages. Without this a parts PDF would be dropped before it could be
// classified. Spaces in a keyword match loosely across separators.
func buildKeepRe(classes []DocClass) *regexp.Regexp {
	seen := map[string]bool{}
	var alts []string
	for _, c := range classes {
		for _, kw := range c.Keywords {
			kw = strings.ToLower(strings.TrimSpace(kw))
			if kw == "" || seen[kw] {
				continue
			}
			seen[kw] = true
			parts := strings.Fields(kw)
			for i := range parts {
				parts[i] = regexp.QuoteMeta(parts[i])
			}
			alts = append(alts, strings.Join(parts, `[\s_-]*`))
		}
	}
	if len(alts) == 0 {
		alts = []string{"manual", "guide"}
	}
	return regexp.MustCompile(`(?i)` + strings.Join(alts, "|"))
}

// pdfLinksFrom fetches an HTML page and extracts absolute PDF links.
func (e *Engine) pdfLinksFrom(ctx context.Context, pageURL string) []string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := e.http.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	base, err := url.Parse(pageURL)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var manualish, other []string
	for _, m := range hrefRe.FindAllStringSubmatch(string(body), 24) {
		u, err := url.Parse(strings.TrimSpace(m[1]))
		if err != nil {
			continue
		}
		abs := base.ResolveReference(u).String()
		if seen[abs] {
			continue
		}
		seen[abs] = true
		l := strings.ToLower(abs)
		if legalDocRe.MatchString(l) {
			continue // footer/legal PDFs (MSA statements, policies) — never docs
		}
		if e.docKeep.MatchString(l) {
			manualish = append(manualish, abs)
		} else {
			other = append(other, abs)
		}
	}
	out := manualish
	if len(out) == 0 {
		out = other // knowledge-CDN links are often opaque ids; keep them
	}
	if len(out) > 5 {
		out = out[:5]
	}
	if len(out) > 0 {
		log.Printf("page-follow %s -> %d pdf link(s)", pageURL, len(out))
	}
	return out
}

// webCandidates is the general search stage. pdfOnly=true uses the configured
// query templates (typically filetype:pdf); pdfOnly=false strips the filetype
// filter so official HTML manual pages surface too.
func (e *Engine) webCandidates(ctx context.Context, it Item, pdfOnly bool, queries []string) []Candidate {
	seen := map[string]bool{}
	var cands []Candidate
	for _, q := range queries {
		if !pdfOnly {
			q = strings.TrimSpace(strings.ReplaceAll(q, "filetype:pdf", ""))
		}
		e.limiter.wait(ctx)
		results, err := e.searxng(ctx, q)
		if err != nil {
			return cands
		}
		if len(results) == 0 {
			results, _ = e.searxngRetry(ctx, q)
		}
		log.Printf("searxng %q -> %d results", q, len(results))
		for _, r := range results {
			if seen[r.URL] || r.URL == "" {
				continue
			}
			seen[r.URL] = true
			if isListingHostedDoc(r.URL) || isBotBlockedDocHost(r.URL) {
				continue
			}
			cands = append(cands, Candidate{
				Title:   r.Title,
				URL:     r.URL,
				Snippet: truncate(r.Content, e.opt.MaxSnippetChars),
			})
			if len(cands) >= e.opt.MaxCandidates {
				return cands
			}
		}
	}
	return cands
}

func (e *Engine) searxngRetry(ctx context.Context, q string) ([]searxResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timeAfter2s():
	}
	return e.searxng(ctx, q)
}

func dedupeByURL(cands []Candidate, max int) []Candidate {
	seen := map[string]bool{}
	out := cands[:0]
	for _, c := range cands {
		if seen[c.URL] {
			continue
		}
		seen[c.URL] = true
		out = append(out, c)
		if len(out) >= max {
			break
		}
	}
	return out
}

func timeAfter2s() <-chan time.Time { return time.After(2 * time.Second) }
