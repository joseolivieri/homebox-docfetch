package scheduler

import (
	"context"
	"log"
	"sync"

	"github.com/robfig/cron/v3"
)

// Specs are the three cron expressions (5-field) that drive the daemon.
type Specs struct {
	ScanNew   string
	Followup  string
	Reconcile string
}

// Run registers the cron jobs and blocks until ctx is cancelled. A single mutex
// guards all jobs so a long scan never overlaps a followup/reconcile (or itself).
func Run(ctx context.Context, sc *Scanner, specs Specs) error {
	c := cron.New()
	var mu sync.Mutex

	guard := func(name string, fn func(context.Context) error) func() {
		return func() {
			if !mu.TryLock() {
				log.Printf("[%s] skipped: another job is running", name)
				return
			}
			defer mu.Unlock()
			log.Printf("[%s] start", name)
			if err := fn(ctx); err != nil {
				log.Printf("[%s] error: %v", name, err)
			} else {
				log.Printf("[%s] done", name)
			}
		}
	}

	if specs.ScanNew != "" {
		if _, err := c.AddFunc(specs.ScanNew, guard("scan", func(ctx context.Context) error { return sc.Scan(ctx, false) })); err != nil {
			return err
		}
	}
	if specs.Followup != "" {
		if _, err := c.AddFunc(specs.Followup, guard("followup", func(ctx context.Context) error { return sc.Scan(ctx, true) })); err != nil {
			return err
		}
	}
	if specs.Reconcile != "" {
		if _, err := c.AddFunc(specs.Reconcile, guard("reconcile", sc.Reconcile)); err != nil {
			return err
		}
	}

	c.Start()
	log.Printf("scheduler running (scan=%q followup=%q reconcile=%q)", specs.ScanNew, specs.Followup, specs.Reconcile)
	<-ctx.Done()
	stopCtx := c.Stop() // stops scheduling; wait for a running job to finish
	<-stopCtx.Done()
	return nil
}
