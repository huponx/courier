package store

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Webhook is an endpoint that receives the events it subscribes to.
type Webhook struct {
	ID        string
	Name      string
	URL       string
	Kind      string // slack | generic
	Events    []string
	Enabled   bool
	Secret    string
	CreatedBy string
	CreatedAt time.Time
}

const webhookCols = `id::text, name, url, kind, events, enabled, secret, created_by, created_at`

func scanWebhook(row interface{ Scan(...any) error }) (Webhook, error) {
	var w Webhook
	err := row.Scan(&w.ID, &w.Name, &w.URL, &w.Kind, &w.Events, &w.Enabled, &w.Secret, &w.CreatedBy, &w.CreatedAt)
	return w, mapErr(err)
}

// Webhooks lists every webhook, oldest first.
func (s *Store) Webhooks(ctx context.Context) ([]Webhook, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+webhookCols+` FROM courier_webhooks ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Webhook
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Webhook returns one webhook, or ErrNotFound.
func (s *Store) Webhook(ctx context.Context, id string) (Webhook, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Webhook{}, ErrNotFound
	}
	return scanWebhook(s.pool.QueryRow(ctx, `SELECT `+webhookCols+` FROM courier_webhooks WHERE id = $1`, id))
}

// SaveWebhook creates the webhook (no ID) or changes it. The secret changes only when w.Secret is set.
func (s *Store) SaveWebhook(ctx context.Context, w Webhook) (Webhook, error) {
	if w.Events == nil {
		w.Events = []string{}
	}
	if w.ID == "" {
		w.ID = uuid.NewString()
		return scanWebhook(s.pool.QueryRow(ctx, `
INSERT INTO courier_webhooks (id, name, url, kind, events, enabled, secret, created_by) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING `+webhookCols, w.ID, w.Name, w.URL, w.Kind, w.Events, w.Enabled, w.Secret, w.CreatedBy))
	}
	if _, err := uuid.Parse(w.ID); err != nil {
		return Webhook{}, ErrNotFound
	}
	return scanWebhook(s.pool.QueryRow(ctx, `
UPDATE courier_webhooks SET name = $2, url = $3, kind = $4, events = $5, enabled = $6,
    secret = CASE WHEN $7 <> '' THEN $7 ELSE secret END, updated_at = now()
WHERE id = $1 RETURNING `+webhookCols, w.ID, w.Name, w.URL, w.Kind, w.Events, w.Enabled, w.Secret))
}

func (s *Store) DeleteWebhook(ctx context.Context, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return ErrNotFound
	}
	tag, err := s.pool.Exec(ctx, `DELETE FROM courier_webhooks WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// WebhooksFor returns the enabled webhooks subscribed to the event type.
func (s *Store) WebhooksFor(ctx context.Context, eventType string) ([]Webhook, error) {
	all, err := s.Webhooks(ctx)
	if err != nil {
		return nil, err
	}
	var out []Webhook
	for _, w := range all {
		if w.Enabled && MatchEvent(w.Events, eventType) {
			out = append(out, w)
		}
	}
	return out, nil
}

// MatchEvent reports whether one of the patterns covers the event type: "*" every type, "bvchain.*" a prefix, or
// the exact name.
func MatchEvent(patterns []string, eventType string) bool {
	for _, p := range patterns {
		switch {
		case p == "*" || p == eventType:
			return true
		case strings.HasSuffix(p, ".*") && strings.HasPrefix(eventType, strings.TrimSuffix(p, "*")):
			return true
		}
	}
	return false
}
