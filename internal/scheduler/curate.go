// Curation extras beyond docs: official product photo and warranty estimate.
// These moved here from the portal so ALL web egress lives in the scanner
// (stage 2) and the portal (stage 1) only ever calls the vision model —
// keeping it ready for a local/offline LLM.
package scheduler

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/discovery"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/enrich"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/homebox"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/llm"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/notes"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/store"
)

// CurationSearch is the web-search surface curation needs (SearXNG-backed).
type CurationSearch interface {
	ProductImageCandidates(ctx context.Context, subject, brand string, max int, maxBytes int64) ([]discovery.ImageCandidate, error)
	OGImage(ctx context.Context, pageURL string, maxBytes int64) (*discovery.ImageCandidate, error)
	Search(ctx context.Context, query string) ([]enrich.SearchResult, error)
}

// Vision is the LLM surface curation needs (image selection + warranty read).
type Vision interface {
	PickProductImage(ctx context.Context, visionModel, subject, category string, reference *llm.IntakeImage, candidates []llm.IntakeImage) (int, float64, error)
	VerifyProductImage(ctx context.Context, visionModel, subject, category string, img llm.IntakeImage) (bool, float64, error)
	EstimateWarranty(ctx context.Context, itemDesc string, cands []llm.Candidate) (*llm.WarrantyEstimate, error)
}

// SetCuration enables the photo + warranty curation steps.
func (s *Scanner) SetCuration(cs CurationSearch, v Vision, visionModel string) {
	s.curSearch = cs
	s.vision = v
	s.visionModel = visionModel
}

// attachApproved fulfils a one-tap approval queued in the notes block by the
// portal's Attach button. No content verification — a human approved this
// exact URL. Errors bubble up so the item retries on the next pass.
func (s *Scanner) attachApproved(ctx context.Context, detail *homebox.EntityOut, url string, base *store.Record) error {
	data, err := s.disc.Download(ctx, url, s.cfg.MaxPDFBytes)
	if err != nil {
		return fmt.Errorf("approved download %s: %w", url, err)
	}
	updated, err := s.api.UploadAttachment(ctx, detail.ID, filename(detail), s.cfg.DocType, false, bytes.NewReader(data))
	if err != nil {
		return err
	}
	if updated != nil && updated.ID != "" {
		upd := fullUpdateFrom(updated)
		n := notes.Append(updated.Notes, notes.Line("manual attached via approve "+notes.MDLink("pdf", url)))
		upd.Notes = &n
		upd.Fields = homebox.UpsertField(upd.Fields, "Manual", notes.MDLink("pdf", url))
		if _, err := s.api.PutEntity(ctx, detail.ID, upd); err != nil {
			log.Printf("approve note put %s: %v", detail.ID, err)
		}
	}
	_, _ = s.store.LabelDecisions(ctx, detail.ID, url, store.LabelConfirmed, "ntfy")
	log.Printf("manual attached via approve for %q — %s", detail.Name, url)
	t := time.Now()
	base.Status = store.StatusAttached
	base.DocURL = url
	base.DocSHA256 = store.DocSHA(data)
	base.LastAttached = &t
	return s.store.Upsert(ctx, base)
}

