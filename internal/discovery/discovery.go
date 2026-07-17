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
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/joseolivieri/homebox-docfetch/internal/llm"
)

// Item is the identity we search on.
type Item struct {
	Manufacturer string
	ModelNumber  string
	Name         string
	// HintURLs are manufacturer-printed support links (QR codes decoded from
	// the item's physical labels at intake). The "qr" pipeline stage follows
	// them before any searching — strongest provenance available.
	HintURLs []string
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
	Class       string // doc class this candidate was classified into ("manual", "parts", …)
	Score       float64
}

// DocClass is the discovery-side view of a document class: enough to classify
// candidates, keep the right links during page-follow, and expand web queries.
// The scanner owns the richer config (field label, attach type, categories).
type DocClass struct {
	Name     string
	Keywords []string
	Queries  []string
}

// Result is the discovery outcome for one item.
type Result struct {
	Best       *Candidate
	BestHTML   *Candidate // best official/model-matching HTML manual page (link fallback)
	Confidence float64
	UsedLLM    bool
	Stage      string // pipeline stage that produced the pick ("" = combined final pick)
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
	Region          string   // ISO country code (e.g. "us"); other-market URLs are deprioritized everywhere. "" disables
	Pipeline        []string // source stages in priority order: brand-site, web-pdf, web-html
	StopConfidence  float64  // stop at the first stage whose pick reaches this
	Queries         []string
	MaxCandidates   int
	MinPDFBytes     int64
	MaxPDFBytes     int64
	MaxSnippetChars int
	RequireModel    bool
	RatePerMin      int
	Classes         []DocClass // doc classes to classify into; empty => single "manual" class
}

type Engine struct {
	opt      Options
	http     *http.Client
	limiter  *limiter
	reranker Reranker      // may be nil (rules-only)
	verifier Verifier      // may be nil (no content verification)
	brands   BrandResolver // may be nil (brand-site stage disabled)
	docKeep  *regexp.Regexp

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
		opt.Pipeline = []string{"qr", "brand-site", "web-pdf", "web-html"}
	}
	if opt.StopConfidence == 0 {
		opt.StopConfidence = 0.7
	}
	if len(opt.Classes) == 0 {
		opt.Classes = []DocClass{{Name: "manual", Keywords: []string{"manual", "guide", "user", "instruction"}}}
	}
	return &Engine{
		opt:      opt,
		http:     &http.Client{Timeout: 30 * time.Second},
		limiter:  newLimiter(opt.RatePerMin),
		reranker: reranker,
		docKeep:  buildKeepRe(opt.Classes),
	}
}

// classOf classifies a candidate by keyword match on its url/title/snippet.
// Non-default classes are checked first (a "parts" doc mentioning "manual"
// must classify as parts); anything unmatched falls to the primary class.
func (e *Engine) classOf(c *Candidate, primary string) string {
	hay := strings.ToLower(c.Title + " " + c.URL + " " + c.Snippet)
	for _, dc := range e.opt.Classes {
		if dc.Name == primary {
			continue
		}
		for _, kw := range dc.Keywords {
			if kw = strings.ToLower(strings.TrimSpace(kw)); kw != "" && strings.Contains(hay, kw) {
				return dc.Name
			}
		}
	}
	return primary
}

// browserUA: several vendor CDNs (HubSpot et al.) 403 Go's default UA on GET
// while allowing browsers — observed live (lp.ankerjapan.com).
const browserUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36"

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func norm(s string) string { return nonAlnum.ReplaceAllString(strings.ToLower(s), "") }

// renderTemplates substitutes {subject}/{manufacturer}/{modelNumber}/{name}
// in query templates and de-duplicates the rendered result.
func (e *Engine) renderTemplates(it Item, tmpls []string) []string {
	r := strings.NewReplacer(
		"{subject}", it.subject(),
		"{manufacturer}", it.Manufacturer,
		"{modelNumber}", it.ModelNumber,
		"{name}", it.Name,
	)
	seen := map[string]bool{}
	var out []string
	for _, q := range tmpls {
		q = strings.TrimSpace(r.Replace(q))
		if q != "" && !seen[q] {
			seen[q] = true
			out = append(out, q)
		}
	}
	return out
}

