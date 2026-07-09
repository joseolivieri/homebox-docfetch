// Package scheduler wires the pieces into runnable jobs: scan (new items),
// followup (re-check known items), and reconcile (weekly "awaiting review"
// digest). All Homebox / discovery / ntfy access goes through small interfaces
// so the scan decision logic is unit-testable with fakes.
package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/discovery"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/homebox"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/notes"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/notify"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/store"
)

// EntityAPI is the subset of the Homebox client the scanner needs.
type EntityAPI interface {
	ListEntities(ctx context.Context, page, pageSize int, tagIDs []string) (*homebox.EntityListResult, error)
	GetEntity(ctx context.Context, id string) (*homebox.EntityOut, error)
	PatchEntity(ctx context.Context, id string, in homebox.EntityUpdate) (*homebox.EntityOut, error)
	PutEntity(ctx context.Context, id string, in homebox.EntityUpdate) (*homebox.EntityOut, error)
	UploadAttachment(ctx context.Context, id, filename, attType string, primary bool, r io.Reader) (*homebox.EntityOut, error)
	DownloadAttachment(ctx context.Context, entityID, attachmentID string, maxBytes int64) ([]byte, string, error)
	EnsureTag(ctx context.Context, name string) (string, error)
}

// Discoverer runs the search pipeline and downloads a chosen doc.
type Discoverer interface {
	Discover(ctx context.Context, it discovery.Item, want []string) (*discovery.Result, error)
	SelectClass(ctx context.Context, it discovery.Item, cands []discovery.Candidate, class string) *discovery.Result
	Download(ctx context.Context, url string, maxBytes int64) ([]byte, error)
	Skim(ctx context.Context, it discovery.Item, data []byte, wantClass string) discovery.SkimVerdict
}

// DocClassCfg is one fetchable document class for the scanner: label, Homebox
// attachment type, and the category gate that limits it to relevant items.
type DocClassCfg struct {
	Name       string
	Field      string
	AttachAs   string
	Categories []string
	Enabled    bool
}

// Notifier sends ntfy messages.
type Notifier interface {
	Send(ctx context.Context, m notify.Message) error
}

// Config holds the scanner's behavioural knobs (from the property file).
type Config struct {
	PageSize            int
	SkipIfExists        bool // per doc class: skip a class already present on the item
	AutoAttachThreshold float64
	MaxPDFBytes         int64
	FollowupAfter       time.Duration
	BackoffBase         time.Duration
	UnverifiedTag       string
	HomeboxURL          string
	PortalURL           string // public portal base; enables ntfy approve buttons
	SignKey             string // HMAC key for approve links (the Homebox token)

	// Per-provider toggles (docs/photo/warranty; enrich toggles via SetEnricher).
	DocsEnabled        bool
	DocClasses         []DocClassCfg // fetchable document classes (manual primary)
	PhotoEnabled       bool
	PhotoMinConfidence float64
	WarrantyEnabled    bool
	AuditLog           bool
}

// manualClass returns the primary "manual" class config (synthesized if the
// operator somehow omitted it — the whole pipeline is manual-centric).
func (c Config) manualClass() DocClassCfg {
	for _, dc := range c.DocClasses {
		if dc.Name == "manual" {
			return dc
		}
	}
	return DocClassCfg{Name: "manual", Field: "Manual", AttachAs: "manual", Enabled: true}
}

type Scanner struct {
	api      EntityAPI
	disc     Discoverer
	ntfy     Notifier
	store    *store.Store
	cfg      Config
	enricher Enricher // nil = enrichment disabled

	// Curation extras (nil = disabled): web search + vision surfaces.
	curSearch   CurationSearch
	vision      Vision
	visionModel string

	unverifiedTagID string
}

func NewScanner(api EntityAPI, disc Discoverer, n Notifier, st *store.Store, cfg Config) *Scanner {
	if cfg.PageSize == 0 {
		cfg.PageSize = 100
	}
	if len(cfg.DocClasses) == 0 {
		cfg.DocClasses = []DocClassCfg{{Name: "manual", Field: "Manual", AttachAs: "manual", Enabled: true}}
	}
	if cfg.BackoffBase == 0 {
		cfg.BackoffBase = 24 * time.Hour
	}
	return &Scanner{api: api, disc: disc, ntfy: n, store: st, cfg: cfg}
}

// SetEnricher enables metadata enrichment (Phase 1.5).
func (s *Scanner) SetEnricher(e Enricher) { s.enricher = e }

// bootstrap resolves the unverified tag id once.
func (s *Scanner) bootstrap(ctx context.Context) error {
	if s.unverifiedTagID != "" {
		return nil
	}
	id, err := s.api.EnsureTag(ctx, s.cfg.UnverifiedTag)
	if err != nil {
		return fmt.Errorf("ensure unverified tag: %w", err)
	}
	s.unverifiedTagID = id
	return nil
}

