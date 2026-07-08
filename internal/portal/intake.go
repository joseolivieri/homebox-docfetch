package portal

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/homebox"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/notes"
)

// handleCreate commits a confirmed intake: creates the entity, PUTs metadata,
// and attaches the intake photos. Everything web-facing (official photo,
// warranty estimate, metadata enrichment, docs) is the curation stage's job —
// the scanner's change-poll notices the new item within ~30s and takes over.
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
	// QR support links (decoded locally at extract, confirmed by the user).
	// One "qr" notes line each — the scanner's qr pipeline stage reads these —
	// plus a visible custom field for the first one.
	var qrURLs []string
	for _, u := range r.Form["qrUrl"] {
		if u = strings.TrimSpace(u); u != "" && usableQRURL(u) {
			qrURLs = append(qrURLs, u)
			noteLines = append(noteLines, notes.Line("qr "+notes.MDLink("link", u)))
		}
	}
	if len(qrURLs) > 0 {
		upd.Fields = homebox.UpsertField(upd.Fields, "Support (QR)", notes.MDLink("qr", qrURLs[0]))
	}
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

	// 3. Attach intake photos. The personal product photo becomes the primary
	// image; the curation stage later fetches an official photo alongside it,
	// using the personal one (fetched back from Homebox) as its vision
	// reference for ranking candidates.
	day := time.Now().Format("2006-01-02")
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
		fname := a.stem + extFor(img.Mime)
		if _, err := s.hb.UploadAttachment(ctx, ent.ID, fname, a.attType, a.primary, bytes.NewReader(img.Data)); err != nil {
			log.Printf("portal: %s attach failed for %s: %v", a.field, ent.ID, err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"id":  ent.ID,
		"url": strings.TrimRight(s.cfg.Homebox.URL, "/") + "/item/" + ent.ID,
	})
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