// Discover gathers and classifies candidates across the source-priority
// pipeline for one item. want is the set of doc-class names the caller intends
// to attach (category-filtered by the scanner); the web stages run each
// wanted class's query templates, and the pipeline stops early once the
// PRIMARY class ("manual", or want[0]) has a clear winner — so most items
// still resolve at the brand-site stage without touching general search.
// Per-class selection happens in SelectClass; Discover leaves Best unset.
func (e *Engine) Discover(ctx context.Context, it Item, want []string) (*Result, error) {
	primary := primaryClass(want)

	// Web-stage queries: the base templates plus every wanted class's own.
	var tmpls []string
	tmpls = append(tmpls, e.opt.Queries...)
	for _, dc := range e.opt.Classes {
		if contains(want, dc.Name) {
			tmpls = append(tmpls, dc.Queries...)
		}
	}
	queries := e.renderTemplates(it, tmpls)

	var all []Candidate
	lastStage := ""
	for _, stage := range e.opt.Pipeline {
		var cands []Candidate
		switch stage {
		case "qr":
			cands = e.qrCandidates(ctx, it)
		case "brand-site":
			cands = e.brandSiteCandidates(ctx, it)
		case "web-pdf":
			cands = e.webCandidates(ctx, it, true, queries)
		case "web-html":
			cands = e.webCandidates(ctx, it, false, queries)
		default:
			log.Printf("unknown pipeline stage %q — skipping", stage)
		}
		if len(cands) == 0 {
			continue
		}
		e.scoreCandidates(ctx, it, cands)
		for i := range cands {
			cands[i].Class = e.classOf(&cands[i], primary)
		}
		all = append(all, cands...)
		lastStage = stage

		// Early stop: the primary class has an unambiguous winner in what we've
		// gathered so far. Secondary classes (parts, etc.) ride on whatever the
		// same stages already surfaced — brand-site page-follow harvests them
		// alongside the manual — so we don't push into general search for them.
		if _, ok := clearWinner(classSubset(all, primary)); ok {
			log.Printf("pipeline stage %q: primary class %q has a clear winner; stopping", stage, primary)
			break
		}
	}

	return &Result{Candidates: all, Stage: lastStage}, nil
}

// SelectClass runs the selection ladder over one class's candidates, producing
// the per-class attach/link decision (Best PDF, BestHTML link fallback,
// confidence). Called once per enabled class by the scanner.
func (e *Engine) SelectClass(ctx context.Context, it Item, cands []Candidate, class string) *Result {
	requireModel := e.opt.RequireModel && strings.TrimSpace(it.ModelNumber) != ""
	sub := classSubset(cands, class)
	res := e.pick(ctx, it, sub, requireModel)
	res.BestHTML = bestHTML(sub)
	res.Stage = class
	return res
}

func primaryClass(want []string) string {
	if contains(want, "manual") || len(want) == 0 {
		return "manual"
	}
	return want[0]
}