// Scan pages through the whole collection. followup=true forces re-evaluation
// of items past their FollowupAfter window.
func (s *Scanner) Scan(ctx context.Context, followup bool) error {
	if err := s.bootstrap(ctx); err != nil {
		return err
	}
	for page := 1; ; page++ {
		list, err := s.api.ListEntities(ctx, page, s.cfg.PageSize, nil)
		if err != nil {
			return err
		}
		for i := range list.Items {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := s.process(ctx, &list.Items[i], followup); err != nil {
				log.Printf("error processing %q (%s): %v", list.Items[i].Name, list.Items[i].ID, err)
				s.recordError(ctx, &list.Items[i], err)
			}
		}
		if page*s.cfg.PageSize >= list.Total || len(list.Items) == 0 {
			return nil
		}
	}
}

// changeSignal returns a cheap fingerprint of the collection: item total plus
// the newest updatedAt on the first page. Any add/delete/edit shifts it.
func (s *Scanner) changeSignal(ctx context.Context) (string, error) {
	list, err := s.api.ListEntities(ctx, 1, 50, nil)
	if err != nil {
		return "", err
	}
	newest := time.Time{}
	for _, it := range list.Items {
		if it.UpdatedAt.After(newest) {
			newest = it.UpdatedAt
		}
	}
	return fmt.Sprintf("%d|%s", list.Total, newest.UTC().Format(time.RFC3339Nano)), nil
}

// ProcessEntity runs the full per-item pipeline (enrich -> doc fetch/gate) for
// one entity id, immediately. Used by the portal after intake so a freshly
// created item is enriched and documented without waiting for the next tick.
func (s *Scanner) ProcessEntity(ctx context.Context, id string) error {
	if err := s.bootstrap(ctx); err != nil {
		return err
	}
	detail, err := s.api.GetEntity(ctx, id)
	if err != nil {
		return err
	}
	sum := &homebox.EntitySummary{ID: detail.ID, Name: detail.Name, UpdatedAt: detail.UpdatedAt}
	if err := s.process(ctx, sum, false); err != nil {
		log.Printf("error processing %q (%s): %v", sum.Name, sum.ID, err)
		s.recordError(ctx, sum, err)
		return err
	}
	return nil
}

func (s *Scanner) process(ctx context.Context, sum *homebox.EntitySummary, followup bool) error {
	now := time.Now()
	rec, err := s.store.Get(ctx, sum.ID)
	if err != nil {
		return err
	}
	updatedAt := sum.UpdatedAt.UTC().Format(time.RFC3339)
	changed := rec != nil && rec.UpdatedAt != updatedAt

	if rec != nil && !changed && !s.shouldReprocess(rec, followup, now) {
		return nil
	}

	detail, err := s.api.GetEntity(ctx, sum.ID)
	if err != nil {
		return err
	}

	base := &store.Record{
		EntityID:    sum.ID,
		Name:        detail.Name,
		MetaHash:    store.MetaHash(detail.Manufacturer, detail.ModelNumber, detail.Name),
		UpdatedAt:   updatedAt,
		FirstSeen:   firstSeen(rec, now),
		LastChecked: now,
	}
	if rec != nil {
		base.Attempts = rec.Attempts
	}

	// Enrich first — independent of doc state: an item with a manual can still
	// lack metadata, and a filled model# upgrades the doc search below.
	if enriched, err := s.enrichEntity(ctx, detail); err != nil {
		log.Printf("enrich failed for %q (continuing to doc-fetch): %v", detail.Name, err)
	} else {
		detail = enriched
		base.MetaHash = store.MetaHash(detail.Manufacturer, detail.ModelNumber, detail.Name)
	}

	docErr := s.processDocs(ctx, detail, rec, base)

	// Curation extras — independent of the doc outcome.
	s.curatePhoto(ctx, detail)
	s.curateWarranty(ctx, detail)
	return docErr
}

