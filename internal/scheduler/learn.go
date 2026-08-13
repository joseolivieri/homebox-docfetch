// Learning feedback loop (Phase A): turn user actions in Homebox into labels
// on the decision ledger. Runs inside the weekly reconcile — the collection is
// small, so a GetEntity per tracked item is cheap.
//
// Labels harvested here:
//   - rejected (src=override): user deleted the attached manual / cleared the
//     Manual fields — the doc was wrong; re-search without that URL.
//   - overridden: user replaced a machine-written metadata value; the
//     enrichment row is marked superseded so it is never re-filled.
//   - confirmed (src=age): an attached doc survived >30 days untouched.
package scheduler

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/joseolivieri/homebox-docfetch/internal/homebox"
	"github.com/joseolivieri/homebox-docfetch/internal/notify"
	"github.com/joseolivieri/homebox-docfetch/internal/store"
)

const confirmAfter = 30 * 24 * time.Hour

// Reconcile labels overrides/confirmations, then sends the weekly digest with
// review backlog + a 7-day accuracy snapshot from the ledger.
func (s *Scanner) Reconcile(ctx context.Context) error {
	if err := s.bootstrap(ctx); err != nil {
		return err
	}
	s.sweepOverrides(ctx)

	// Retention: audit events age out; signal events (qr/approve/reject) are
	// permanent state and exempt (D27).
	if s.cfg.EventRetention > 0 {
		if n, err := s.store.PruneEvents(ctx, time.Now().Add(-s.cfg.EventRetention)); err != nil {
			log.Printf("reconcile: prune events: %v", err)
		} else if n > 0 {
			log.Printf("reconcile: pruned %d old events", n)
		}
	}

	list, err := s.api.ListEntities(ctx, 1, 1, []string{s.unverifiedTagID})
	if err != nil {
		return err
	}
	outcomes, labels, err := s.store.DecisionStats(ctx, time.Now().Add(-7*24*time.Hour))
	if err != nil {
		log.Printf("reconcile: decision stats: %v", err)
	}
	if list.Total == 0 && len(outcomes) == 0 && len(labels) == 0 {
		return nil
	}

	var b strings.Builder
	if list.Total > 0 {
		fmt.Fprintf(&b, "%d item(s) tagged %s need a review decision.\n", list.Total, s.cfg.UnverifiedTag)
	}
	if line := statLine("7d", outcomes); line != "" {
		b.WriteString(line + "\n")
	}
	if line := statLine("labels", labels); line != "" {
		b.WriteString(line + "\n")
	}
	return s.ntfy.Send(ctx, notify.Message{
		Title: "docfetch: weekly digest",
		Body:  strings.TrimSpace(b.String()),
		Tags:  []string{"clipboard"},
	})
}

func statLine(prefix string, m map[string]int) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return prefix + ": " + strings.Join(parts, ", ")
}

// Sweep runs the override sweep on demand — wired to the cron scan cadence
// because attachment deletions do NOT bump the entity's updatedAt (verified
// live), so the diff-driven scan never sees them. Only a real per-item fetch
// notices a removed manual or photo.
func (s *Scanner) Sweep(ctx context.Context) error {
	if err := s.bootstrap(ctx); err != nil {
		return err
	}
	s.sweepOverrides(ctx)
	return nil
}

// sweepOverrides walks items the pipeline acted on and diffs current Homebox
// state against what was written.
func (s *Scanner) sweepOverrides(ctx context.Context) {
	recs, err := s.store.ListByStatus(ctx, store.StatusAttached)
	if err != nil {
		log.Printf("override sweep: list: %v", err)
		return
	}
	for _, rec := range recs {
		if err := ctx.Err(); err != nil {
			return
		}
		detail, err := s.api.GetEntity(ctx, rec.EntityID)
		if err != nil {
			continue // deleted entity or transient error; nothing to label
		}

		// Doc removed by the user -> negative label + re-search without the URL.
		// Status "new" + zeroed attempts: a human deletion is a fresh
		// instruction to retry NOW, not another failed attempt — accumulated
		// backoff must not defer the refetch by weeks.
		if rec.DocURL != "" && !hasDoc(detail, s.cfg.manualClass()) {
			if n, _ := s.store.LabelDecisions(ctx, rec.EntityID, rec.DocURL, store.LabelRejected, "override"); n > 0 {
				log.Printf("override: %q removed manual %s — labeled rejected", detail.Name, rec.DocURL)
			}
			// A user deletion IS a rejection: signal event (permanent, deduped)
			// so the rescan-driven rejection shows in the log like any other.
			s.userEvent(ctx, detail, store.EvDocReject, s.cfg.manualClass().Name, rec.DocURL, "removed via Homebox (sweep) — rejected, refetching")
			rec.Status = store.StatusNew
			rec.Attempts = 0
			rec.DocURL, rec.DocSHA256, rec.LastAttached = "", "", nil
			_ = s.store.Upsert(ctx, rec)
		} else if rec.LastAttached != nil && time.Since(*rec.LastAttached) > confirmAfter {
			// Survived a month untouched -> soft positive (idempotent: only
			// unlabeled rows are touched).
			_, _ = s.store.LabelDecisions(ctx, rec.EntityID, rec.DocURL, store.LabelConfirmed, "age")
		}

		s.sweepEnrichOverrides(ctx, detail)

		// Photo/warranty re-check: their own gates make this a cheap no-op
		// when the artifacts are present — and a deleted product-official
		// photo becomes a rejection + re-fetch here (curatePhoto handles the
		// labeling), since deletions never surface through updatedAt.
		s.curatePhoto(ctx, detail)
		s.curateWarranty(ctx, detail)
	}
}

// sweepEnrichOverrides marks machine-written metadata the user has since
// changed. Superseded rows keep counting as "already enriched" so a cleared
// or corrected field is never machine-refilled.
func (s *Scanner) sweepEnrichOverrides(ctx context.Context, detail *homebox.EntityOut) {
	ens, err := s.store.Enrichments(ctx, detail.ID)
	if err != nil {
		return
	}
	for _, e := range ens {
		if e.Superseded {
			continue
		}
		current, comparable := "", true
		switch e.Field {
		case "manufacturer":
			current = detail.Manufacturer
		case "modelNumber":
			current = detail.ModelNumber
		case "name":
			current = detail.Name
		case "category":
			comparable = false // written as a tag; user tag edits are not overrides
		default:
			comparable = false
		}
		if !comparable || strings.EqualFold(strings.TrimSpace(current), strings.TrimSpace(e.Value)) {
			continue
		}
		e.Superseded = true
		if err := s.store.RecordEnrichment(ctx, e); err == nil {
			log.Printf("override: %q %s changed %q -> %q — superseded", detail.Name, e.Field, e.Value, current)
			s.userEvent(ctx, detail, store.EvEnrichOverride, e.Field, "",
				fmt.Sprintf("user changed %s %q -> %q; machine value superseded, never refilled", e.Field, e.Value, current))
		}
	}
}
