// Package discovery finds the best user-manual / support-doc URL for a Homebox
// item. Pipeline (see docs/spec.md §3): SearXNG search -> rules filter/score ->
// LLM rerank only when ambiguous -> confidence.
//
// Rules-first is deliberate: an obvious winner (a PDF whose title/url matches
// the model number) is chosen with no LLM call, keeping the token budget tiny.
package discovery

import (
	"context"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/llm"
)

// Item is the identity we search on.
type Item struct {
	Manufacturer string
	ModelNumber  string
	Name         string
}

func (i Item) desc() string {
	parts := []string{}
	for _, p := range []string{i.Manufacturer, i.ModelNumber, i.Name} {
		if strings.TrimSpace(p) != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " ")
}

// subject is the search subject: manufacturer+model when a model number is
// known, otherwise manufacturer+name (or just name). Keeps queries clean
// whether the item has structured metadata or only a name.
func (i Item) subject() string {
	if strings.TrimSpace(i.ModelNumber) != "" {
		return strings.TrimSpace(i.Manufacturer + " " + i.ModelNumber)
	}
	return strings.TrimSpace(i.Manufacturer + " " + i.Name)
}

// Candidate is a scored search result.
type Candidate struct {
	Title       string
	URL         string
	Snippet     string
	ContentType string
	Size        int64
	IsPDF       bool
	IsHTML      bool // manual page rather than a document file
	Official    bool // hosted on the manufacturer's own domain
	ModelMatch  bool
	Score       float64
}

// Result is the discovery outcome for one item.
type Result struct {
	Best       *Candidate
	BestHTML   *Candidate // best official/model-matching HTML manual page (link fallback)
	Confidence float64
	UsedLLM    bool
	Candidates []Candidate
}

// Reranker is satisfied by *llm.Client; injected so the rules path is testable
// without a network dependency.
type Reranker interface {
	Rerank(ctx context.Context, itemDesc string, cands []llm.Candidate) (int, float64, error)
}

type Options struct {
	SearxngURL      string
	Language        string   // SearXNG language code; biases all searches
	Pipeline        []string // source stages in priority order: brand-site, web-pdf, web-html
	StopConfidence  float64  // stop at the first stage whose pick reaches this
	Queries         []string
	MaxCandidates   int
	MinPDFBytes     int64
	MaxPDFBytes     int64
	MaxSnippetChars int
	RequireModel    bool
	RatePerMin      int
}

type Engine struct {
	opt      Options
	http     *http.Client
	limiter  *limiter
	reranker Reranker      // may be nil (rules-only)
	verifier Verifier      // may be nil (no content verification)
	brands   BrandResolver // may be nil (brand-site stage disabled)

	brandMu    sync.Mutex
	brandCache map[string]string
}

func NewEngine(opt Options, reranker Reranker) *Engine {
	if opt.MaxCandidates == 0 {
		opt.MaxCandidates = 8
	}
	if opt.MaxSnippetChars == 0 {
		opt.MaxSnippetChars = 150
	}
	if len(opt.Pipeline) == 0 {
		opt.Pipeline = []string{"brand-site", "web-pdf", "web-html"}
	}
	if opt.StopConfidence == 0 {
		opt.StopConfidence = 0.7
	}
	return &Engine{
		opt:      opt,
		http:     &http.Client{Timeout: 30 * time.Second},
		limiter:  newLimiter(opt.RatePerMin),
		reranker: reranker,
	}
}

// browserUA: several vendor CDNs (HubSpot et al.) 403 Go's default UA on GET
// while allowing browsers — observed live (lp.ankerjapan.com).
const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func norm(s string) string { return nonAlnum.ReplaceAllString(strings.ToLower(s), "") }

// renderQueries substitutes {manufacturer}/{modelNumber}/{name} in templates.
func (e *Engine) renderQueries(it Item) []string {
	r := strings.NewReplacer(
		"{subject}", it.subject(),
		"{manufacturer}", it.Manufacturer,
		"{modelNumber}", it.ModelNumber,
		"{name}", it.Name,
	)
	var out []string
	for _, q := range e.opt.Queries {
		out = append(out, strings.TrimSpace(r.Replace(q)))
	}
	return out
}

// Discover runs the source-priority pipeline for one item: stages execute in
// the configured order and the first stage whose pick reaches StopConfidence
// wins — official brand-site sources beat general web PDFs beat HTML pages.
func (e *Engine) Discover(ctx context.Context, it Item) (*Result, error) {
	requireModel := e.opt.RequireModel && strings.TrimSpace(it.ModelNumber) != ""

	var all []Candidate
	for _, stage := range e.opt.Pipeline {
		var cands []Candidate
		switch stage {
		case "brand-site":
			cands = e.brandSiteCandidates(ctx, it)
		case "web-pdf":
			cands = e.webCandidates(ctx, it, true)
		case "web-html":
			cands = e.webCandidates(ctx, it, false)
		default:
			log.Printf("unknown pipeline stage %q — skipping", stage)
		}
		if len(cands) == 0 {
			continue
		}
		e.scoreCandidates(ctx, it, cands)
		res := e.pick(ctx, it, cands, requireModel)
		if res.Best != nil && res.Confidence >= e.opt.StopConfidence {
			log.Printf("pipeline stage %q produced confident pick (%.2f)", stage, res.Confidence)
			res.Candidates = append(cands, all...)
			res.BestHTML = bestHTML(res.Candidates)
			return res, nil
		}
		all = append(all, cands...)
	}

	// No stage was confident: final pick across everything + HTML fallback.
	res := e.pick(ctx, it, all, requireModel)
	res.BestHTML = bestHTML(all)
	return res, nil
}

