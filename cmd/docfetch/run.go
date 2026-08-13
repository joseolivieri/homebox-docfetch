package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/joseolivieri/homebox-docfetch/internal/config"
	"github.com/joseolivieri/homebox-docfetch/internal/discovery"
	"github.com/joseolivieri/homebox-docfetch/internal/enrich"
	"github.com/joseolivieri/homebox-docfetch/internal/homebox"
	"github.com/joseolivieri/homebox-docfetch/internal/llm"
	"github.com/joseolivieri/homebox-docfetch/internal/notify"
	"github.com/joseolivieri/homebox-docfetch/internal/portal"
	"github.com/joseolivieri/homebox-docfetch/internal/scheduler"
	"github.com/joseolivieri/homebox-docfetch/internal/store"
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

	// Doc classes: discovery needs keywords/queries to classify + harvest;
	// the scanner needs field/attach-type/category to attach.
	var discClasses []discovery.DocClass
	var schedClasses []scheduler.DocClassCfg
	for _, dc := range cur.Docs.Classes {
		discClasses = append(discClasses, discovery.DocClass{Name: dc.Name, Keywords: dc.Keywords, Queries: dc.Queries})
		schedClasses = append(schedClasses, scheduler.DocClassCfg{
			Name: dc.Name, Field: dc.Field, AttachAs: dc.AttachAs,
			Categories: dc.Categories, Enabled: dc.Enabled,
		})
	}

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
		Classes:         discClasses,
	}, rr)

	nt := notify.New(cfg.Notify.NtfyURL, cfg.Notify.NtfyTopic, cfg.Notify.NtfyToken)

	sc := scheduler.NewScanner(hb, eng, nt, st, scheduler.Config{
		PageSize:            cfg.Homebox.PageSize,
		SkipIfExists:        cur.Docs.SkipIfExists,
		AutoAttachThreshold: cur.Docs.AutoAttachThreshold,
		MaxPDFBytes:         cur.Discovery.MaxPDFBytes,
		FollowupAfter:       cur.Schedule.FollowupAfter,
		BackoffBase:         cur.Discovery.BackoffBase,
		UnverifiedTag:       cfg.Tags.Unverified,
		HomeboxURL:          cfg.Homebox.URL,
		PortalURL:           strings.TrimRight(cfg.Intake.PublicURL, "/"),
		SignKey:             cfg.Homebox.Token,
		DocsEnabled:         cfg.DocsEnabled(),
		DocClasses:          schedClasses,
		PhotoEnabled:        cur.Photo.Enabled,
		PhotoMinConfidence:  cur.Photo.MinConfidence,
		WarrantyEnabled:     cur.Warranty.Enabled,
		Breadcrumb:          cfg.Notes.BreadcrumbEnabled(),
		EventRetention:      time.Duration(cfg.Notes.EventRetentionDays) * 24 * time.Hour,
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

// runServe runs the curation scheduler and the intake portal in one process
// on the shared store (plan-architecture-v2 M1 / D25). This is the blessed
// deployment shape; the standalone scheduler/portal subcommands are deprecated.
func runServe(ctx context.Context, cfgPath string) error {
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
		return fmt.Errorf("serve requires an LLM key (portal vision extraction)")
	}

	// Either half exiting (error or clean ctx shutdown) stops the other.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Portal signals (approve/reject/new intake) trigger immediate scanner
	// processing — the DB bus doesn't bump Homebox updatedAt, so the
	// change-poll can't see it (M2/D26). Egress stays in the scanner.
	trigger := func(entityID string) {
		go func() {
			if err := d.sc.ProcessEntity(ctx, entityID); err != nil {
				log.Printf("portal-triggered process %s: %v", entityID, err)
			}
		}()
	}

	errc := make(chan error, 2)
	go func() { errc <- scheduler.Run(ctx, d.sc, specsFrom(cfg)) }()
	go func() { errc <- portal.New(cfg, d.hb, d.ai, d.st, trigger).Run(ctx) }()
	err = <-errc
	cancel()
	if err2 := <-errc; err == nil {
		err = err2
	}
	return err
}

func specsFrom(cfg *config.Config) scheduler.Specs {
	return scheduler.Specs{
		ScanNew:    cfg.Curation.Schedule.ScanNew,
		Followup:   cfg.Curation.Schedule.Followup,
		Reconcile:  cfg.Curation.Reconcile.DigestSchedule,
		ChangePoll: cfg.Curation.Schedule.ChangePoll,
	}
}

// Deprecated: split-mode subcommand kept for transition; use `serve`.
func runScheduler(ctx context.Context, cfgPath string) error {
	log.Println("warning: `scheduler` is deprecated; use `serve` (runs scheduler + portal in one process)")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	d, err := build(cfg)
	if err != nil {
		return err
	}
	defer d.st.Close()
	return scheduler.Run(ctx, d.sc, specsFrom(cfg))
}

// Deprecated: split-mode subcommand kept for transition; use `serve`.
func runPortal(ctx context.Context, cfgPath string) error {
	log.Println("warning: `portal` is deprecated; use `serve` (runs scheduler + portal in one process)")
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
	// NOTE: split mode shares nothing with a scanner in another container —
	// qr/approve/reject signals land in this process's store only. Use `serve`.
	return portal.New(cfg, d.hb, d.ai, d.st, nil).Run(ctx)
}

// runLog prints recent activity events (the CLI face of the portal /log page).
func runLog(ctx context.Context, cfgPath, entityID string, limit int) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.StateDB)
	if err != nil {
		return err
	}
	defer st.Close()
	events, err := st.Events(ctx, entityID, limit)
	if err != nil {
		return err
	}
	for i := len(events) - 1; i >= 0; i-- { // oldest first for terminal reading
		e := events[i]
		name := e.EntityName
		if name == "" {
			name = e.EntityID
		}
		line := fmt.Sprintf("%s  %-14s %-22s %s", e.Ts.Format("2006-01-02 15:04"), e.Kind, name, e.Detail)
		if e.URL != "" {
			line += " " + e.URL
		}
		fmt.Println(strings.TrimSpace(line))
	}
	if len(events) == 0 {
		fmt.Println("no events")
	}
	return nil
}
