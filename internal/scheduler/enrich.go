package scheduler

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/enrich"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/homebox"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/notes"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/notify"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/store"
)

// Enricher is the metadata-completion engine (nil disables enrichment).
type Enricher interface {
	Enrich(ctx context.Context, it enrich.Item, hasCategoryTag bool) ([]enrich.FieldResult, error)
}

// enrichEntity fills identity gaps on the entity before doc-fetch. Returns the
// (possibly updated) detail. Fill-only: it never touches non-empty fields.
// All metadata writes go through PUT (PATCH silently drops scalars).
func (s *Scanner) enrichEntity(ctx context.Context, detail *homebox.EntityOut) (*homebox.EntityOut, error) {
	if s.enricher == nil {
		return detail, nil
	}
	// Idempotency: skip fields we already wrote for this entity.
	needs := false
	for _, f := range []struct{ name, val string }{
		{"manufacturer", detail.Manufacturer},
		{"modelNumber", detail.ModelNumber},
	} {
		if strings.TrimSpace(f.val) != "" {
			continue
		}
		done, err := s.store.AlreadyEnriched(ctx, detail.ID, f.name)
		if err != nil {
			return detail, err
		}
		if !done {
			needs = true
		}
	}
	if !needs {
		return detail, nil
	}

	frs, err := s.enricher.Enrich(ctx, enrich.Item{
		Manufacturer: detail.Manufacturer,
		ModelNumber:  detail.ModelNumber,
		Name:         detail.Name,
	}, len(detail.Tags) > 0) // any tag counts as categorized-enough for now
	if err != nil {
		return detail, err
	}
	if len(frs) == 0 {
		return detail, nil
	}

	var writes []enrich.FieldResult
	var reviews []string
	for _, fr := range frs {
		switch fr.Action {
		case enrich.ActionWrite:
			writes = append(writes, fr)
		case enrich.ActionReview:
			reviews = append(reviews, fmt.Sprintf("%s=%q (%.0f%%)", fr.Field, fr.Value, fr.Confidence*100))
		}
	}

	if len(reviews) > 0 {
		_ = s.ntfy.Send(ctx, notify.Message{
			Title: "docfetch: confirm metadata",
			Body:  fmt.Sprintf("%s — suggested: %s", detail.Name, strings.Join(reviews, ", ")),
			Tags:  []string{"mag"},
		})
	}
	if len(writes) == 0 {
		return detail, nil
	}

	// Merge staged fields into a full update (fill-only) and PUT.
	upd := fullUpdateFrom(detail)
	var wroteCategory string
	for _, fr := range writes {
		switch fr.Field {
		case "manufacturer":
			if detail.Manufacturer == "" {
				v := fr.Value
				upd.Manufacturer = &v
			}
		case "modelNumber":
			if detail.ModelNumber == "" {
				v := fr.Value
				upd.ModelNumber = &v
			}
		case "name":
			if strings.TrimSpace(detail.Name) == "" {
				upd.Name = fr.Value
			}
		case "category":
			wroteCategory = fr.Value
		}
	}
	if line := enrich.Note(writes); line != "" {
		merged := notes.Append(detail.Notes, notes.Line(line))
		upd.Notes = &merged
	}
	// Keep existing tags; add unverified so a human reviews the machine fill.
	tagIDs := []string{s.unverifiedTagID}
	for _, t := range detail.Tags {
		if t.ID != s.unverifiedTagID {
			tagIDs = append(tagIDs, t.ID)
		}
	}
	if wroteCategory != "" {
		if id, err := s.api.EnsureTag(ctx, wroteCategory); err == nil && id != "" {
			dup := false
			for _, t := range tagIDs {
				if t == id {
					dup = true
					break
				}
			}
			if !dup {
				tagIDs = append(tagIDs, id)
			}
		}
	}
	upd.TagIDs = tagIDs

	updated, err := s.api.PutEntity(ctx, detail.ID, upd)
	if err != nil {
		return detail, fmt.Errorf("enrich put: %w", err)
	}
	for _, fr := range writes {
		_ = s.store.RecordEnrichment(ctx, &store.Enrichment{
			EntityID:     detail.ID,
			Field:        fr.Field,
			Value:        fr.Value,
			Confidence:   fr.Confidence,
			EvidenceURLs: strings.Join(fr.Evidence, ","),
		})
		log.Printf("enriched %q — %s=%q (conf=%.2f, sources=%d)", detail.Name, fr.Field, fr.Value, fr.Confidence, len(fr.Evidence))
	}
	return updated, nil
}

// fullUpdateFrom builds a complete EntityUpdate mirroring the current entity,
// so a PUT (full replace) preserves everything we are not changing.
func fullUpdateFrom(d *homebox.EntityOut) homebox.EntityUpdate {
	upd := homebox.EntityUpdate{ID: d.ID, Name: d.Name}
	cp := func(v string) *string { s := v; return &s }
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
	var tags []string
	for _, t := range d.Tags {
		tags = append(tags, t.ID)
	}
	upd.TagIDs = tags
	return upd
}
