// Package enrich auto-completes item identity metadata (manufacturer /
// modelNumber / name / category) with corroborated confidence.
//
// Trust model (docs/decisions.md D13–D16): fill-only (never overwrite human
// data), per-field gating, and a write requires corroboration — the value must
// appear on >= MinAgreeingSources independent domains AND the back-check
// round-trip must pass. LLM self-scores alone never authorize a write.
package enrich

import (
	"context"
	"log"
	"net/url"
	"regexp"
	"strings"

	"github.com/joseolivieri/homebox-docfetch/internal/llm"
)

// Searcher provides raw search results (satisfied by discovery.Engine via a
// small adapter, or a fake in tests).
type Searcher interface {
	Search(ctx context.Context, query string) ([]SearchResult, error)
}

type SearchResult struct {
	Title   string
	URL     string
	Snippet string
}

// Extractor is satisfied by *llm.Client.
type Extractor interface {
	ExtractIdentity(ctx context.Context, itemDesc string, cands []llm.Candidate) (*llm.Identity, error)
}

type Options struct {
	Enabled            bool
	FillOnly           bool
	AutoWriteThreshold float64
	MinAgreeingSources int
	BackCheck          bool
	Fields             []string
	MaxSnippetChars    int
}

// Item is the current (possibly gappy) identity of a Homebox entity.
type Item struct {
	Manufacturer string
	ModelNumber  string
	Name         string
}

// FieldResult is the outcome for one field.
type FieldResult struct {
	Field      string
	Value      string
	Confidence float64
	Evidence   []string // supporting domains/urls
	Action     Action
}

type Action int

const (
	ActionSkip Action = iota
	ActionWrite
	ActionReview
)

type Engine struct {
	opt  Options
	srch Searcher
	llm  Extractor
}

func New(opt Options, s Searcher, x Extractor) *Engine {
	if opt.AutoWriteThreshold == 0 {
		opt.AutoWriteThreshold = 0.85
	}
	if opt.MinAgreeingSources == 0 {
		opt.MinAgreeingSources = 2
	}
	if len(opt.Fields) == 0 {
		opt.Fields = []string{"manufacturer", "modelNumber", "name", "category"}
	}
	if opt.MaxSnippetChars == 0 {
		opt.MaxSnippetChars = 150
	}
	return &Engine{opt: opt, srch: s, llm: x}
}

// gaps returns which configured fields are empty on the item.
func (e *Engine) gaps(it Item) []string {
	var out []string
	for _, f := range e.opt.Fields {
		switch f {
		case "manufacturer":
			if strings.TrimSpace(it.Manufacturer) == "" {
				out = append(out, f)
			}
		case "modelNumber":
			if strings.TrimSpace(it.ModelNumber) == "" {
				out = append(out, f)
			}
		case "name":
			if strings.TrimSpace(it.Name) == "" {
				out = append(out, f)
			}
		case "category":
			out = append(out, f) // category is a tag; presence checked by caller
		}
	}
	return out
}