// curatePhoto fetches an official product photo when the item lacks one.
// Marker priority (identity over pixel similarity — a personal-photo anchor
// once matched a charger instead of the earbuds):
//
//  1. og:image from official pages we already know (QR target, Manual (web))
//     — the manufacturer's own "this is the product picture" declaration;
//     one vision yes/no sanity check, no search.
//  2. Image search (brand-domain tier first) -> vision selects by product
//     identity (manufacturer/model/category); the user's photo is only a
//     variant tie-breaker.
//
// A user deleting the product-official attachment counts as a rejection: the
// old source is labeled rejected, filtered, and the stage re-runs (~30s via
// the change-poll updatedAt bump).
func (s *Scanner) curatePhoto(ctx context.Context, detail *homebox.EntityOut) {
	if !s.cfg.PhotoEnabled || s.curSearch == nil || s.vision == nil {
		return
	}
	if hasOfficialPhoto(detail) {
		return
	}
	last, _ := s.store.LatestDecision(ctx, detail.ID, "photo")
	if last != nil && last.Outcome == "attached" && last.Label == "" {
		// We attached one and it is gone -> the user deleted it. Negative
		// label; never propose that image again; fall through to re-fetch.
		if n, _ := s.store.LabelDecisions(ctx, detail.ID, last.ChosenURL, store.LabelRejected, "override"); n > 0 {
			log.Printf("photo %s: user removed %s — labeled rejected, re-fetching", detail.ID, last.ChosenURL)
		}
	} else if s.recentClassDecision(ctx, detail.ID, "photo") {
		return
	}
	subject := subjectOf(detail)
	if subject == "" {
		return
	}
	category := categoryOf(detail)
	rejected, _ := s.store.RejectedURLs(ctx, detail.ID, "photo")

	// Stage 1: og:image from known official pages.
	for _, page := range s.officialPages(detail) {
		og, err := s.curSearch.OGImage(ctx, page, 10<<20)
		if err != nil || og == nil || rejected[og.Src] {
			continue
		}
		ok, conf, err := s.vision.VerifyProductImage(ctx, s.visionModel, subject, category,
			llm.IntakeImage{Data: og.Data, Mime: og.Mime})
		if err != nil {
			log.Printf("photo %s: og verify: %v", detail.ID, err)
			continue
		}
		if !ok || conf < s.cfg.PhotoMinConfidence {
			log.Printf("photo %s: og:image rejected (match=%v conf=%.2f) %s", detail.ID, ok, conf, og.Src)
			continue
		}
		s.attachPhoto(ctx, detail, *og, conf, "og-image")
		return
	}

	// Stage 2: image search + identity-based vision selection.
	cands, err := s.curSearch.ProductImageCandidates(ctx, subject, detail.Manufacturer, 5, 10<<20)
	if err != nil {
		log.Printf("photo %s: image search: %v", detail.ID, err)
		return
	}
	kept := cands[:0]
	for _, c := range cands {
		if !rejected[c.Src] {
			kept = append(kept, c)
		}
	}
	cands = kept
	if len(cands) == 0 {
		s.recordClass(ctx, detail, "photo", "notfound", "", 0)
		return
	}
	imgs := make([]llm.IntakeImage, len(cands))
	for i, c := range cands {
		imgs[i] = llm.IntakeImage{Data: c.Data, Mime: c.Mime}
	}
	ref := s.personalPhotoRef(ctx, detail)
	best, conf, err := s.vision.PickProductImage(ctx, s.visionModel, subject, category, ref, imgs)
	if err != nil {
		log.Printf("photo %s: image rank: %v", detail.ID, err)
		return
	}
	if best < 0 || conf < s.cfg.PhotoMinConfidence {
		log.Printf("photo %s: no candidate above threshold (best=%d conf=%.2f min=%.2f)", detail.ID, best, conf, s.cfg.PhotoMinConfidence)
		s.recordClass(ctx, detail, "photo", "notfound", "", conf)
		return
	}
	s.attachPhoto(ctx, detail, cands[best], conf, "image-search")
}

// attachPhoto uploads the chosen official photo + provenance note + ledger row.
func (s *Scanner) attachPhoto(ctx context.Context, detail *homebox.EntityOut, chosen discovery.ImageCandidate, conf float64, stage string) {
	fname := "product-official" + extFor(chosen.Mime)
	if _, err := s.api.UploadAttachment(ctx, detail.ID, fname, "photo", detail.ImageID == "", bytes.NewReader(chosen.Data)); err != nil {
		log.Printf("photo %s: attach: %v", detail.ID, err)
		return
	}
	log.Printf("photo %s: official product photo attached (conf=%.2f stage=%s src=%s)", detail.ID, conf, stage, chosen.Src)

	if fresh, err := s.api.GetEntity(ctx, detail.ID); err == nil {
		upd := fullUpdateFrom(fresh)
		n := notes.Append(fresh.Notes, notes.Line(fmt.Sprintf("photo (%.2f, %s) %s", conf, stage, notes.MDLink("src", chosen.Src))))
		upd.Notes = &n
		if _, err := s.api.PutEntity(ctx, detail.ID, upd); err != nil {
			log.Printf("photo %s: note put: %v", detail.ID, err)
		}
	}
	err := s.store.RecordDecision(ctx, &store.Decision{
		EntityID: detail.ID, EntityName: detail.Name, DocClass: "photo",
		Stage: stage, Outcome: "attached", ChosenURL: chosen.Src, Confidence: conf,
	})
	if err != nil {
		log.Printf("ledger record %s/photo: %v", detail.ID, err)
	}
}