// processDocs is the doc-fetch phase. It fetches every enabled+applicable doc
// CLASS (manual, parts, …) independently: each class selects its own best
// candidate, so a parts list can no longer win the manual's slot (the bug that
// gave a dishwasher a parts PDF instead of its manual). The manual class is
// primary — it drives the entity's store status and is the only class that
// review-gates via ntfy; secondary classes attach when confident, else skip.
func (s *Scanner) processDocs(ctx context.Context, detail *homebox.EntityOut, rec, base *store.Record) error {
	if !s.cfg.DocsEnabled {
		base.Status = store.StatusNotFound
		return s.store.Upsert(ctx, base)
	}

	manual := s.cfg.manualClass()

	// One-tap approval queued via the notes block (the portal makes no web
	// calls — the Attach button just writes "approved [pdf](url)"). Fulfil it
	// here: download + attach the exact URL the human approved, no discovery.
	if approved := notes.ApprovedURLs(detail.Notes); len(approved) > 0 {
		return s.attachApproved(ctx, detail, approved[len(approved)-1], base, manual)
	}

	item := discovery.Item{
		Manufacturer: detail.Manufacturer,
		ModelNumber:  detail.ModelNumber,
		Name:         detail.Name,
		// Label QR links recorded at intake (or hand-added "- qr <url>" lines):
		// the qr pipeline stage follows these before any searching.
		HintURLs: notes.QRURLs(detail.Notes),
	}
	if strings.TrimSpace(item.Manufacturer) == "" && strings.TrimSpace(item.ModelNumber) == "" && strings.TrimSpace(item.Name) == "" {
		log.Printf("skip %q — no searchable identity", detail.Name)
		base.Status = store.StatusNotFound
		base.Attempts++
		return s.store.Upsert(ctx, base)
	}

	// Applicable classes (enabled ∧ category gate) not already satisfied.
	var pending []DocClassCfg
	for _, dc := range s.targetClasses(detail) {
		if s.cfg.SkipIfExists && hasDoc(detail, dc) {
			continue
		}
		pending = append(pending, dc)
	}
	if len(pending) == 0 {
		// Nothing to fetch: either all present, or none applies to this item.
		if hasDoc(detail, manual) {
			base.Status = store.StatusAttached
		} else {
			base.Status = store.StatusNotFound
		}
		return s.store.Upsert(ctx, base)
	}

	// Rejected URLs (ntfy Reject / hand-written "rejected" lines) are permanent
	// negative labels: ingest new ones and strip them from this run.
	rejected := s.rejectedSet(ctx, detail)

	res, err := s.disc.Discover(ctx, item, classNames(pending))
	if err != nil {
		return err
	}
	base.Attempts++
	if res == nil {
		res = &discovery.Result{}
	}
	filterRejected(res, rejected)

	// Secondary classes first (attach/link side-effects only; no ntfy, no store
	// status), then the manual class drives base status + return.
	for _, dc := range pending {
		if dc.Name == manual.Name {
			continue
		}
		cres := s.disc.SelectClass(ctx, item, res.Candidates, dc.Name)
		s.fetchSecondary(ctx, detail, item, cres, dc)
	}

	if !containsClass(pending, manual.Name) {
		// Manual already present; only secondaries were pending.
		base.Status = store.StatusAttached
		return s.store.Upsert(ctx, base)
	}
	cres := s.disc.SelectClass(ctx, item, res.Candidates, manual.Name)
	return s.resolveManual(ctx, detail, item, cres, rec, base)
}

// skimConfidence is assigned when content skimming (not URL heuristics)
// confirms the doc covers the item's model.
const skimConfidence = 0.85

// resolveManual runs the primary-class decision ladder: attach / official
// page-follow / skim-promote / link / review-gate / notfound. Owns the store
// record.
func (s *Scanner) resolveManual(ctx context.Context, detail *homebox.EntityOut, item discovery.Item, res *discovery.Result, rec, base *store.Record) error {
	dc := s.cfg.manualClass()
	// Official-first: SEO re-host farms put the model number in their titles
	// and URLs while manufacturers use internal doc numbers, so every
	// model-token heuristic systematically favors spam. When an official PDF
	// exists but didn't win the pick, content-read it first — a non-official
	// source never beats an official one that skim-confirms.
	if cand, data := s.officialFirst(ctx, item, res, dc); cand != nil {
		res.Best, res.Confidence = cand, skimConfidence
		return s.attach(ctx, detail, item, res, dc, rec, base, data)
	}
	switch {
	case res.Best != nil && !res.Best.IsHTML && res.Confidence >= s.cfg.AutoAttachThreshold:
		log.Printf("attach %q [%s] — conf=%.2f llm=%v url=%s", detail.Name, dc.Name, res.Confidence, res.UsedLLM, res.Best.URL)
		return s.attach(ctx, detail, item, res, dc, rec, base, nil)
	case res.BestHTML != nil:
		if op := bestOfficialPDF(res.Candidates); op != nil {
			log.Printf("attach %q [%s] — official page-follow pdf %s", detail.Name, dc.Name, op.URL)
			res.Best = op
			return s.attach(ctx, detail, item, res, dc, rec, base, nil)
		}
		if cand, data := s.skimPromote(ctx, item, res, dc); cand != nil {
			res.Best, res.Confidence = cand, skimConfidence
			return s.attach(ctx, detail, item, res, dc, rec, base, data)
		}
		return s.linkManual(ctx, detail, res, dc, base)
	case res.Best != nil:
		// URL heuristics couldn't confirm it — but the document itself may
		// name the model (manufacturer URLs use internal doc numbers). Skim
		// the top candidates before bothering the human.
		if cand, data := s.skimPromote(ctx, item, res, dc); cand != nil {
			res.Best, res.Confidence = cand, skimConfidence
			return s.attach(ctx, detail, item, res, dc, rec, base, data)
		}
		log.Printf("review-gate %q [%s] — conf=%.2f (below %.2f) url=%s", detail.Name, dc.Name, res.Confidence, s.cfg.AutoAttachThreshold, res.Best.URL)
		return s.reviewGate(ctx, detail, res, base)
	default:
		log.Printf("no %s found for %q (candidates=%d)", dc.Name, detail.Name, len(res.Candidates))
		s.recordDecision(ctx, detail, res, dc, "notfound", "")
		base.Status = store.StatusNotFound
		return s.store.Upsert(ctx, base)
	}
}

