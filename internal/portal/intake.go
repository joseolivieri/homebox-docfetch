package portal

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/homebox"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/llm"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/notes"
)

// handleCreate commits a confirmed intake: creates the entity, PUTs metadata,
// attaches the receipt, then runs photo + warranty + doc-fetch in the
// background. Multipart form: confirmed fields + optional receipt photo.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("name is required"))
		return
	}
	ctx := r.Context()

	// 1. Create with triage + provenance tags (and optional location parent).
	create := homebox.EntityCreate{
		Name:     name,
		TagIDs:   []string{s.unverifiedTagID, s.provenanceTagID},
		Quantity: 1,
	}
	if v := strings.TrimSpace(r.FormValue("quantity")); v != "" {
		if q, err := strconv.ParseFloat(v, 64); err == nil && q >= 1 {
			create.Quantity = q
		}
	}
	if loc := strings.TrimSpace(r.FormValue("locationId")); loc != "" {
		create.ParentID = loc
	}
	ent, err := s.hb.CreateEntity(ctx, create)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	// 2. PUT metadata (PATCH silently drops scalar metadata — spec §6).
	upd := homebox.EntityUpdate{ID: ent.ID, Name: name, TagIDs: create.TagIDs}
	upd.Quantity = &create.Quantity
	set := func(dst **string, key string) {
		if v := strings.TrimSpace(r.FormValue(key)); v != "" {
			*dst = &v
		}
	}
	set(&upd.Manufacturer, "manufacturer")
	set(&upd.ModelNumber, "modelNumber")
	set(&upd.SerialNumber, "serialNumber")
	set(&upd.PurchaseFrom, "purchaseFrom")
	set(&upd.PurchaseDate, "purchaseDate")
	if v := strings.TrimSpace(r.FormValue("purchasePrice")); v != "" {
		if p, err := strconv.ParseFloat(v, 64); err == nil && p > 0 {
			upd.PurchasePrice = &p
		}
	}
	if create.ParentID != "" {
		upd.ParentID = &create.ParentID
	}

	// Warranty from the confirm screen (vision-extracted, user-corrected).
	// Hard expiry only when both a duration and a purchase date exist (D11);
	// details/claims link always recorded when present.
	warrantyMonths := 0
	if v := strings.TrimSpace(r.FormValue("warrantyMonths")); v != "" {
		if m, err := strconv.Atoi(v); err == nil && m > 0 && m <= 360 {
			warrantyMonths = m
		}
	}
	var warrantyLines []string
	if warrantyMonths > 0 {
		warrantyLines = append(warrantyLines, fmt.Sprintf("%dmo warranty (from intake photo)", warrantyMonths))
		if pd := strings.TrimSpace(r.FormValue("purchaseDate")); pd != "" {
			if t, err := time.Parse("2006-01-02", pd); err == nil {
				e := t.AddDate(0, warrantyMonths, 0).Format("2006-01-02")
				upd.WarrantyExpires = &e
			}
		}
	}
	if v := strings.TrimSpace(r.FormValue("warrantyClaimsUrl")); v != "" {
		warrantyLines = append(warrantyLines, "claims: "+v)
	}
	if v := strings.TrimSpace(r.FormValue("warrantyDetails")); v != "" {
		warrantyLines = append(warrantyLines, v)
	}
	if len(warrantyLines) > 0 {
		d := strings.Join(warrantyLines, "\n")
		upd.WarrantyDetails = &d
	}

	noteLines := []string{notes.Line("created via photo intake")}
	if s.cfg.Notes.AuditLog {
		// Terse provenance for everything the intake attaches/derives.
		var got []string
		for _, f := range []string{"sticker", "receipt", "product", "warranty"} {
			if r.MultipartForm != nil && len(r.MultipartForm.File[f]) > 0 {
				got = append(got, f)
			}
		}
		if len(got) > 0 {
			noteLines = append(noteLines, notes.Line("photos: "+strings.Join(got, ", ")))
		}
		if warrantyMonths > 0 {
			noteLines = append(noteLines, notes.Line(fmt.Sprintf("warranty %dmo (from photo)", warrantyMonths)))
		}
	}
	note := notes.Append("", noteLines...)
	upd.Notes = &note
	if _, err := s.hb.PutEntity(ctx, ent.ID, upd); err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("metadata put: %w", err))
		return
	}

	// 3. Attach intake photos. Personal product photo becomes the primary image;
	// the official-photo fetch below still runs and attaches alongside it. Its
	// bytes are also retained as the vision reference for ranking official-photo
	// candidates (reverse-image-search-ish matching).
	day := time.Now().Format("2006-01-02")
	var personal *llm.IntakeImage
	attachments := []struct {
		field, attType, stem string
		primary              bool
	}{
		{"sticker", "photo", "model-sticker", false},
		{"receipt", "receipt", "receipt-" + day, false},
		{"product", "photo", "product-personal", true},
		{"warranty", "warranty", "warranty-" + day, false},
	}
	for _, a := range attachments {
		img, ok := formImage(r, a.field)
		if !ok {
			continue
		}
		if a.field == "product" {
			p := img
			personal = &p
		}
		fname := a.stem + extFor(img.Mime)
		if _, err := s.hb.UploadAttachment(ctx, ent.ID, fname, a.attType, a.primary, bytes.NewReader(img.Data)); err != nil {
			log.Printf("portal: %s attach failed for %s: %v", a.field, ent.ID, err)
		}
	}

	// 4. Background enrichment chain (photo, warranty, docs). Detached from the
	// request context — phone may navigate away immediately.
	go s.postIntake(context.WithoutCancel(ctx), ent.ID, personal)

	writeJSON(w, http.StatusOK, map[string]string{
		"id":  ent.ID,
		"url": strings.TrimRight(s.cfg.Homebox.URL, "/") + "/item/" + ent.ID,
	})
}

