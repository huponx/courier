// Package relay is the outbox reader: it pushes the deliveries that are due and not in Redis to the stream,
// so a Redis outage between the commit and the push, a retry whose time came, or a flushed Redis never lose a job.
package relay

import (
	"context"
	"time"

	"github.com/huponx/courier/internal/queue"
	"github.com/huponx/courier/internal/store"
	"go.uber.org/zap"
)

type Relay struct {
	Store    *store.Store
	Queue    *queue.Client
	Log      *zap.Logger
	Interval time.Duration
	Stale    time.Duration
	// Retention: events older than this are purged once an hour (0 disables).
	Retention time.Duration
	// InboxRetention: inbox items older than this are purged with them (0 disables).
	InboxRetention time.Duration
}

const batch = 200

// Run blocks until ctx is done.
func (r *Relay) Run(ctx context.Context) {
	tick := time.NewTicker(r.Interval)
	defer tick.Stop()
	var lastPurge time.Time
	for {
		r.pass(ctx)
		if (r.Retention > 0 || r.InboxRetention > 0) && time.Since(lastPurge) >= time.Hour {
			lastPurge = time.Now()
			r.purge(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// pass pushes batches until nothing is due.
func (r *Relay) pass(ctx context.Context) {
	for ctx.Err() == nil {
		ids, err := r.Store.ClaimDue(ctx, batch, r.Stale)
		if err != nil {
			if ctx.Err() == nil {
				r.Log.Error("relay claim", zap.Error(err))
			}
			return
		}
		var failed []string
		for _, id := range ids {
			if err := r.Queue.Enqueue(ctx, id); err != nil {
				failed = append(failed, id)
			}
		}
		if len(failed) > 0 {
			r.Log.Error("relay enqueue", zap.Int("failed", len(failed)))
			// the stamp is cleared outside ctx: on shutdown the rows must not wait for the stale window
			rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := r.Store.Release(rctx, failed...); err != nil {
				r.Log.Error("relay release", zap.Error(err))
			}
			cancel()
			return
		}
		if len(ids) < batch {
			return
		}
	}
}

func (r *Relay) purge(ctx context.Context) {
	if r.Retention > 0 {
		if n, err := r.Store.Purge(ctx, time.Now().Add(-r.Retention)); err != nil {
			r.Log.Error("purge", zap.Error(err))
		} else if n > 0 {
			r.Log.Info("purged old events", zap.Int64("events", n))
		}
	}
	if r.InboxRetention > 0 {
		if n, err := r.Store.PurgeInbox(ctx, time.Now().Add(-r.InboxRetention)); err != nil {
			r.Log.Error("purge inbox", zap.Error(err))
		} else if n > 0 {
			r.Log.Info("purged old inbox items", zap.Int64("items", n))
		}
	}
}
