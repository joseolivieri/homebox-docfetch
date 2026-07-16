package scheduler

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/joseolivieri/homebox-docfetch/internal/enrich"
	"github.com/joseolivieri/homebox-docfetch/internal/homebox"
	"github.com/joseolivieri/homebox-docfetch/internal/notify"
	"github.com/joseolivieri/homebox-docfetch/internal/store"
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
	s.setBreadcrumb(&upd, detail.Notes, detail)
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
		ev := ""
		if len(fr.Evidence) > 0 {
			ev = fr.Evidence[0]
		}
		s.event(ctx, detail, store.EvEnrichWrite, fr.Field, ev, fmt.Sprintf("%s=%q conf=%.2f", fr.Field, fr.Value, fr.Confidence))
		log.Printf("enriched %q — %s=%q (conf=%.2f, sources=%d)", detail.Name, fr.Field, fr.Value, fr.Confidence, len(fr.Evidence))
	}
	return updated, nil
}

// fullUpdateFrom is a local alias for homebox.FullUpdateFrom (kept to avoid
// churn at every scheduler call site).
func fullUpdateFrom(d *homebox.EntityOut) homebox.EntityUpdate { return homebox.FullUpdateFrom(d) }
