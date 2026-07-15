package ingame

import (
	"context"
	"time"

	"hosuto/internal/store"
)

// Run is the in-game supervisor: a single daemon goroutine (started from main, cancelled on shutdown)
// that keeps exactly one log follower alive per RUNNING server. It diffs the desired set (servers that
// are "active" to systemd) against the running followers on a ticker and starts/stops the difference.
// When the whole feature is disabled in config it stops every follower and idles — flipping the config
// takes effect within one tick, no restart.
func (e *Engine) Run(ctx context.Context) {
	// Each follower has its own cancel; the map is the supervisor's private bookkeeping (single
	// goroutine touches it, so no lock needed).
	followers := map[string]context.CancelFunc{}
	defer func() {
		for _, cancel := range followers {
			cancel()
		}
	}()

	// If aigentic is not wired up there is nothing to answer with; the CLI would only ever apologise,
	// so don't tail logs at all.
	if e.Aigentic == nil || !e.Aigentic.Enabled() {
		<-ctx.Done()
		return
	}

	reconcile := func() {
		desired := map[string]store.Server{}
		if e.enabled() {
			rctx, cancel := ctxTimeout(ctx, 10*time.Second)
			for _, srv := range e.Store.Servers() {
				if e.Mgr.State(rctx, srv) == "active" {
					desired[srv.ID] = srv
				}
			}
			cancel()
		}
		// Stop followers whose server is gone or no longer active.
		for id, cancel := range followers {
			if _, ok := desired[id]; !ok {
				cancel()
				delete(followers, id)
			}
		}
		// Start a follower for every newly-active server.
		for id, srv := range desired {
			if _, ok := followers[id]; ok {
				continue
			}
			fctx, cancel := context.WithCancel(ctx)
			followers[id] = cancel
			go e.follow(fctx, srv)
		}
	}

	reconcile() // act immediately, don't wait a full interval on boot
	ticker := time.NewTicker(e.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile()
		}
	}
}