// officialPages lists pages with manufacturer provenance already on the item:
// label QR targets and the linked official manual page.
func (s *Scanner) officialPages(detail *homebox.EntityOut) []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, u := range notes.QRURLs(detail.Notes) {
		add(u)
	}
	add(notes.Target(homebox.FieldValue(detail.Fields, "Manual (web)")))
	return out
}

// categoryOf derives the product type from the item's tags (enrichment writes
// the category as a tag). Machine/triage tags are skipped.
func categoryOf(detail *homebox.EntityOut) string {
	for _, t := range detail.Tags {
		if strings.Contains(t.Name, "/") { // docfetch/unverified, source/docfetch
			continue
		}
		return t.Name
	}
	return ""
}

// curateWarranty estimates the manufacturer warranty. Hard expiry only with a
// confident, sourced answer anchored to a purchase date; otherwise a note.
func (s *Scanner) curateWarranty(ctx context.Context, detail *homebox.EntityOut) {
	if !s.cfg.WarrantyEnabled || s.curSearch == nil || s.vision == nil {
		return
	}
	if detail.PurchaseDate == "" || detail.WarrantyExpires != "" || detail.LifetimeWarranty {
		return
	}
	// Already estimated on a previous pass (details carry the provenance text).
	if strings.Contains(detail.WarrantyDetails, "warranty per") || strings.Contains(detail.WarrantyDetails, "est. ") {
		return
	}
	if s.recentClassDecision(ctx, detail.ID, "warranty") {
		return
	}
	subject := subjectOf(detail)
	if subject == "" {
		return
	}
	results, err := s.curSearch.Search(ctx, subject+" manufacturer warranty")
	if err != nil || len(results) == 0 {
		log.Printf("warranty %s: no search results (err=%v)", detail.ID, err)
		s.recordClass(ctx, detail, "warranty", "notfound", "", 0)
		return
	}
	cands := make([]llm.Candidate, 0, 8)
	for i, r := range results {
		if i >= 8 {
			break
		}
		cands = append(cands, llm.Candidate{Title: r.Title, URL: r.URL, Snippet: truncate(r.Snippet, 150)})
	}
	est, err := s.vision.EstimateWarranty(ctx, subject, cands)
	if err != nil || est == nil || (est.Months == 0 && !est.Lifetime) {
		log.Printf("warranty %s: no estimate (err=%v est=%+v)", detail.ID, err, est)
		s.recordClass(ctx, detail, "warranty", "notfound", "", 0)
		return
	}

	fresh, err := s.api.GetEntity(ctx, detail.ID)
	if err != nil {
		return
	}
	upd := fullUpdateFrom(fresh)
	claims := ""
	if est.ClaimsURL != "" {
		claims = "; claims: " + est.ClaimsURL
	}
	auditLine := ""
	switch {
	case est.Lifetime && est.Confidence >= 0.85 && est.Source != "":
		t := true
		upd.LifetimeWarranty = &t
		d := strings.TrimSpace(fresh.WarrantyDetails + "\nlifetime warranty per " + est.Source + claims)
		upd.WarrantyDetails = &d
		auditLine = fmt.Sprintf("warranty lifetime (%.2f) %s", est.Confidence, srcRef(est.Source))
	case est.Months > 0:
		purchase, perr := parsePurchaseDate(detail.PurchaseDate)
		if perr != nil {
			return
		}
		if est.Confidence >= 0.85 && est.Source != "" {
			e := purchase.AddDate(0, est.Months, 0).Format("2006-01-02")
			upd.WarrantyExpires = &e
			d := strings.TrimSpace(fresh.WarrantyDetails + "\n" + strconv.Itoa(est.Months) + "mo standard warranty per " + est.Source + claims)
			upd.WarrantyDetails = &d
			auditLine = fmt.Sprintf("warranty %dmo (%.2f) %s", est.Months, est.Confidence, srcRef(est.Source))
		} else {
			// Uncertain: estimate note only, no hard expiry (decisions.md D11).
			d := strings.TrimSpace(fresh.WarrantyDetails + fmt.Sprintf("\nest. %dmo from %s (unverified)%s", est.Months, purchase.Format("2006-01-02"), claims))
			upd.WarrantyDetails = &d
			auditLine = fmt.Sprintf("warranty est %dmo (%.2f)", est.Months, est.Confidence)
		}
	default:
		return
	}
	if s.cfg.AuditLog && auditLine != "" {
		n := notes.Append(fresh.Notes, notes.Line(auditLine))
		upd.Notes = &n
	}
	if _, err := s.api.PutEntity(ctx, detail.ID, upd); err != nil {
		log.Printf("warranty put %s: %v", detail.ID, err)
		return
	}
	log.Printf("warranty %s: months=%d lifetime=%v (conf=%.2f)", detail.Name, est.Months, est.Lifetime, est.Confidence)
	s.recordClass(ctx, detail, "warranty", "attached", est.Source, est.Confidence)
}

