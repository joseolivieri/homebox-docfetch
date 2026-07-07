package main

import (
	"context"
	"log"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/config"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/discovery"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/homebox"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/llm"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/notify"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/scheduler"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/store"
)

// build assembles the scanner and its dependencies from config.
func build(cfg *config.Config) (*scheduler.Scanner, *store.Store, error) {
	st, err := store.Open(cfg.StateDB)
	if err != nil {
		return nil, nil, err
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

	nt := notify.New(cfg.Notify.NtfyURL, cfg.Notify.NtfyTopic)

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
	})
	return sc, st, nil
}

func runOnce(ctx context.Context, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	sc, st, err := build(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	log.Println("running single scan pass")
	if err := sc.Scan(ctx, false); err != nil {
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
	sc, st, err := build(cfg)
	if err != nil {
		return err
	}
	defer st.Close()
	return scheduler.Run(ctx, sc, scheduler.Specs{
		ScanNew:   cfg.Schedule.ScanNew,
		Followup:  cfg.Schedule.Followup,
		Reconcile: cfg.Reconcile.DigestSchedule,
	})
}