// fetchSecondary attaches (or links) a non-manual class when the pipeline is
// confident, else records a notfound in the ledger. No ntfy prompt and no
// store-status side effect — secondary docs are a bonus, not a gate.
func (s *Scanner) fetchSecondary(ctx context.Context, detail *homebox.EntityOut, item discovery.Item, res *discovery.Result, dc DocClassCfg) {
	if cand, data := s.officialFirst(ctx, item, res, dc); cand != nil {
		res.Best, res.Confidence = cand, skimConfidence
		if err := s.attach(ctx, detail, item, res, dc, nil, nil, data); err != nil {
			log.Printf("secondary attach %q [%s]: %v", detail.Name, dc.Name, err)
		}
		return
	}
	if res.Best != nil && !res.Best.IsHTML && res.Confidence >= s.cfg.AutoAttachThreshold {
		if err := s.attach(ctx, detail, item, res, dc, nil, nil, nil); err != nil {
			log.Printf("secondary attach %q [%s]: %v", detail.Name, dc.Name, err)
		}
		return
	}
	if res.BestHTML != nil {
		if op := bestOfficialPDF(res.Candidates); op != nil {
			res.Best = op
			if err := s.attach(ctx, detail, item, res, dc, nil, nil, nil); err != nil {
				log.Printf("secondary attach %q [%s]: %v", detail.Name, dc.Name, err)
			}
			return
		}
	}
	if cand, data := s.skimPromote(ctx, item, res, dc); cand != nil {
		res.Best, res.Confidence = cand, skimConfidence
		if err := s.attach(ctx, detail, item, res, dc, nil, nil, data); err != nil {
			log.Printf("secondary attach %q [%s]: %v", detail.Name, dc.Name, err)
		}
		return
	}
	if res.BestHTML != nil {
		if err := s.linkManual(ctx, detail, res, dc, nil); err != nil {
			log.Printf("secondary link %q [%s]: %v", detail.Name, dc.Name, err)
		}
		return
	}
	s.recordDecision(ctx, detail, res, dc, "notfound", "")
}

// officialFirst content-reads the best official PDF candidate when the pick
// (if any) is non-official. Returns the candidate + bytes on skim
// confirmation. When the winning pick is already official, this is a no-op —
// the normal ladder handles it.
func (s *Scanner) officialFirst(ctx context.Context, item discovery.Item, res *discovery.Result, dc DocClassCfg) (*discovery.Candidate, []byte) {
	if res.Best != nil && res.Best.Official {
		return nil, nil
	}
	off := bestOfficialPDF(res.Candidates)
	if off == nil {
		return nil, nil
	}
	data, err := s.disc.Download(ctx, off.URL, s.cfg.MaxPDFBytes)
	if err != nil {
		log.Printf("official-first download failed (%s): %v", off.URL, err)
		return nil, nil
	}
	v := s.disc.Skim(ctx, item, data, dc.Name)
	// Official provenance + readable PDF of the right class is enough when the
	// item has no model number to confirm; with a model number, require it.
	confirmed := v.IsPDF && !v.ProductMismatch && !v.ClassMismatch &&
		(v.ModelConfirmed || strings.TrimSpace(item.ModelNumber) == "" || !v.HasText)
	if !confirmed {
		return nil, nil
	}
	log.Printf("official-first %q [%s]: official source confirmed — %s", item.Name, dc.Name, off.URL)
	return off, data
}

// skimPromote downloads the class's top-scored PDF candidates and reads their
// opening pages: a document whose text names the item's model number is
// attach-worthy regardless of what its URL looks like. Capped at 2 downloads
// per item per pass. Returns the winning candidate and its bytes (reused for
// the attach — no second download).
func (s *Scanner) skimPromote(ctx context.Context, item discovery.Item, res *discovery.Result, dc DocClassCfg) (*discovery.Candidate, []byte) {
	if strings.TrimSpace(item.ModelNumber) == "" {
		return nil, nil // nothing to confirm against
	}
	tried := 0
	for _, c := range topPDFCandidates(res, 2) {
		tried++
		data, err := s.disc.Download(ctx, c.URL, s.cfg.MaxPDFBytes)
		if err != nil {
			log.Printf("skim-promote download failed (%s): %v", c.URL, err)
			continue
		}
		v := s.disc.Skim(ctx, item, data, dc.Name)
		if v.IsPDF && v.ModelConfirmed && !v.ProductMismatch && !v.ClassMismatch {
			log.Printf("skim-promote %q [%s]: document text names the model — %s", item.Name, dc.Name, c.URL)
			return c, data
		}
	}
	_ = tried
	return nil, nil
}

