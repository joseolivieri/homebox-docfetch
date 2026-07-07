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
	eng := discovery.NewEngine(discovery.Options{
		SearxngURL:      cfg.Discovery.SearxngURL,
		Queries:         cfg.Discovery.Queries,
		MaxCandidates:   cfg.Discovery.MaxCandidates,
		MinPDFBytes:     cfg.Discovery.MinPDFBytes,
		MaxPDFBytes:     cfg.Discovery.MaxPDFBytes,
		MaxSnippetChars: cfg.LLM.MaxSnippetChars,
		RequireModel:    cfg.Confidence.RequireModelMatch,
		RatePerMin:      cfg.Discovery.RateLimitPerMin,
	}, rr)

	nt := notify.New(cfg.Notify.NtfyURL, cfg.Notify.NtfyTopic, cfg.Notify.NtfyToken)

	sc := scheduler.NewScanner(hb, eng, nt, st, scheduler.Config{
		PageSize:            cfg.Homebox.PageSize,
		DocType:             cfg.Attach.DocType,
		SkipIfManualExists:  cfg.Attach.SkipIfManualExists,
		AutoAttachThreshold: cfg.Confidence.AutoAttachThreshold,
		MaxPDFBytes:         cfg.Discovery.MaxPDFBytes,
		FollowupAfter:       cfg.Schedule.FollowupAfter,
		BackoffBase:         cfg.Discovery.BackoffBase,
		UnverifiedTag:       cfg.Intake.UnverifiedTag,
		HomeboxURL:          cfg.Homebox.URL,
		PortalURL:           strings.TrimRight(cfg.Portal.PublicURL, "/"),
		SignKey:             cfg.Homebox.Token,
	})

	// Metadata enrichment (Phase 1.5) needs both search and an LLM extractor.
	var ai *llm.Client
	if xr, ok := rr.(*llm.Client); ok {
		ai = xr
	}
	if cfg.Enrich.Enabled {
		if ai != nil {
			sc.SetEnricher(enrich.New(enrich.Options{
				Enabled:            true,
				FillOnly:           cfg.Enrich.FillOnly,
				AutoWriteThreshold: cfg.Enrich.AutoWriteThreshold,
				MinAgreeingSources: cfg.Enrich.MinAgreeingSources,
				BackCheck:          cfg.Enrich.BackCheck,
				Fields:             cfg.Enrich.Fields,
				MaxSnippetChars:    cfg.LLM.MaxSnippetChars,
			}, eng, ai))
		} else {
			log.Println("enrich.enabled=true but no LLM key configured; enrichment disabled")
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
		ScanNew:    cfg.Schedule.ScanNew,
		Followup:   cfg.Schedule.Followup,
		Reconcile:  cfg.Reconcile.DigestSchedule,
		ChangePoll: cfg.Schedule.ChangePoll,
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
	return portal.New(cfg, d.hb, d.eng, d.ai, d.sc).Run(ctx)
}