// postIntake runs the slow enrichment after the entity exists: official product
// photo (vision-ranked, thresholded), warranty estimate, then the standard
// enrich+doc-fetch pipeline. personal, when non-nil, is the user's own photo of
// the item and anchors the official-photo ranking.
func (s *Server) postIntake(ctx context.Context, id string, personal *llm.IntakeImage) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	detail, err := s.hb.GetEntity(ctx, id)
	if err != nil {
		log.Printf("postIntake %s: get: %v", id, err)
		return
	}
	subject := strings.TrimSpace(detail.Manufacturer + " " + detail.ModelNumber)
	if strings.TrimSpace(detail.ModelNumber) == "" {
		subject = strings.TrimSpace(detail.Manufacturer + " " + detail.Name)
	}

	// Official product photo — fetched even when a personal photo was provided
	// at intake (both live on the entity). Candidates are vision-ranked (against
	// the personal photo when available) and gated by photo_min_confidence:
	// below the bar, NO photo attaches. Confidence + source recorded in notes.
	if subject != "" {
		s.fetchOfficialPhoto(ctx, detail, subject, personal)
	}

	// Warranty estimate — needs a purchase date anchor and the feature enabled.
	if s.cfg.Portal.WarrantyEstimate && detail.PurchaseDate != "" && detail.WarrantyExpires == "" && !detail.LifetimeWarranty {
		s.estimateWarranty(ctx, detail, subject)
	}

	// Docs + metadata enrichment are intentionally NOT run here: the scheduler's
	// change-poll picks the new item up within ~30s, and having a single writer
	// prevents the portal/scheduler race that once attached two manuals.
}

