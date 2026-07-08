// Command docfetch is the homebox-docfetch sidecar entrypoint.
//
// Subcommands:
//
//	scheduler   run the cron loop (scan / followup / reconcile)   [P1-07]
//	once        run a single scan pass and exit                    [P1-07]
//	portal      run the photo-intake HTTP server                   [Phase 2]
//	probe       smoke-test the Homebox client against the live API [dev]
//	version     print version
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/joseolivieri/homelab/homebox-docfetch/internal/config"
	"github.com/joseolivieri/homelab/homebox-docfetch/internal/homebox"
)

var version = "0.0.1-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", "config.yaml", "path to the property file")
	_ = fs.Parse(os.Args[2:])

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	switch cmd {
	case "version":
		fmt.Println("docfetch", version)
	case "probe":
		mustRun(probe(ctx, *cfgPath))
	case "once":
		mustRun(runOnce(ctx, *cfgPath))
	case "scheduler":
		mustRun(runScheduler(ctx, *cfgPath))
	case "portal":
		mustRun(runPortal(ctx, *cfgPath))
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `docfetch — Homebox doc-fetch sidecar

usage: docfetch <command> [--config path]

commands:
  scheduler   run the cron loop (scan / followup / reconcile)   [P1-07]
  once        run a single scan pass and exit                    [P1-07]
  portal      run the photo-intake HTTP server                   [Phase 2]
  probe       smoke-test the Homebox client against the live API
  version     print version
`)
}

func mustRun(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// probe validates the Homebox client end-to-end against the live instance:
// read-only listing + tag bootstrap, then a temp entity create→patch→attach→delete
// lifecycle to exercise the write paths. The temp entity is always deleted.
func probe(ctx context.Context, cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	hb := homebox.New(cfg.Homebox.URL, cfg.Homebox.Token)

	types, err := hb.ListEntityTypes(ctx)
	if err != nil {
		return fmt.Errorf("list entity-types: %w", err)
	}
	fmt.Printf("entity-types: %d\n", len(types))
	for _, t := range types {
		fmt.Printf("  - %s (isLocation=%v)\n", t.Name, t.IsLocation)
	}

	list, err := hb.ListEntities(ctx, 1, cfg.Homebox.PageSize, nil)
	if err != nil {
		return fmt.Errorf("list entities: %w", err)
	}
	fmt.Printf("entities: total=%d (page has %d)\n", list.Total, len(list.Items))

	unverID, err := hb.EnsureTag(ctx, cfg.Tags.Unverified)
	if err != nil {
		return fmt.Errorf("ensure unverified tag: %w", err)
	}
	provID, err := hb.EnsureTag(ctx, cfg.Tags.Provenance)
	if err != nil {
		return fmt.Errorf("ensure provenance tag: %w", err)
	}
	fmt.Printf("tags ready: %s=%s  %s=%s\n", cfg.Tags.Unverified, unverID, cfg.Tags.Provenance, provID)

	// Write-path lifecycle on a clearly-marked temp entity.
	ent, err := hb.CreateEntity(ctx, homebox.EntityCreate{
		Name:   "[docfetch probe] temp — safe to delete",
		TagIDs: []string{unverID, provID},
	})
	if err != nil {
		return fmt.Errorf("create temp entity: %w", err)
	}
	fmt.Printf("created temp entity: %s\n", ent.ID)

	defer func() {
		if err := hb.DeleteEntity(context.WithoutCancel(ctx), ent.ID); err != nil {
			fmt.Fprintf(os.Stderr, "WARN: failed to delete temp entity %s: %v\n", ent.ID, err)
		} else {
			fmt.Printf("deleted temp entity: %s\n", ent.ID)
		}
	}()

	mfr, model := "ProbeCorp", "PB-1000"
	if _, err := hb.PatchEntity(ctx, ent.ID, homebox.EntityUpdate{
		ID: ent.ID, Name: ent.Name, Manufacturer: &mfr, ModelNumber: &model,
	}); err != nil {
		return fmt.Errorf("patch temp entity: %w", err)
	}
	fmt.Println("patched temp entity metadata")

	// Minimal valid PDF as an attachment (type=manual).
	pdf := []byte("%PDF-1.4\n1 0 obj<</Type/Catalog>>endobj\ntrailer<</Root 1 0 R>>\n%%EOF\n")
	updated, err := hb.UploadAttachment(ctx, ent.ID, "probe.pdf", "manual", false, bytes.NewReader(pdf))
	if err != nil {
		return fmt.Errorf("upload attachment: %w", err)
	}
	fmt.Printf("uploaded attachment; entity now has %d attachment(s)\n", len(updated.Attachments))

	fmt.Println("PROBE OK")
	return nil
}