func classSubset(cands []Candidate, class string) []Candidate {
	out := make([]Candidate, 0, len(cands))
	for _, c := range cands {
		if c.Class == class {
			out = append(out, c)
		}
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
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
			desc += " (prefer documents in language: " + e.opt.Language + "; prefer US/global-market sources over country-specific sites)"
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
// bestHTML picks the linkable manual page. ModelMatch (identity relevance) is
// REQUIRED — Official alone is not enough: a brand-domain page for a different
// product must never become this item's Manual (web) link (observed live: an
// Anker Prime page linked onto a Soundcore C30i).
func bestHTML(cands []Candidate) *Candidate {
	var best *Candidate
	for i := range cands {
		c := &cands[i]
		if !c.IsHTML || !c.ModelMatch || isMarketplacePage(c.URL) {
			continue
		}
		if best == nil || c.Score > best.Score || (c.Official && !best.Official) {
			best = c
		}
	}
	return best
}

// isMarketplacePage flags listing/search pages and platform pages that must
// never become a "<Field> (web)" support link — a marketplace search result
// is not documentation (observed live: an eBay shop search linked as the
// manual page for a water timer).
func isMarketplacePage(u string) bool {
	l := strings.ToLower(u)
	for _, bad := range []string{
		"ebay.", "amazon.", "walmart.", "aliexpress.", "alibaba.", "etsy.",
		"temu.", "wish.com", "mercari", "rakuten.", "shopee.",
	} {
		if strings.Contains(l, bad) {
			return true
		}
	}
	return isPlatformPage(u)
}

// isPlatformPage flags video/social platform hosts. A maker's YouTube channel
// is genuine provenance (recorded as a qr.link event for the future
// maintenance-videos milestone) but is not documentation: no PDFs to harvest,
// never a brand domain, never a "(web)" support link.
func isPlatformPage(u string) bool {
	l := strings.ToLower(u)
	for _, p := range []string{
		"youtube.", "youtu.be", "vimeo.", "tiktok.",
		"facebook.", "instagram.", "twitter.", "x.com/", "linktr.ee",
	} {
		if strings.Contains(l, p) {
			return true
		}
	}
	return false
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
		if isCountrySpecificURL(c.URL, e.opt.Region) {
			// Bias toward the configured region / global sources —
			// country-market sites can carry different warranty terms and
			// non-target-language documents.
			c.Score--
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

// countryCodes are market codes recognized in ccTLDs, path segments, and
// locale pairs. Deliberately excludes ambiguous English-word codes ("in" and
// "id" stay host-only via the ccTLD check below, not path matching).
var countryCodes = map[string]bool{
	"uk": true, "gb": true, "au": true, "nz": true, "ca": true, "us": true,
	"jp": true, "cn": true, "kr": true, "tw": true, "hk": true, "sg": true,
	"de": true, "fr": true, "it": true, "es": true, "nl": true, "pl": true,
	"se": true, "no": true, "dk": true, "fi": true, "at": true, "ch": true,
	"be": true, "pt": true, "gr": true, "cz": true, "ro": true, "hu": true,
	"ru": true, "tr": true, "br": true, "mx": true, "vn": true, "th": true,
	"my": true, "ae": true, "sa": true, "za": true, "ie": true,
}

// pseudoGenericTLDs are two-letter ccTLDs used generically by global sites —
// never treated as country markers.
var pseudoGenericTLDs = map[string]bool{
	"io": true, "co": true, "ai": true, "tv": true, "me": true, "fm": true,
	"gg": true, "sh": true, "cc": true, "ws": true, "to": true, "ly": true,
}

var countryWords = []string{"japan", "china", "korea", "europe", "deutschland", "vietnam", "france", "italia", "espana", "brasil", "australia"}

// isCountrySpecificURL flags URLs that target a country market OTHER than the
// configured region — via ccTLD (anker.jp), country-named host
// (ankervietnam.vn), or a market path/locale segment (anker.com/nz/…,
// support.x.com/en-nz/…). region "" disables the check.
func isCountrySpecificURL(raw, region string) bool {
	if region == "" {
		return false
	}
	region = strings.ToLower(region)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	h := strings.ToLower(u.Host)
	// ccTLD (last label): ANY two-letter TLD other than the region is a
	// country marker, minus the pseudo-generic set (.io/.co/…). The named map
	// alone missed real markets (innovar.com.ve served a Whirlpool manual).
	labels := strings.Split(h, ".")
	if last := labels[len(labels)-1]; len(last) == 2 && last != region && !pseudoGenericTLDs[last] {
		return true
	}
	for _, w := range countryWords {
		if strings.Contains(h, w) {
			return true
		}
	}
	// Market path segments: /nz/, /en-nz/, /de-de/ in the first two components.
	segs := strings.Split(strings.Trim(strings.ToLower(u.Path), "/"), "/")
	for i, s := range segs {
		if i >= 2 {
			break
		}
		cc := s
		if p := strings.SplitN(s, "-", 2); len(p) == 2 && len(p[1]) == 2 {
			cc = p[1] // locale pair like en-nz
		}
		if len(cc) == 2 && countryCodes[cc] && cc != region {
			return true
		}
	}
	return false
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

// isListingHostedDoc flags docs hosted on marketplace/listing CDNs and
// anonymous-upload/doc-spam farms — re-hosted copies of real manuals with no
// provenance (observed live: uploads.strikinglycdn.com with a gibberish
// filename outranking the manufacturer's own PDF).
func isListingHostedDoc(u string) bool {
	l := strings.ToLower(u)
	for _, bad := range []string{
		"media-amazon.com", "images-amazon.com", "ssl-images-amazon", "ebayimg", "aliexpress",
		"strikinglycdn", "docdroid", "pdfcoffee", "idoc.pub", "kupdf", "vdocuments",
		"cupdf", "edoc.site", "dokumen.pub", "vsip.info", "fdocuments",
		".weebly.com", ".wixsite.com", "godaddysites.com", "yumpu.com", "scribd",
	} {
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
