package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/config"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/discovery"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/enrich"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/homebox"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/llm"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/notify"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/portal"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/scheduler"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/store"
)

// deps bundles the assembled components so both the scheduler and the portal
// can share one construction path.
type deps struct {
	hb  *homebox.Client
	eng *discovery.Engine
	ai  *llm.Client // nil when no key configured
	st  *store.Store
	sc  *scheduler.Scanner
}

// build assembles the scanner and its dependencies from config.
func build(cfg *config.Config) (*deps, error) {
	st, err := store.Open(cfg.StateDB)
	if err != nil {
		return nil, err
	}

	hb := homebox.New(cfg.Homebox.URL, cfg.Homebox.Token)

	// Reranker stays a nil interface when no LLM key is configured, so the
	// discovery engine runs rules-only rather than dialing a nil pointer.
	var rr discovery.Reranker
	if cfg.LLM.APIKey != "" {
		rr = llm.New(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.LLM.RerankModel)
	}
	cur := cfg.Curation
	eng := discovery.NewEngine(discovery.Options{
		SearxngURL:      cur.Discovery.SearxngURL,
		Language:        cur.Discovery.Language,
		Region:          cur.Discovery.Region,
		Pipeline:        cur.Discovery.Pipeline,
		StopConfidence:  cur.Docs.AutoAttachThreshold,
		Queries:         cur.Discovery.Queries,
		MaxCandidates:   cur.Discovery.MaxCandidates,
		MinPDFBytes:     cur.Discovery.MinPDFBytes,
		MaxPDFBytes:     cur.Discovery.MaxPDFBytes,
		MaxSnippetChars: cfg.LLM.MaxSnippetChars,
		RequireModel:    cur.Docs.RequireModelMatch,
		RatePerMin:      cur.Discovery.RateLimitPerMin,
	}, rr)

	nt := notify.New(cfg.Notify.NtfyURL, cfg.Notify.NtfyTopic, cfg.Notify.NtfyToken)

	sc := scheduler.NewScanner(hb, eng, nt, st, scheduler.Config{
		PageSize:            cfg.Homebox.PageSize,
		DocType:             cur.Docs.DocType,
		SkipIfManualExists:  cur.Docs.SkipIfManualExists,
		AutoAttachThreshold: cur.Docs.AutoAttachThreshold,
		MaxPDFBytes:         cur.Discovery.MaxPDFBytes,
		FollowupAfter:       cur.Schedule.FollowupAfter,
		BackoffBase:         cur.Discovery.BackoffBase,
		UnverifiedTag:       cfg.Tags.Unverified,
		HomeboxURL:          cfg.Homebox.URL,
		PortalURL:           strings.TrimRight(cfg.Intake.PublicURL, "/"),
		SignKey:             cfg.Homebox.Token,
		DocsEnabled:         cfg.DocsEnabled(),
		PhotoEnabled:        cur.Photo.Enabled,
		PhotoMinConfidence:  cur.Photo.MinConfidence,
		WarrantyEnabled:     cur.Warranty.Enabled,
		AuditLog:            cfg.Notes.AuditLog,
	})

	// Metadata enrichment + curation extras need search plus an LLM.
	var ai *llm.Client
	if xr, ok := rr.(*llm.Client); ok {
		ai = xr
	}
	if ai != nil {
		eng.SetVerifier(ai)      // content-level doc verification before attach
		eng.SetBrandResolver(ai) // official-domain resolution for the brand-site stage
		sc.SetCuration(eng, ai, cfg.LLM.VisionModel)
	}
	if cur.Enrich.Enabled {
		if ai != nil {
			sc.SetEnricher(enrich.New(enrich.Options{
				Enabled:            true,
				FillOnly:           cur.Enrich.FillOnly,
				AutoWriteThreshold: cur.Enrich.AutoWriteThreshold,
				MinAgreeingSources: cur.Enrich.MinAgreeingSources,
				BackCheck:          cur.Enrich.BackCheck,
				Fields:             cur.Enrich.Fields,
				MaxSnippetChars:    cfg.LLM.MaxSnippetChars,
			}, eng, ai))
		} else {
			log.Println("curation.enrich.enabled=true but no LLM key configured; enrichment disabled")
		}
	}
	return &deps{hb: hb, eng: eng, ai: ai, st: st, sc: sc}, nil
}

func runOnce(ctx context.Context, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	d, err := build(cfg)
	if err != nil {
		return err
	}
	defer d.st.Close()
	log.Println("running single scan pass")
	if err := d.sc.Scan(ctx, false); err != nil {
		return err
	}
	log.Println("scan complete")
	return nil
}

func runScheduler(ctx context.Context, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	d, err := build(cfg)
	if err != nil {
		return err
	}
	defer d.st.Close()
	return scheduler.Run(ctx, d.sc, scheduler.Specs{
		ScanNew:    cfg.Curation.Schedule.ScanNew,
		Followup:   cfg.Curation.Schedule.Followup,
		Reconcile:  cfg.Curation.Reconcile.DigestSchedule,
		ChangePoll: cfg.Curation.Schedule.ChangePoll,
	})
}

func runPortal(ctx context.Context, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	d, err := build(cfg)
	if err != nil {
		return err
	}
	defer d.st.Close()
	if d.ai == nil {
		return fmt.Errorf("portal requires an LLM key (vision extraction)")
	}
	// Intake stage: homebox + vision only — no discovery engine, no scanner.
	return portal.New(cfg, d.hb, d.ai).Run(ctx)
}
