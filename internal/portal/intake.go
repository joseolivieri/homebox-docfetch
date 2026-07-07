package portal

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/homebox"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/llm"
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
		Name:   name,
		TagIDs: []string{s.unverifiedTagID, s.provenanceTagID},
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
	note := "docfetch: created via photo intake " + time.Now().Format("2006-01-02")
	upd.Notes = &note
	if _, err := s.hb.PutEntity(ctx, ent.ID, upd); err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("metadata put: %w", err))
		return
	}

	// 3. Attach receipt photo if provided.
	if f, hdr, err := r.FormFile("receipt"); err == nil {
		data, rerr := io.ReadAll(io.LimitReader(f, maxUploadBytes))
		f.Close()
		if rerr == nil && len(data) > 0 {
			fname := "receipt-" + time.Now().Format("2006-01-02") + extFor(hdr.Header.Get("Content-Type"))
			if _, err := s.hb.UploadAttachment(ctx, ent.ID, fname, "receipt", false, bytes.NewReader(data)); err != nil {
				log.Printf("portal: receipt attach failed for %s: %v", ent.ID, err)
			}
		}
	}

	// 4. Background enrichment chain (photo, warranty, docs). Detached from the
	// request context — phone may navigate away immediately.
	go s.postIntake(context.WithoutCancel(ctx), ent.ID)

	writeJSON(w, http.StatusOK, map[string]string{
		"id":  ent.ID,
		"url": strings.TrimRight(s.cfg.Homebox.URL, "/") + "/item/" + ent.ID,
	})
}

// postIntake runs the slow enrichment after the entity exists: official product
// photo, warranty estimate, then the standard enrich+doc-fetch pipeline.
func (s *Server) postIntake(ctx context.Context, id string) {
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

	// Product photo (skip if the item already has an image; no junk on miss).
	if detail.ImageID == "" && subject != "" {
		data, mime, src, err := s.eng.BestProductImage(ctx, subject, 10<<20)
		switch {
		case err != nil:
			log.Printf("postIntake %s: image search: %v", id, err)
		case len(data) == 0:
			log.Printf("postIntake %s: no suitable product image (skipping)", id)
		default:
			fname := "product" + extFor(mime)
			if _, err := s.hb.UploadAttachment(ctx, id, fname, "photo", true, bytes.NewReader(data)); err != nil {
				log.Printf("postIntake %s: photo attach: %v", id, err)
			} else {
				log.Printf("postIntake %s: product photo attached (%s)", id, src)
			}
		}
	}

	// Warranty estimate — needs a purchase date anchor and the feature enabled.
	if s.cfg.Portal.WarrantyEstimate && detail.PurchaseDate != "" && detail.WarrantyExpires == "" && !detail.LifetimeWarranty {
		s.estimateWarranty(ctx, detail, subject)
	}

	// Standard pipeline: metadata enrichment + manual fetch + gates.
	if err := s.sc.ProcessEntity(ctx, id); err != nil {
		log.Printf("postIntake %s: pipeline: %v", id, err)
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
	if err != nil || est.Months == 0 {
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
	if est.Confidence >= 0.85 && est.Source != "" {
		e := expiry.Format("2006-01-02")
		upd.WarrantyExpires = &e
		d := strings.TrimSpace(fresh.WarrantyDetails + "\ndocfetch: " + strconv.Itoa(est.Months) + "mo standard warranty per " + est.Source)
		upd.WarrantyDetails = &d
	} else {
		// Uncertain: estimate note only, no hard expiry (decisions.md D11).
		d := strings.TrimSpace(fresh.WarrantyDetails + fmt.Sprintf("\ndocfetch: est. %dmo from %s (unverified)", est.Months, purchase.Format("2006-01-02")))
		upd.WarrantyDetails = &d
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