// fetchOfficialPhoto gathers image candidates, has the vision model pick the
// best (matched against the personal reference photo when present), applies the
// confidence threshold, attaches, and records provenance in notes.
func (s *Server) fetchOfficialPhoto(ctx context.Context, detail *homebox.EntityOut, subject string, personal *llm.IntakeImage) {
	cands, err := s.eng.ProductImageCandidates(ctx, subject, 5, 10<<20)
	if err != nil {
		log.Printf("postIntake %s: image search: %v", detail.ID, err)
		return
	}
	if len(cands) == 0 {
		log.Printf("postIntake %s: no product image candidates (skipping)", detail.ID)
		return
	}
	imgs := make([]llm.IntakeImage, len(cands))
	for i, c := range cands {
		imgs[i] = llm.IntakeImage{Data: c.Data, Mime: c.Mime}
	}
	best, conf, err := s.ai.PickProductImage(ctx, s.cfg.LLM.VisionModel, subject, personal, imgs)
	if err != nil {
		log.Printf("postIntake %s: image rank: %v", detail.ID, err)
		return
	}
	minConf := s.cfg.Portal.PhotoMinConfidence
	if minConf == 0 {
		minConf = 0.7
	}
	if best < 0 || conf < minConf {
		log.Printf("postIntake %s: no photo above threshold (best=%d conf=%.2f min=%.2f) — skipping", detail.ID, best, conf, minConf)
		return
	}
	chosen := cands[best]
	fname := "product-official" + extFor(chosen.Mime)
	if _, err := s.hb.UploadAttachment(ctx, detail.ID, fname, "photo", detail.ImageID == "", bytes.NewReader(chosen.Data)); err != nil {
		log.Printf("postIntake %s: photo attach: %v", detail.ID, err)
		return
	}
	log.Printf("postIntake %s: official product photo attached (conf=%.2f ref=%v src=%s)", detail.ID, conf, personal != nil, chosen.Src)

	// Provenance note (fresh fetch + full merge so nothing gets blanked).
	fresh, err := s.hb.GetEntity(ctx, detail.ID)
	if err != nil {
		return
	}
	upd := fullUpdate(fresh)
	ref := ""
	if personal != nil {
		ref = ", matched"
	}
	n := notes.Append(fresh.Notes, notes.Line(fmt.Sprintf("photo (%.2f%s) %s", conf, ref, notes.MDLink("src", chosen.Src))))
	upd.Notes = &n
	if _, err := s.hb.PutEntity(ctx, detail.ID, upd); err != nil {
		log.Printf("postIntake %s: photo note put: %v", detail.ID, err)
	}
}

