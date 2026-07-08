package scheduler

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// Specs are the cron expressions (5-field) plus the change-poll interval that
// drive the daemon.
type Specs struct {
	ScanNew   string
	Followup  string
	Reconcile string
	// ChangePoll, when > 0, probes Homebox for collection changes at this
	// interval (one pageSize=1 list call) and triggers a scan on change —
	// near-on-add behavior without a webhook. The cron scan stays as a floor.
	ChangePoll time.Duration
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
		// The cron scan also sweeps: attachment deletions don't bump
		// updatedAt, so only a per-item fetch (Sweep) notices removed
		// manuals/photos and turns them into rejection labels + re-fetches.
		// The 30s change-poll scan below stays diff-only (cheap).
		if _, err := c.AddFunc(specs.ScanNew, guard("scan", func(ctx context.Context) error {
			if err := sc.Scan(ctx, false); err != nil {
				return err
			}
			return sc.Sweep(ctx)
		})); err != nil {
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

	// Change-poll loop: cheap probe, full scan only when the collection changed.
	if specs.ChangePoll > 0 {
		scan := guard("change-scan", func(ctx context.Context) error { return sc.Scan(ctx, false) })
		go func() {
			t := time.NewTicker(specs.ChangePoll)
			defer t.Stop()
			last := ""
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					sig, err := sc.changeSignal(ctx)
					if err != nil {
						log.Printf("[change-poll] probe error: %v", err)
						continue
					}
					if last == "" { // first probe just primes the signal
						last = sig
						continue
					}
					if sig != last {
						last = sig
						log.Printf("[change-poll] collection changed; scanning")
						scan()
					}
				}
			}
		}()
	}

	c.Start()
	log.Printf("scheduler running (scan=%q followup=%q reconcile=%q change_poll=%s)", specs.ScanNew, specs.Followup, specs.Reconcile, specs.ChangePoll)
	<-ctx.Done()
	stopCtx := c.Stop() // stops scheduling; wait for a running job to finish
	<-stopCtx.Done()
	return nil
}