// --- helpers ---

// personalPhotoRef downloads the user's own product photo (attached at
// intake) to anchor vision ranking of official-photo candidates.
func (s *Scanner) personalPhotoRef(ctx context.Context, detail *homebox.EntityOut) *llm.IntakeImage {
	var pick *homebox.Attachment
	for i := range detail.Attachments {
		a := &detail.Attachments[i]
		if strings.HasPrefix(a.Title, "product-personal") {
			pick = a
			break
		}
		if pick == nil && a.Type == "photo" && a.Primary {
			pick = a
		}
	}
	if pick == nil {
		return nil
	}
	data, mime, err := s.api.DownloadAttachment(ctx, detail.ID, pick.ID, 20<<20)
	if err != nil || len(data) == 0 {
		return nil
	}
	if !strings.HasPrefix(mime, "image/") {
		mime = "image/jpeg"
	}
	return &llm.IntakeImage{Data: data, Mime: mime}
}

// recentClassDecision reports whether this (entity, class) was decided
// recently enough to skip — succeeded ever, or tried within the follow-up
// window. Keeps notfound photo/warranty lookups from re-running every tick.
func (s *Scanner) recentClassDecision(ctx context.Context, entityID, class string) bool {
	d, err := s.store.LatestDecision(ctx, entityID, class)
	if err != nil || d == nil {
		return false
	}
	if d.Outcome == "attached" {
		// A rejected attach (user deleted the artifact) must not block re-runs.
		return d.Label != store.LabelRejected
	}
	// notfound retries on the short backoff — a photo/warranty lookup is one
	// search + one small vision call, and sources improve over time.
	window := s.cfg.BackoffBase
	if window == 0 {
		window = 24 * time.Hour
	}
	return time.Since(d.CreatedAt) < window
}

// recordClass appends a non-doc ledger row (photo / warranty).
func (s *Scanner) recordClass(ctx context.Context, detail *homebox.EntityOut, class, outcome, url string, conf float64) {
	err := s.store.RecordDecision(ctx, &store.Decision{
		EntityID: detail.ID, EntityName: detail.Name, DocClass: class,
		Outcome: outcome, ChosenURL: url, Confidence: conf,
	})
	if err != nil {
		log.Printf("ledger record %s/%s: %v", detail.ID, class, err)
	}
}

func hasOfficialPhoto(detail *homebox.EntityOut) bool {
	for _, a := range detail.Attachments {
		if strings.HasPrefix(a.Title, "product-official") {
			return true
		}
	}
	return false
}

// subjectOf is the search identity: manufacturer + model + name with
// duplicate words removed. The descriptive name matters — "Anker A3330" says
// nothing about what the product looks like; "Anker A3330 Soundcore C30i"
// does (observed live: model-only photo searches found nothing usable).
func subjectOf(detail *homebox.EntityOut) string {
	seen := map[string]bool{}
	var out []string
	for _, part := range []string{detail.Manufacturer, detail.ModelNumber, detail.Name} {
		for _, w := range strings.Fields(part) {
			k := strings.ToLower(w)
			if !seen[k] {
				seen[k] = true
				out = append(out, w)
			}
		}
	}
	return strings.Join(out, " ")
}

// srcRef renders a source as a labeled link when it is a URL, else verbatim.
func srcRef(src string) string {
	if strings.HasPrefix(src, "http") {
		return notes.MDLink("src", src)
	}
	return src
}

func parsePurchaseDate(v string) (time.Time, error) {
	v = strings.TrimSpace(v)
	if len(v) > 10 {
		v = v[:10]
	}
	return time.Parse("2006-01-02", v)
}

func extFor(mime string) string {
	switch {
	case strings.Contains(mime, "png"):
		return ".png"
	case strings.Contains(mime, "webp"):
		return ".webp"
	default:
		return ".jpg"
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}