// topPDFCandidates returns up to max PDF candidates by descending score,
// starting with res.Best when it is a PDF.
func topPDFCandidates(res *discovery.Result, max int) []*discovery.Candidate {
	var out []*discovery.Candidate
	if res.Best != nil && res.Best.IsPDF {
		out = append(out, res.Best)
	}
	idx := make([]int, 0, len(res.Candidates))
	for i := range res.Candidates {
		c := &res.Candidates[i]
		if c.IsPDF && c.Score > 0 && (res.Best == nil || c.URL != res.Best.URL) {
			idx = append(idx, i)
		}
	}
	sort.Slice(idx, func(a, b int) bool { return res.Candidates[idx[a]].Score > res.Candidates[idx[b]].Score })
	for _, i := range idx {
		if len(out) >= max {
			break
		}
		out = append(out, &res.Candidates[i])
	}
	if len(out) > max {
		out = out[:max]
	}
	return out
}

// targetClasses returns the enabled doc classes applicable to this item after
// the per-class category gate (empty categories = all items).
func (s *Scanner) targetClasses(detail *homebox.EntityOut) []DocClassCfg {
	var out []DocClassCfg
	for _, dc := range s.cfg.DocClasses {
		if !dc.Enabled || !categoryMatch(detail, dc.Categories) {
			continue
		}
		out = append(out, dc)
	}
	return out
}

// categoryMatch reports whether the item satisfies a class's category gate.
// No categories configured => the class applies to everything. A category
// matches against the item's tags AND its name/type — enrichment doesn't
// always tag a category, but "Whirlpool WDF520PADM7 Dishwasher" still names it.
func categoryMatch(detail *homebox.EntityOut, cats []string) bool {
	if len(cats) == 0 {
		return true
	}
	hay := strings.ToLower(detail.Name)
	for _, t := range detail.Tags {
		hay += " " + strings.ToLower(t.Name)
	}
	for _, c := range cats {
		if c = strings.ToLower(strings.TrimSpace(c)); c != "" && strings.Contains(hay, c) {
			return true
		}
	}
	return false
}

func classNames(cs []DocClassCfg) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func containsClass(cs []DocClassCfg, name string) bool {
	for _, c := range cs {
		if c.Name == name {
			return true
		}
	}
	return false
}

// rejectedSet unions ledger rejections with "rejected" lines in the entity's
// notes block (the reject button writes there — Homebox is the shared bus
// between the portal and scheduler processes), ingesting new notes rejections
// as ledger labels on first sight.
func (s *Scanner) rejectedSet(ctx context.Context, detail *homebox.EntityOut) map[string]bool {
	set, err := s.store.RejectedURLs(ctx, detail.ID, s.cfg.manualClass().Name)
	if err != nil {
		log.Printf("ledger rejected urls %s: %v", detail.ID, err)
		set = map[string]bool{}
	}
	for _, u := range notes.RejectedURLs(detail.Notes) {
		if set[u] {
			continue
		}
		if n, err := s.store.LabelDecisions(ctx, detail.ID, u, store.LabelRejected, "ntfy"); err != nil {
			log.Printf("ledger label %s: %v", detail.ID, err)
		} else if n == 0 {
			// No proposal row to label (hand-written rejection): synthesize one so
			// the URL still counts as a negative and stays filtered.
			_ = s.store.RecordDecision(ctx, &store.Decision{
				EntityID: detail.ID, EntityName: detail.Name, DocClass: s.cfg.manualClass().Name,
				ChosenURL: u, Outcome: "review", Label: store.LabelRejected, LabelSrc: "manual",
			})
		}
		set[u] = true
	}
	return set
}

// filterRejected strips rejected URLs from a discovery result in place.
func filterRejected(res *discovery.Result, rejected map[string]bool) {
	if res == nil || len(rejected) == 0 {
		return
	}
	if res.Best != nil && rejected[res.Best.URL] {
		res.Best = nil
	}
	if res.BestHTML != nil && rejected[res.BestHTML.URL] {
		res.BestHTML = nil
	}
	kept := res.Candidates[:0]
	for _, c := range res.Candidates {
		if !rejected[c.URL] {
			kept = append(kept, c)
		}
	}
	res.Candidates = kept
}

// candLite is the compact per-candidate shape stored in the ledger.
type candLite struct {
	URL      string  `json:"u"`
	Score    float64 `json:"s"`
	Official bool    `json:"o,omitempty"`
	PDF      bool    `json:"p,omitempty"`
	HTML     bool    `json:"h,omitempty"`
	Model    bool    `json:"m,omitempty"`
}