func (s *Server) estimateWarranty(ctx context.Context, detail *homebox.EntityOut, subject string) {
	results, err := s.eng.Search(ctx, subject+" manufacturer warranty")
	if err != nil || len(results) == 0 {
		return
	}
	cands := make([]llm.Candidate, 0, 8)
	for i, r := range results {
		if i >= 8 {
			break
		}
		cands = append(cands, llm.Candidate{Title: r.Title, URL: r.URL, Snippet: truncate(r.Snippet, 150)})
	}
	est, err := s.ai.EstimateWarranty(ctx, subject, cands)
	if err != nil || (est.Months == 0 && !est.Lifetime) {
		return
	}
	// Lifetime warranty: set the native bool, no expiry math needed.
	if est.Lifetime && est.Confidence >= 0.85 && est.Source != "" {
		fresh, ferr := s.hb.GetEntity(ctx, detail.ID)
		if ferr != nil {
			return
		}
		upd := fullUpdate(fresh)
		t := true
		upd.LifetimeWarranty = &t
		d := strings.TrimSpace(fresh.WarrantyDetails + "\nlifetime warranty per " + est.Source)
		if est.ClaimsURL != "" {
			d += "; claims: " + est.ClaimsURL
		}
		upd.WarrantyDetails = &d
		if s.cfg.Notes.AuditLog {
			n := notes.Append(fresh.Notes, notes.Line(fmt.Sprintf("warranty lifetime (%.2f) %s", est.Confidence, notes.MDLink("src", est.Source))))
			upd.Notes = &n
		}
		if _, err := s.hb.PutEntity(ctx, detail.ID, upd); err != nil {
			log.Printf("warranty lifetime put %s: %v", detail.ID, err)
		} else {
			log.Printf("warranty %s: lifetime (conf=%.2f)", detail.Name, est.Confidence)
		}
		return
	}
	if est.Months == 0 {
		return
	}
	purchase, err := time.Parse("2006-01-02", strings.TrimSpace(detail.PurchaseDate[:10]))
	if err != nil {
		return
	}
	expiry := purchase.AddDate(0, est.Months, 0)

	// Re-fetch + full-merge so the PUT can't blank anything set since.
	fresh, err := s.hb.GetEntity(ctx, detail.ID)
	if err != nil {
		return
	}
	upd := fullUpdate(fresh)
	claims := ""
	if est.ClaimsURL != "" {
		claims = "; claims: " + est.ClaimsURL
	}
	auditLine := ""
	if est.Confidence >= 0.85 && est.Source != "" {
		e := expiry.Format("2006-01-02")
		upd.WarrantyExpires = &e
		d := strings.TrimSpace(fresh.WarrantyDetails + "\n" + strconv.Itoa(est.Months) + "mo standard warranty per " + est.Source + claims)
		upd.WarrantyDetails = &d
		auditLine = fmt.Sprintf("warranty %dmo (%.2f) %s", est.Months, est.Confidence, notes.MDLink("src", est.Source))
	} else {
		// Uncertain: estimate note only, no hard expiry (decisions.md D11).
		d := strings.TrimSpace(fresh.WarrantyDetails + fmt.Sprintf("\nest. %dmo from %s (unverified)%s", est.Months, purchase.Format("2006-01-02"), claims))
		upd.WarrantyDetails = &d
		auditLine = fmt.Sprintf("warranty est %dmo (%.2f)", est.Months, est.Confidence)
	}
	if s.cfg.Notes.AuditLog && auditLine != "" {
		n := notes.Append(fresh.Notes, notes.Line(auditLine))
		upd.Notes = &n
	}
	if _, err := s.hb.PutEntity(ctx, detail.ID, upd); err != nil {
		log.Printf("warranty put %s: %v", detail.ID, err)
	} else {
		log.Printf("warranty %s: %dmo (conf=%.2f)", detail.Name, est.Months, est.Confidence)
	}
}

// fullUpdate mirrors scheduler.fullUpdateFrom for the portal package.
func fullUpdate(d *homebox.EntityOut) homebox.EntityUpdate {
	cp := func(v string) *string { s := v; return &s }
	upd := homebox.EntityUpdate{ID: d.ID, Name: d.Name}
	upd.Manufacturer = cp(d.Manufacturer)
	upd.ModelNumber = cp(d.ModelNumber)
	upd.SerialNumber = cp(d.SerialNumber)
	upd.AssetID = cp(d.AssetID)
	upd.Notes = cp(d.Notes)
	upd.Description = cp(d.Description)
	upd.Quantity = &d.Quantity
	upd.Insured = &d.Insured
	upd.Archived = &d.Archived
	upd.LifetimeWarranty = &d.LifetimeWarranty
	upd.PurchaseFrom = cp(d.PurchaseFrom)
	upd.PurchaseDate = cp(d.PurchaseDate)
	upd.PurchasePrice = &d.PurchasePrice
	upd.WarrantyExpires = cp(d.WarrantyExpires)
	upd.WarrantyDetails = cp(d.WarrantyDetails)
	if d.Parent != nil && d.Parent.ID != "" {
		// PUT is a full replace; omitting parentId clears the location.
		upd.ParentID = cp(d.Parent.ID)
	}
	// PUT without fields wipes all custom fields (verified live).
	upd.Fields = d.Fields
	var tags []string
	for _, t := range d.Tags {
		tags = append(tags, t.ID)
	}
	upd.TagIDs = tags
	return upd
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

var _ = http.StatusOK
