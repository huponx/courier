// Package tplsync keeps the templates edited in the admin app (courier_templates) loaded in the renderer of a process.
package tplsync

import (
	"context"
	"time"

	"github.com/huponx/courier/internal/notify/render"
	"github.com/huponx/courier/internal/store"
	"go.uber.org/zap"
)

// Load reads every edited template and puts it over the built-in ones. On an error the renderer keeps what it had.
func Load(ctx context.Context, st *store.Store, r *render.Renderer) error {
	rows, err := st.Templates(ctx)
	if err != nil {
		return err
	}
	files := map[string]string{}
	for _, t := range rows {
		for p, c := range render.OverlayPaths(t.Locale, t.Template, t.Files) {
			files[p] = c
		}
	}
	r.SetOverrides(files)
	return nil
}

// Run loads at start and then every interval (an edit made through another process reaches this one within it),
// until ctx is done. It does not fail: without the database the built-in templates stay in use.
func Run(ctx context.Context, st *store.Store, r *render.Renderer, interval time.Duration, log *zap.Logger) {
	if err := Load(ctx, st, r); err != nil && ctx.Err() == nil {
		log.Warn("templates: load failed, the built-in ones are used", zap.Error(err))
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := Load(ctx, st, r); err != nil && ctx.Err() == nil {
				log.Warn("templates: reload failed", zap.Error(err))
			}
		}
	}
}