// recordDecision appends the pipeline's verdict for this entity to the
// learning ledger. chosenURL overrides the result's pick (fallback downloads
// can land on a different candidate than res.Best).
func (s *Scanner) recordDecision(ctx context.Context, detail *homebox.EntityOut, res *discovery.Result, dc DocClassCfg, outcome, chosenURL string) {
	d := &store.Decision{
		EntityID:   detail.ID,
		EntityName: detail.Name,
		DocClass:   dc.Name,
		Outcome:    outcome,
		ChosenURL:  chosenURL,
	}
	if res != nil {
		d.Stage = res.Stage
		d.Confidence = res.Confidence
		d.UsedLLM = res.UsedLLM
		if d.ChosenURL == "" {
			if res.Best != nil {
				d.ChosenURL = res.Best.URL
			} else if res.BestHTML != nil {
				d.ChosenURL = res.BestHTML.URL
			}
		}
		lite := make([]candLite, 0, len(res.Candidates))
		for i, c := range res.Candidates {
			if i >= 12 {
				break
			}
			lite = append(lite, candLite{URL: c.URL, Score: c.Score, Official: c.Official,
				PDF: c.IsPDF, HTML: c.IsHTML, Model: c.ModelMatch})
		}
		if b, err := json.Marshal(lite); err == nil {
			d.Candidates = string(b)
		}
	}
	if err := s.store.RecordDecision(ctx, d); err != nil {
		log.Printf("ledger record %s: %v", detail.ID, err)
	}
}

// linkManual records online doc sources in custom fields — "<Field>" for a
// remote PDF, "<Field> (web)" for an HTML support page. Homebox auto-links a
// single URL per text field. No file is attached. base==nil marks a secondary
// class (no store status, no review-gate fallback).
func (s *Scanner) linkManual(ctx context.Context, detail *homebox.EntityOut, res *discovery.Result, dc DocClassCfg, base *store.Record) error {
	fresh, err := s.api.GetEntity(ctx, detail.ID)
	if err != nil {
		return err
	}
	upd := fullUpdateFrom(fresh)
	var links []string
	docURL := ""
	if res.Best != nil && res.Best.IsPDF {
		upd.Fields = homebox.UpsertField(upd.Fields, dc.Field, notes.MDLink("pdf", res.Best.URL))
		links = append(links, notes.MDLink("pdf", res.Best.URL))
		docURL = res.Best.URL
	}
	if res.BestHTML != nil {
		upd.Fields = homebox.UpsertField(upd.Fields, dc.Field+" (web)", notes.MDLink("web", res.BestHTML.URL))
		links = append(links, notes.MDLink("web", res.BestHTML.URL))
		if docURL == "" {
			docURL = res.BestHTML.URL
		}
	}
	if docURL == "" {
		if base == nil {
			s.recordDecision(ctx, detail, res, dc, "notfound", "")
			return nil
		}
		return s.reviewGate(ctx, detail, res, base)
	}
	n := notes.Append(fresh.Notes, notes.Line(dc.Name+" linked "+strings.Join(links, " ")))
	upd.Notes = &n
	if _, err := s.api.PutEntity(ctx, detail.ID, upd); err != nil {
		return err
	}
	log.Printf("%s linked for %q — %s", dc.Name, detail.Name, docURL)
	s.recordDecision(ctx, detail, res, dc, "linked", docURL)
	if base != nil {
		t := time.Now()
		base.Status = store.StatusAttached
		base.DocURL = docURL
		base.LastAttached = &t
		return s.store.Upsert(ctx, base)
	}
	return nil
}