// Enrich infers missing fields for the item. Returns per-field results; only
// fields with ActionWrite should be persisted by the caller.
func (e *Engine) Enrich(ctx context.Context, it Item, hasCategoryTag bool) ([]FieldResult, error) {
	if !e.opt.Enabled {
		return nil, nil
	}
	gaps := e.gaps(it)
	if hasCategoryTag {
		gaps = remove(gaps, "category")
	}
	if len(gaps) == 0 {
		return nil, nil
	}

	// Search on the strongest available key.
	key := strings.TrimSpace(it.ModelNumber)
	if key == "" {
		key = strings.TrimSpace(strings.TrimSpace(it.Manufacturer) + " " + it.Name)
	}
	if key == "" {
		return nil, nil
	}
	results, err := e.srch.Search(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}

	cands := make([]llm.Candidate, 0, len(results))
	for _, r := range results {
		cands = append(cands, llm.Candidate{Title: r.Title, URL: r.URL, Snippet: truncate(r.Snippet, e.opt.MaxSnippetChars)})
	}
	id, err := e.llm.ExtractIdentity(ctx, key, cands)
	if err != nil {
		return nil, err
	}

	extracted := map[string]string{
		"manufacturer": strings.TrimSpace(id.Manufacturer),
		"modelNumber":  strings.TrimSpace(id.ModelNumber),
		"name":         strings.TrimSpace(id.Name),
		"category":     strings.TrimSpace(strings.ToLower(id.Category)),
	}

	// Back-check once for the whole identity: search the inferred mfr+model and
	// require the original key's tokens to appear in the results.
	backOK := true
	if e.opt.BackCheck && extracted["manufacturer"] != "" && extracted["modelNumber"] != "" {
		backOK = e.backCheck(ctx, extracted["manufacturer"], extracted["modelNumber"], it)
	}

	var out []FieldResult
	for _, f := range gaps {
		val := extracted[f]
		if val == "" {
			continue
		}
		if f == "modelNumber" && !modelSane(val) {
			log.Printf("enrich: rejecting modelNumber %q (fails format sanity)", val)
			continue
		}
		agree := agreeingDomains(val, results)
		conf := id.Confidence[f]
		fr := FieldResult{Field: f, Value: val, Confidence: conf, Evidence: agree}

		switch {
		// category is a tag, human-reviewable in bulk; corroboration by domain
		// text rarely applies — gate on threshold + backOK only.
		case f == "category" && conf >= e.opt.AutoWriteThreshold && backOK:
			fr.Action = ActionWrite
		case f != "category" && len(agree) >= e.opt.MinAgreeingSources && backOK && conf >= e.opt.AutoWriteThreshold:
			fr.Action = ActionWrite
		case conf >= 0.5:
			fr.Action = ActionReview
		default:
			fr.Action = ActionSkip
		}
		out = append(out, fr)
	}
	return out, nil
}

// backCheck searches the inferred identity and verifies the original item is
// what comes back (round-trip consistency).
func (e *Engine) backCheck(ctx context.Context, mfr, model string, it Item) bool {
	results, err := e.srch.Search(ctx, mfr+" "+model)
	if err != nil || len(results) == 0 {
		return false
	}
	// Original key tokens (name or model) must appear in the round-trip corpus.
	probe := norm(it.Name)
	if strings.TrimSpace(it.ModelNumber) != "" {
		probe = norm(it.ModelNumber)
	}
	if probe == "" {
		return false
	}
	var corpus strings.Builder
	for _, r := range results {
		corpus.WriteString(norm(r.Title))
		corpus.WriteString(norm(r.Snippet))
		corpus.WriteString(norm(r.URL))
	}
	return strings.Contains(corpus.String(), probe)
}

// agreeingDomains returns the distinct registrable-ish domains whose
// title/url/snippet contain the value.
func agreeingDomains(value string, results []SearchResult) []string {
	nv := norm(value)
	if nv == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range results {
		hay := norm(r.Title) + norm(r.Snippet) + norm(r.URL)
		if !strings.Contains(hay, nv) {
			continue
		}
		d := domain(r.URL)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func norm(s string) string { return nonAlnum.ReplaceAllString(strings.ToLower(s), "") }

func domain(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	h := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	// collapse subdomains to the last two labels — good enough for agreement counting
	parts := strings.Split(h, ".")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, ".")
}

var (
	hasDigit  = regexp.MustCompile(`\d`)
	hasLetter = regexp.MustCompile(`[a-zA-Z]`)
)

// modelSane rejects marketing phrases and marketing numbers masquerading as
// model numbers. Real part numbers mix letters and digits (A1289, WH-1000XM5);
// a short bare number ("737") is a product line, not a SKU — accepting one
// poisons doc verification downstream (observed live: enrichment wrote
// modelNumber=737 for the Anker 737 Power Bank, whose SKU is A1289).
func modelSane(v string) bool {
	if len(v) < 2 || len(v) > 40 {
		return false
	}
	if strings.Count(v, " ") > 2 {
		return false
	}
	if !hasDigit.MatchString(v) && !strings.Contains(v, "-") {
		return false
	}
	if !hasLetter.MatchString(v) && len(v) < 6 {
		return false // short all-digit string = marketing number
	}
	return true
}

func remove(ss []string, v string) []string {
	out := ss[:0]
	for _, s := range ss {
		if s != v {
			out = append(out, s)
		}
	}
	return out
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