// pick applies the existing selection ladder to one candidate set:
// clear rules winner -> LLM rerank -> score fallback.
func (e *Engine) pick(ctx context.Context, it Item, cands []Candidate, requireModel bool) *Result {
	res := &Result{Candidates: cands}
	if len(cands) == 0 {
		return res
	}
	if best, ok := clearWinner(cands); ok {
		res.Best = best
		res.Confidence = 0.9
		return e.applyModelGate(res, requireModel)
	}
	if e.reranker != nil {
		lc := make([]llm.Candidate, len(cands))
		for i, c := range cands {
			lc[i] = llm.Candidate{Title: c.Title, URL: c.URL, Snippet: c.Snippet}
		}
		e.limiter.wait(ctx)
		desc := it.desc()
		if e.opt.Language != "" {
			desc += " (prefer documents in language: " + e.opt.Language + ")"
		}
		idx, conf, err := e.reranker.Rerank(ctx, desc, lc)
		if err == nil && idx >= 0 {
			res.Best = &cands[idx]
			res.Confidence = conf
			res.UsedLLM = true
			return e.applyModelGate(res, requireModel)
		}
	}
	best := &cands[0]
	for i := range cands {
		if cands[i].Score > best.Score {
			best = &cands[i]
		}
	}
	res.Best = best
	res.Confidence = 0.4
	return e.applyModelGate(res, requireModel)
}

// bestHTML selects the strongest HTML manual page for the link fallback:
// official-domain pages first, then model-matching ones. nil when none qualify.
func bestHTML(cands []Candidate) *Candidate {
	var best *Candidate
	for i := range cands {
		c := &cands[i]
		if !c.IsHTML || (!c.Official && !c.ModelMatch) {
			continue
		}
		if best == nil || c.Score > best.Score || (c.Official && !best.Official) {
			best = c
		}
	}
	return best
}

// applyModelGate zeroes confidence when a model match is required but absent.
func (e *Engine) applyModelGate(res *Result, requireModel bool) *Result {
	if requireModel && res.Best != nil && !res.Best.ModelMatch {
		res.Confidence = 0
	}
	return res
}

func (e *Engine) scoreCandidates(ctx context.Context, it Item, cands []Candidate) {
	model := norm(it.ModelNumber)
	for i := range cands {
		c := &cands[i]
		if model != "" {
			hay := norm(c.Title) + norm(c.URL) + norm(c.Snippet)
			c.ModelMatch = strings.Contains(hay, model)
		}
		ct, size := e.head(ctx, c.URL)
		c.ContentType = ct
		c.Size = size
		c.IsPDF = strings.Contains(ct, "pdf") || strings.HasSuffix(strings.ToLower(c.URL), ".pdf")
		if !c.IsPDF && (c.IsHTML || strings.Contains(ct, "html")) {
			c.IsHTML = true
		}
		if c.IsPDF && (size == 0 || (size >= e.opt.MinPDFBytes && size <= e.opt.MaxPDFBytes)) {
			c.Score += 2
		}
		if c.ModelMatch {
			c.Score += 2
		}
		if isVendorish(c.URL) {
			c.Score++
		}
		if c.Official {
			c.Score += 2
		}
	}
}

// clearWinner returns the sole model-matching PDF, if exactly one exists.
func clearWinner(cands []Candidate) (*Candidate, bool) {
	var win *Candidate
	n := 0
	for i := range cands {
		if cands[i].IsPDF && cands[i].ModelMatch {
			win = &cands[i]
			n++
		}
	}
	if n == 1 {
		return win, true
	}
	return nil, false
}

func (e *Engine) head(ctx context.Context, url string) (contentType string, size int64) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", 0
	}
	req.Header.Set("User-Agent", browserUA)
	resp, err := e.http.Do(req)
	if err != nil {
		return "", 0
	}
	defer resp.Body.Close()
	return resp.Header.Get("Content-Type"), resp.ContentLength
}

// isBotBlockedDocHost flags doc hosts that 403 all non-browser clients
// (Cloudflare TLS fingerprinting; verified live with full browser headers).
func isBotBlockedDocHost(u string) bool {
	l := strings.ToLower(u)
	for _, bad := range []string{"fccid.io", "manuals.plus", "manualzz.com"} {
		if strings.Contains(l, bad) {
			return true
		}
	}
	return false
}

// isListingHostedDoc flags docs hosted on marketplace/listing CDNs.
func isListingHostedDoc(u string) bool {
	l := strings.ToLower(u)
	for _, bad := range []string{"media-amazon.com", "images-amazon.com", "ssl-images-amazon", "ebayimg", "aliexpress"} {
		if strings.Contains(l, bad) {
			return true
		}
	}
	return false
}

func isVendorish(u string) bool {
	l := strings.ToLower(u)
	// Aggregators are fine but official domains score a touch higher.
	for _, bad := range []string{"manualslib", "manua.ls", "manualsonline", "scribd", "amazon."} {
		if strings.Contains(l, bad) {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