// attach downloads (or reuses preloaded skim bytes), dedupes by content hash,
// and uploads the doc under its class's Homebox attachment type. On
// download/verify failure it falls back to the next-best scored PDF
// candidates. base/rec are nil for secondary classes (no store record, no
// dedup — the field-presence gate prevents re-attach).
func (s *Scanner) attach(ctx context.Context, detail *homebox.EntityOut, item discovery.Item, res *discovery.Result, dc DocClassCfg, rec, base *store.Record, preloaded []byte) error {
	best := res.Best
	data := preloaded // skim-promote already downloaded AND content-verified these bytes
	if data == nil {
		var err error
		data, err = s.disc.Download(ctx, best.URL, s.cfg.MaxPDFBytes)
		if err != nil {
			log.Printf("download failed for %q [%s] (%v); trying fallback candidates", detail.Name, dc.Name, err)
			best, data = s.downloadFallback(ctx, res, item, dc)
			if best == nil {
				// Nothing fetchable server-side (bot-walled hosts): link the best
				// remote sources instead — a phone browser passes the bot wall.
				return s.linkManual(ctx, detail, res, dc, base)
			}
		} else if !skimAccepts(s.disc.Skim(ctx, item, data, dc.Name), best) {
			// Official brand-domain docs skip the different-product veto
			// (provenance beats a sparse-excerpt LLM read; observed
			// false-negatives on image-heavy official manuals) but NOT the
			// PDF-magic or wrong-class checks — an official parts list must
			// still not attach as the manual.
			log.Printf("content skim rejected %q [%s] (%s); trying fallback candidates", detail.Name, dc.Name, best.URL)
			best, data = s.downloadFallback(ctx, res, item, dc)
			if best == nil {
				res.Best = nil
				return s.linkManual(ctx, detail, res, dc, base)
			}
		}
	}
	sha := store.DocSHA(data)

	if rec != nil && rec.DocSHA256 == sha {
		// Identical doc already attached previously; do not re-upload.
		base.DocURL = best.URL
		base.DocSHA256 = sha
		base.Status = store.StatusAttached
		base.LastAttached = rec.LastAttached
		return s.store.Upsert(ctx, base)
	}

	updated, err := s.api.UploadAttachment(ctx, detail.ID, filename(detail, dc), dc.AttachAs, false, bytes.NewReader(data))
	if err != nil {
		return err
	}
	// Log the attach + link the source in custom fields (Homebox auto-links a
	// single URL per field).
	if updated != nil && updated.ID != "" {
		upd := fullUpdateFrom(updated)
		line := fmt.Sprintf("%s attached (%.2f) %s", dc.Name, res.Confidence, notes.MDLink("pdf", best.URL))
		n := notes.Append(updated.Notes, notes.Line(line))
		upd.Notes = &n
		upd.Fields = homebox.UpsertField(upd.Fields, dc.Field, notes.MDLink("pdf", best.URL))
		if res.BestHTML != nil && res.BestHTML.Official {
			upd.Fields = homebox.UpsertField(upd.Fields, dc.Field+" (web)", notes.MDLink("web", res.BestHTML.URL))
		}
		if _, err := s.api.PutEntity(ctx, detail.ID, upd); err != nil {
			log.Printf("attach note put %s: %v", detail.ID, err)
		}
	}
	s.recordDecision(ctx, detail, res, dc, "attached", best.URL)
	if base == nil {
		return nil // secondary class: no store record to update
	}
	t := time.Now()
	base.DocURL = best.URL
	base.DocSHA256 = sha
	base.Status = store.StatusAttached
	base.LastAttached = &t
	return s.store.Upsert(ctx, base)
}

// reviewGate tags the entity unverified and sends ONE ntfy prompt with
// one-tap Attach / Reject actions. Reject writes a "rejected" notes line via
// the portal, which the next scan ingests as a permanent negative label.
func (s *Scanner) reviewGate(ctx context.Context, detail *homebox.EntityOut, res *discovery.Result, base *store.Record) error {
	dc := s.cfg.manualClass()
	if res.Best == nil {
		// Nothing left to review (every candidate failed or was rejected).
		s.recordDecision(ctx, detail, res, dc, "notfound", "")
		base.Status = store.StatusNotFound
		return s.store.Upsert(ctx, base)
	}
	// Already prompted for this exact URL — do not notify again. (Our own
	// writes bump updatedAt and re-trigger the change-poll; without this
	// dedupe the review prompt would repeat every poll tick.)
	if prev, _ := s.store.Get(ctx, detail.ID); prev != nil &&
		prev.Status == store.StatusPendingReview && prev.DocURL == res.Best.URL {
		base.DocURL = prev.DocURL
		base.Status = store.StatusPendingReview
		return s.store.Upsert(ctx, base)
	}
	if err := s.tagUnverified(ctx, detail); err != nil {
		return err
	}
	// A model-gated pick reports confidence 0 — the honest reason is "could
	// not confirm the model number", not "0%".
	why := fmt.Sprintf("confidence %.0f%%", res.Confidence*100)
	if res.Confidence == 0 {
		why = "model number unconfirmed"
	}
	msg := notify.Message{
		Title: "docfetch: review a manual",
		Body:  fmt.Sprintf("%s — candidate found (%s). Tap to view.", detail.Name, why),
		Click: res.Best.URL,
		Tags:  []string{"page_facing_up"},
	}
	if s.cfg.PortalURL != "" && s.cfg.SignKey != "" {
		msg.Actions = []string{
			"http, Attach, " + ActionURL(s.cfg.PortalURL, "approve", detail.ID, res.Best.URL, s.cfg.SignKey) + ", method=POST, clear=true",
			"http, Reject, " + ActionURL(s.cfg.PortalURL, "reject", detail.ID, res.Best.URL, s.cfg.SignKey) + ", method=POST",
		}
	}
	if err := s.ntfy.Send(ctx, msg); err != nil {
		return err
	}
	s.recordDecision(ctx, detail, res, dc, "review", res.Best.URL)
	base.DocURL = res.Best.URL
	base.Status = store.StatusPendingReview
	return s.store.Upsert(ctx, base)
}

// --- helpers ---

func (s *Scanner) shouldReprocess(rec *store.Record, followup bool, now time.Time) bool {
	switch rec.Status {
	case store.StatusAttached:
		return false
	case store.StatusNotFound:
		if followup {
			return true
		}
		// exponential backoff: base << attempts
		return now.After(rec.LastChecked.Add(s.backoff(rec.Attempts)))
	case store.StatusPendingReview:
		return followup
	default: // new / error
		return true
	}
}

func (s *Scanner) backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > 6 {
		attempts = 6 // cap the shift
	}
	return s.cfg.BackoffBase << (attempts - 1)
}

