// Package scheduler wires the pieces into runnable jobs: scan (new items),
// followup (re-check known items), and reconcile (weekly "awaiting review"
// digest). All Homebox / discovery / ntfy access goes through small interfaces
// so the scan decision logic is unit-testable with fakes.
package scheduler

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"regexp"
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
	EnsureTag(ctx context.Context, name string) (string, error)
}

// Discoverer runs the search pipeline and downloads a chosen doc.
type Discoverer interface {
	Discover(ctx context.Context, it discovery.Item) (*discovery.Result, error)
	Download(ctx context.Context, url string, maxBytes int64) ([]byte, error)
}

// Notifier sends ntfy messages.
type Notifier interface {
	Send(ctx context.Context, m notify.Message) error
}

// Config holds the scanner's behavioural knobs (from the property file).
type Config struct {
	PageSize            int
	DocType             string
	SkipIfManualExists  bool
	AutoAttachThreshold float64
	MaxPDFBytes         int64
	FollowupAfter       time.Duration
	BackoffBase         time.Duration
	UnverifiedTag       string
	HomeboxURL          string
	PortalURL           string // public portal base; enables ntfy approve buttons
	SignKey             string // HMAC key for approve links (the Homebox token)
}

type Scanner struct {
	api      EntityAPI
	disc     Discoverer
	ntfy     Notifier
	store    *store.Store
	cfg      Config
	enricher Enricher // nil = enrichment disabled

	unverifiedTagID string
}

func NewScanner(api EntityAPI, disc Discoverer, n Notifier, st *store.Store, cfg Config) *Scanner {
	if cfg.PageSize == 0 {
		cfg.PageSize = 100
	}
	if cfg.DocType == "" {
		cfg.DocType = "manual"
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

	// Already has a manual -> doc-fetch is done.
	if s.cfg.SkipIfManualExists && hasManual(detail) {
		log.Printf("skip %q — manual already present", detail.Name)
		base.Status = store.StatusAttached
		return s.store.Upsert(ctx, base)
	}

	item := discovery.Item{Manufacturer: detail.Manufacturer, ModelNumber: detail.ModelNumber, Name: detail.Name}
	if strings.TrimSpace(item.Manufacturer) == "" && strings.TrimSpace(item.ModelNumber) == "" && strings.TrimSpace(item.Name) == "" {
		// Truly nothing to search on.
		log.Printf("skip %q — no searchable identity", detail.Name)
		base.Status = store.StatusNotFound
		base.Attempts++
		return s.store.Upsert(ctx, base)
	}

	res, err := s.disc.Discover(ctx, item)
	if err != nil {
		return err
	}
	base.Attempts++
	if res == nil {
		res = &discovery.Result{}
	}

	switch {
	case res.Best != nil && res.Confidence >= s.cfg.AutoAttachThreshold:
		log.Printf("attach %q — conf=%.2f llm=%v url=%s", detail.Name, res.Confidence, res.UsedLLM, res.Best.URL)
		return s.attach(ctx, detail, res.Best, rec, base)
	case res.Best != nil:
		log.Printf("review-gate %q — conf=%.2f (below %.2f) url=%s", detail.Name, res.Confidence, s.cfg.AutoAttachThreshold, res.Best.URL)
		return s.reviewGate(ctx, detail, res, base)
	default:
		log.Printf("no manual found for %q (candidates=%d)", detail.Name, len(res.Candidates))
		base.Status = store.StatusNotFound
		return s.store.Upsert(ctx, base)
	}
}

// attach downloads, dedupes by content hash, uploads as a manual.
func (s *Scanner) attach(ctx context.Context, detail *homebox.EntityOut, best *discovery.Candidate, rec, base *store.Record) error {
	data, err := s.disc.Download(ctx, best.URL, s.cfg.MaxPDFBytes)
	if err != nil {
		return err
	}
	sha := store.DocSHA(data)
	base.DocURL = best.URL
	base.DocSHA256 = sha

	if rec != nil && rec.DocSHA256 == sha {
		// Identical doc already attached previously; do not re-upload.
		base.Status = store.StatusAttached
		base.LastAttached = rec.LastAttached
		return s.store.Upsert(ctx, base)
	}

	updated, err := s.api.UploadAttachment(ctx, detail.ID, filename(detail), s.cfg.DocType, false, bytes.NewReader(data))
	if err != nil {
		return err
	}
	// Log the attach in the entity's docfetch notes block.
	if updated != nil && updated.ID != "" {
		upd := fullUpdateFrom(updated)
		n := notes.Append(updated.Notes, notes.Line(fmt.Sprintf("manual attached — %s", best.URL)))
		upd.Notes = &n
		if _, err := s.api.PutEntity(ctx, detail.ID, upd); err != nil {
			log.Printf("attach note put %s: %v", detail.ID, err)
		}
	}
	t := time.Now()
	base.Status = store.StatusAttached
	base.LastAttached = &t
	return s.store.Upsert(ctx, base)
}

// reviewGate tags the entity unverified and sends one ntfy prompt.
func (s *Scanner) reviewGate(ctx context.Context, detail *homebox.EntityOut, res *discovery.Result, base *store.Record) error {
	if err := s.tagUnverified(ctx, detail); err != nil {
		return err
	}
	msg := notify.Message{
		Title: "docfetch: review a manual",
		Body:  fmt.Sprintf("%s — candidate found (confidence %.0f%%). Tap to view; Attach to accept.", detail.Name, res.Confidence*100),
		Click: res.Best.URL,
		Tags:  []string{"page_facing_up"},
	}
	if s.cfg.PortalURL != "" && s.cfg.SignKey != "" {
		msg.Actions = []string{
			"http, Attach manual, " + ApproveURL(s.cfg.PortalURL, detail.ID, res.Best.URL, s.cfg.SignKey) + ", method=POST, clear=true",
		}
	}
	if err := s.ntfy.Send(ctx, msg); err != nil {
		return err
	}
	base.DocURL = res.Best.URL
	base.Status = store.StatusPendingReview
	return s.store.Upsert(ctx, base)
}

// Reconcile sends a weekly digest of how many items still carry the unverified tag.
func (s *Scanner) Reconcile(ctx context.Context) error {
	if err := s.bootstrap(ctx); err != nil {
		return err
	}
	list, err := s.api.ListEntities(ctx, 1, 1, []string{s.unverifiedTagID})
	if err != nil {
		return err
	}
	if list.Total == 0 {
		return nil
	}
	return s.ntfy.Send(ctx, notify.Message{
		Title: "docfetch: items awaiting review",
		Body:  fmt.Sprintf("%d item(s) tagged %s need a room/doc decision.", list.Total, s.cfg.UnverifiedTag),
		Tags:  []string{"clipboard"},
	})
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
		if t.ID != s.unverifiedTagID {
			ids = append(ids, t.ID)
		}
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

func hasManual(detail *homebox.EntityOut) bool {
	for _, a := range detail.Attachments {
		if a.Type == "manual" {
			return true
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

func filename(detail *homebox.EntityOut) string {
	stem := strings.Trim(unsafeName.ReplaceAllString(
		strings.Join(nonEmpty(detail.Manufacturer, detail.ModelNumber, detail.Name), "-"), "-"), "-")
	if stem == "" {
		stem = "manual"
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
