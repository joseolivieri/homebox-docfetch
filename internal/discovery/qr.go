package discovery

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// qrCandidates is the "qr" pipeline stage: follow the manufacturer-printed
// support links decoded from the item's labels (Item.HintURLs). QR targets
// are usually shortlinks to a support/landing page — resolve redirects, then
// harvest that page's PDF links with the same machinery as brand-site
// page-follow. Everything found carries Official provenance (the maker put
// this link on the physical product), and the final domain seeds the brand
// cache so later items from the same maker skip the LLM domain lookup.
//
// ModelMatch is set even when the model number is absent from the URL: the
// physical label IS this exact product — stronger identity evidence than any
// URL token.
func (e *Engine) qrCandidates(ctx context.Context, it Item) []Candidate {
	var out []Candidate
	for i, hint := range it.HintURLs {
		if i >= 3 { // a label rarely carries more than one useful code
			break
		}
		finalURL, contentType := e.resolveQR(ctx, hint)
		if finalURL == "" {
			continue
		}
		e.seedBrandCache(it.Manufacturer, finalURL)

		if strings.Contains(contentType, "application/pdf") || strings.HasSuffix(strings.ToLower(finalURL), ".pdf") {
			out = append(out, Candidate{
				Title: "QR-linked document", URL: finalURL,
				IsPDF: true, Official: true, ModelMatch: true, Score: 12,
			})
			continue
		}
		// HTML landing/support page: harvest its PDF links, keep the page
		// itself as the linkable fallback.
		for _, pdf := range e.pdfLinksFrom(ctx, finalURL) {
			out = append(out, Candidate{
				Title: "QR page-follow", URL: pdf,
				IsPDF: true, Official: true, ModelMatch: true, Score: 11,
			})
		}
		out = append(out, Candidate{
			Title: "QR support page", URL: finalURL,
			IsHTML: true, Official: true, ModelMatch: true, Score: 8,
		})
	}
	if len(out) > 0 {
		log.Printf("qr stage: %d candidate(s) from %d label link(s)", len(out), len(it.HintURLs))
	}
	return out
}

// resolveQR follows redirects and reports the final URL + content type.
// HEAD first (cheap), GET fallback for servers that reject HEAD.
func (e *Engine) resolveQR(ctx context.Context, raw string) (string, string) {
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, err := http.NewRequestWithContext(ctx, method, raw, nil)
		if err != nil {
			return "", ""
		}
		req.Header.Set("User-Agent", browserUA)
		resp, err := e.http.Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp.Request.URL.String(), resp.Header.Get("Content-Type")
		}
	}
	log.Printf("qr stage: hint unreachable: %s", raw)
	return "", ""
}

// seedBrandCache records the manufacturer's domain from a QR target so the
// brand-site stage (and future items) skip the LLM domain resolution.
func (e *Engine) seedBrandCache(manufacturer, finalURL string) {
	m := strings.ToLower(strings.TrimSpace(manufacturer))
	if m == "" {
		return
	}
	d := rootDomain(finalURL)
	if d == "" {
		return
	}
	e.brandMu.Lock()
	defer e.brandMu.Unlock()
	if e.brandCache == nil {
		e.brandCache = map[string]string{}
	}
	if _, ok := e.brandCache[m]; !ok {
		e.brandCache[m] = d
		log.Printf("brand domain (qr-seeded): %q -> %s", manufacturer, d)
	}
}

// rootDomain reduces a URL's host to its registrable domain, best-effort
// (support.anker.com -> anker.com; x.co.uk style suffixes keep three labels).
func rootDomain(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	h := strings.ToLower(strings.TrimPrefix(u.Host, "www."))
	labels := strings.Split(h, ".")
	if len(labels) <= 2 {
		return h
	}
	// Multi-label public suffix like .co.uk / .com.au: keep three labels.
	sld := labels[len(labels)-2]
	if len(labels[len(labels)-1]) == 2 && (sld == "co" || sld == "com" || sld == "org" || sld == "net" || sld == "ac" || sld == "gov") {
		return strings.Join(labels[len(labels)-3:], ".")
	}
	return strings.Join(labels[len(labels)-2:], ".")
}