func (s *Scanner) tagUnverified(ctx context.Context, detail *homebox.EntityOut) error {
	ids := []string{s.unverifiedTagID}
	for _, t := range detail.Tags {
		if t.ID == s.unverifiedTagID {
			return nil // already tagged — skip the PATCH (it bumps updatedAt,
			// which would re-trigger the change-poll on our own write)
		}
		ids = append(ids, t.ID)
	}
	_, err := s.api.PatchEntity(ctx, detail.ID, homebox.EntityUpdate{ID: detail.ID, Name: detail.Name, TagIDs: ids})
	return err
}

func (s *Scanner) recordError(ctx context.Context, sum *homebox.EntitySummary, cause error) {
	rec, _ := s.store.Get(ctx, sum.ID)
	base := &store.Record{
		EntityID:    sum.ID,
		Name:        sum.Name,
		UpdatedAt:   sum.UpdatedAt.UTC().Format(time.RFC3339),
		Status:      store.StatusError,
		FirstSeen:   firstSeen(rec, time.Now()),
		LastChecked: time.Now(),
	}
	if rec != nil {
		base.Attempts = rec.Attempts + 1
	}
	_ = s.store.Upsert(ctx, base)
}

// downloadFallback tries the remaining PDF candidates in score order; each one
// must also pass the content skim.
func (s *Scanner) downloadFallback(ctx context.Context, res *discovery.Result, item discovery.Item, dc DocClassCfg) (*discovery.Candidate, []byte) {
	tried := 0
	for i := range res.Candidates {
		c := &res.Candidates[i]
		if c == res.Best || !c.IsPDF || c.Score <= 0 {
			continue
		}
		if tried >= 2 { // cap fallback attempts
			break
		}
		tried++
		data, err := s.disc.Download(ctx, c.URL, s.cfg.MaxPDFBytes)
		if err != nil {
			log.Printf("fallback download failed (%s): %v", c.URL, err)
			continue
		}
		if !skimAccepts(s.disc.Skim(ctx, item, data, dc.Name), c) {
			continue
		}
		log.Printf("fallback candidate succeeded: %s", c.URL)
		return c, data
	}
	return nil, nil
}

// skimAccepts is the attach veto: real PDF, right class, and (for non-official
// sources) not positively a different product.
func skimAccepts(v discovery.SkimVerdict, c *discovery.Candidate) bool {
	if !v.IsPDF || v.ClassMismatch {
		return false
	}
	if !c.Official && v.ProductMismatch {
		return false
	}
	return true
}

// bestOfficialPDF returns the highest-scored official-domain PDF candidate.
func bestOfficialPDF(cands []discovery.Candidate) *discovery.Candidate {
	var best *discovery.Candidate
	for i := range cands {
		c := &cands[i]
		if !c.Official || c.IsHTML || !strings.HasSuffix(strings.ToLower(c.URL), ".pdf") && !c.IsPDF {
			continue
		}
		if best == nil || c.Score > best.Score {
			best = c
		}
	}
	return best
}

// hasDoc reports whether a doc class is already present: a custom field (set
// by both attach and link) is the primary signal; for the manual class we also
// honor a pre-existing type=manual attachment that predates the field feature.
func hasDoc(detail *homebox.EntityOut, dc DocClassCfg) bool {
	if homebox.FieldValue(detail.Fields, dc.Field) != "" ||
		homebox.FieldValue(detail.Fields, dc.Field+" (web)") != "" {
		return true
	}
	if dc.Name == "manual" {
		for _, a := range detail.Attachments {
			if a.Type == "manual" {
				return true
			}
		}
	}
	return false
}

func firstSeen(rec *store.Record, now time.Time) time.Time {
	if rec != nil && !rec.FirstSeen.IsZero() {
		return rec.FirstSeen
	}
	return now
}

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func filename(detail *homebox.EntityOut, dc DocClassCfg) string {
	// Token-dedupe: the name usually already contains manufacturer + model
	// ("Whirlpool WDF520PADM7 Dishwasher") — joining all three verbatim gave
	// Whirlpool-WDF520PADM7-Whirlpool-WDF520PADM7-Dishwasher.pdf.
	seen := map[string]bool{}
	var toks []string
	for _, part := range nonEmpty(detail.Manufacturer, detail.ModelNumber, detail.Name) {
		for _, w := range strings.Fields(part) {
			k := strings.ToLower(unsafeName.ReplaceAllString(w, ""))
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			toks = append(toks, w)
		}
	}
	stem := strings.Trim(unsafeName.ReplaceAllString(strings.Join(toks, "-"), "-"), "-")
	if stem == "" {
		stem = "doc"
	}
	// Suffix non-manual classes so a parts PDF doesn't collide with the manual.
	if dc.Name != "manual" && dc.Name != "" {
		stem += "-" + dc.Name
	}
	return stem + ".pdf"
}

func nonEmpty(vs ...string) []string {
	var out []string
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}
