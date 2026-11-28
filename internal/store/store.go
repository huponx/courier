// Package store keeps the notification state in PostgreSQL: the events, the deliveries (audit trail,
// retry state, dead letters) and the outbox flag the relay reads. Redis only carries the work.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	StatusPending = "pending"
	StatusSent    = "sent"
	StatusFailed  = "failed"
	StatusDead    = "dead"
	StatusSkipped = "skipped"
)

var (
	ErrNotFound = errors.New("store: not found")
	// ErrNotClaimable: the delivery is not pending, or another worker holds its lease.
	ErrNotClaimable = errors.New("store: delivery is not claimable")
	// ErrNotRetryable: only failed and dead deliveries can be retried by hand.
	ErrNotRetryable = errors.New("store: delivery is not failed or dead")
)

type Store struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) Close() { s.pool.Close() }

type NewEvent struct {
	IdempotencyKey string
	EventType      string
	Locale         string
	Data           map[string]string
	CorrelationID  string
	RequestID      string
	UserID         string
	TenantID       string
	AgencyID       string
}

type NewDelivery struct {
	Channel string
	Address string
	// UserID is the identity user (empty for a raw address); Locale overrides the event locale when set;
	// Vars (the recipient's name) is merged over the event data when rendering.
	UserID string
	Locale string
	Vars   map[string]string
	// Skip records the delivery as skipped (the choice of the user turned it off), with this reason, instead of sending it.
	Skip string
	// NotBefore delays the first attempt (quiet hours, the window of a digest); zero: now.
	NotBefore time.Time
	// DigestKey groups the delivery with the pending ones of the same key, channel and address: it then does not
	// become a delivery, the event is attached to the first one (the leader) and goes out with it.
	DigestKey string
}

type Event struct {
	ID            string
	EventType     string
	Locale        string
	Data          map[string]string
	CorrelationID string
	RequestID     string
	UserID        string
	TenantID      string
	AgencyID      string
	CreatedAt     time.Time
}

type Delivery struct {
	ID            string
	EventID       string
	Channel       string
	Address       string
	Status        string
	Attempts      int
	LastError     string
	NextAttemptAt time.Time
	SentAt        *time.Time
	UserID        string
	Locale        string
	Vars          map[string]string
}

// Work is a delivery claimed by a worker together with its event.
type Work struct {
	Delivery Delivery
	Event    Event
}

// CreateEvent writes the event and its deliveries in one transaction. A second call with the same
// idempotency key creates nothing and returns the first event (no deliveries).
func (s *Store) CreateEvent(ctx context.Context, ev NewEvent, dels []NewDelivery) (eventID string, deliveries []Delivery, err error) {
	id, deliveries, _, err := s.CreateEventWithInbox(ctx, ev, dels, nil)
	return id, deliveries, err
}

// CreateEventWithInbox also writes the inbox items in the same transaction and returns them (to publish).
func (s *Store) CreateEventWithInbox(ctx context.Context, ev NewEvent, dels []NewDelivery, inbox []NewInbox) (eventID string, deliveries []Delivery, items []InboxItem, err error) {
	data, err := json.Marshal(nonNil(ev.Data))
	if err != nil {
		return "", nil, nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", nil, nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	id := uuid.NewString()
	var key *string
	if ev.IdempotencyKey != "" {
		key = &ev.IdempotencyKey
	}
	tag, err := tx.Exec(ctx, `
INSERT INTO courier_events (id, idempotency_key, event_type, locale, data, correlation_id, request_id, user_id, tenant_id, agency_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (idempotency_key) DO NOTHING`,
		id, key, ev.EventType, ev.Locale, data, ev.CorrelationID, ev.RequestID, ev.UserID, ev.TenantID, ev.AgencyID)
	if err != nil {
		return "", nil, nil, err
	}
	if tag.RowsAffected() == 0 {
		var existing string
		if err := tx.QueryRow(ctx, `SELECT id FROM courier_events WHERE idempotency_key = $1`, ev.IdempotencyKey).Scan(&existing); err != nil {
			return "", nil, nil, err
		}
		return existing, nil, nil, nil
	}

	for _, d := range dels {
		did := uuid.NewString()
		vars, err := json.Marshal(nonNil(d.Vars))
		if err != nil {
			return "", nil, nil, err
		}
		status, reason := StatusPending, ""
		if d.Skip != "" {
			status, reason = StatusSkipped, d.Skip
		}
		var next *time.Time // nil: the database clock, now
		if !d.NotBefore.IsZero() {
			next = &d.NotBefore
		}
		digest := d.DigestKey
		if status != StatusPending {
			digest = "" // a skipped delivery is not part of a group
		}
		if digest != "" {
			attached, err := attachToLeader(ctx, tx, id, d)
			if err != nil {
				return "", nil, nil, err
			}
			if attached {
				continue
			}
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO courier_deliveries (id, event_id, channel, address, status, last_error, next_attempt_at, user_id, locale, vars, digest_key)
VALUES ($1, $2, $3, $4, $5, $6, COALESCE($7, now()), $8, $9, $10, $11) ON CONFLICT DO NOTHING`,
			did, id, d.Channel, d.Address, status, reason, next, d.UserID, d.Locale, vars, digest)
		if err != nil {
			return "", nil, nil, err
		}
		if tag.RowsAffected() == 0 {
			// a concurrent event just became the leader of the group: join it
			if attached, err := attachToLeader(ctx, tx, id, d); err != nil || !attached {
				if err == nil {
					err = errors.New("store: digest leader vanished")
				}
				return "", nil, nil, err
			}
			continue
		}
		// a skipped delivery is only the audit row: nothing to push
		if status == StatusSkipped {
			continue
		}
		deliveries = append(deliveries, Delivery{ID: did, EventID: id, Channel: d.Channel, Address: d.Address, Status: StatusPending,
			UserID: d.UserID, Locale: d.Locale, Vars: nonNil(d.Vars), NextAttemptAt: d.NotBefore})
	}
	for _, in := range inbox {
		it := InboxItem{ID: uuid.NewString(), UserID: in.UserID, EventID: id, EventType: ev.EventType, Title: in.Title, Body: in.Body, Link: in.Link, Severity: in.Severity}
		if err := tx.QueryRow(ctx, `
INSERT INTO courier_inbox (id, user_id, event_id, event_type, title, body, link, severity)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING created_at`,
			it.ID, it.UserID, id, it.EventType, it.Title, it.Body, it.Link, it.Severity).Scan(&it.CreatedAt); err != nil {
			return "", nil, nil, err
		}
		items = append(items, it)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", nil, nil, err
	}
	return id, deliveries, items, nil
}

// attachToLeader joins the event to the open delivery of the digest group, if there is one. The leader is locked
// until the commit: a worker that claims it meanwhile waits, and then sees the item.
func attachToLeader(ctx context.Context, tx pgx.Tx, eventID string, d NewDelivery) (bool, error) {
	var leader string
	err := tx.QueryRow(ctx, `
SELECT id::text FROM courier_deliveries
WHERE digest_key = $1 AND channel = $2 AND address = $3 AND status = 'pending' FOR UPDATE`, d.DigestKey, d.Channel, d.Address).Scan(&leader)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO courier_digest_items (delivery_id, event_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, leader, eventID)
	return err == nil, err
}

// GetEvent returns the event and all its deliveries.
func (s *Store) GetEvent(ctx context.Context, id string) (Event, []Delivery, error) {
	var ev Event
	var raw []byte
	err := s.pool.QueryRow(ctx, `
SELECT id, event_type, locale, data, correlation_id, request_id, user_id, tenant_id, agency_id, created_at
FROM courier_events WHERE id = $1`, id).
		Scan(&ev.ID, &ev.EventType, &ev.Locale, &raw, &ev.CorrelationID, &ev.RequestID, &ev.UserID, &ev.TenantID, &ev.AgencyID, &ev.CreatedAt)
	if err != nil {
		return Event{}, nil, mapErr(err)
	}
	ev.Data = decodeData(raw)

	rows, err := s.pool.Query(ctx, `
SELECT `+deliveryCols+` FROM courier_deliveries WHERE event_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return Event{}, nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return Event{}, nil, err
		}
		out = append(out, d)
	}
	return ev, out, rows.Err()
}

// Claim leases a pending delivery to one worker. It fails with ErrNotClaimable when the delivery is
// done or another worker holds the lease, so a duplicate stream message is a no-op.
func (s *Store) Claim(ctx context.Context, deliveryID string, lease time.Duration) (Work, error) {
	var w Work
	var raw, vars []byte
	err := s.pool.QueryRow(ctx, `
WITH d AS (
    UPDATE courier_deliveries SET locked_until = now() + make_interval(secs => $2), digest_key = '', updated_at = now()
    WHERE id = $1 AND status = 'pending' AND next_attempt_at <= now() AND (locked_until IS NULL OR locked_until < now())
    RETURNING id, event_id, channel, address, status, attempts, last_error, next_attempt_at, sent_at, user_id, locale, vars
)
SELECT d.id, d.event_id, d.channel, d.address, d.status, d.attempts, d.last_error, d.next_attempt_at, d.sent_at, d.user_id, d.locale, d.vars,
       e.id, e.event_type, e.locale, e.data, e.correlation_id, e.request_id, e.user_id, e.tenant_id, e.agency_id, e.created_at
FROM d JOIN courier_events e ON e.id = d.event_id`, deliveryID, lease.Seconds()).
		Scan(&w.Delivery.ID, &w.Delivery.EventID, &w.Delivery.Channel, &w.Delivery.Address, &w.Delivery.Status,
			&w.Delivery.Attempts, &w.Delivery.LastError, &w.Delivery.NextAttemptAt, &w.Delivery.SentAt, &w.Delivery.UserID, &w.Delivery.Locale, &vars,
			&w.Event.ID, &w.Event.EventType, &w.Event.Locale, &raw, &w.Event.CorrelationID, &w.Event.RequestID,
			&w.Event.UserID, &w.Event.TenantID, &w.Event.AgencyID, &w.Event.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Work{}, ErrNotClaimable
	}
	if err != nil {
		return Work{}, err
	}
	w.Event.Data = decodeData(raw)
	w.Delivery.Vars = decodeData(vars)
	return w, nil
}

// MarkSent closes the delivery and, when it was the last open one of the event, drops the template
// data (it may hold a reset link or a code).
func (s *Store) MarkSent(ctx context.Context, deliveryID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var eventID string
	if err := tx.QueryRow(ctx, `SELECT event_id::text FROM courier_deliveries WHERE id = $1`, deliveryID).Scan(&eventID); err != nil {
		return mapErr(err)
	}
	// Lock the events this delivery carries (its own and the attached ones, in id order) before closing it: two workers
	// that finish the last two deliveries of an event at the same time would each see the other one still open and
	// neither would drop the data. With the lock the second one waits and then sees the first one sent.
	if _, err := tx.Exec(ctx, `
SELECT id FROM courier_events
WHERE id = $1 OR id IN (SELECT event_id FROM courier_digest_items WHERE delivery_id = $2) ORDER BY id FOR UPDATE`, eventID, deliveryID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
UPDATE courier_deliveries
SET status = 'sent', attempts = attempts + 1, last_error = '', sent_at = now(), locked_until = NULL, updated_at = now()
WHERE id = $1`, deliveryID); err != nil {
		return err
	}
	// the data of an event goes when nothing is left to send for it: no open delivery of its own (a skipped one is
	// done) and no open digest that carries it; the events attached to this delivery are checked the same way
	if _, err := tx.Exec(ctx, `
UPDATE courier_events e SET data = '{}'
WHERE (e.id = $1 OR e.id IN (SELECT event_id FROM courier_digest_items WHERE delivery_id = $2))
  AND NOT EXISTS (SELECT 1 FROM courier_deliveries d WHERE d.event_id = e.id AND d.status NOT IN ('sent', 'skipped'))
  AND NOT EXISTS (SELECT 1 FROM courier_digest_items i JOIN courier_deliveries d ON d.id = i.delivery_id
                  WHERE i.event_id = e.id AND d.status NOT IN ('sent', 'skipped') AND d.id <> $2)`, eventID, deliveryID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// MarkFailed records a permanent error: no retry.
func (s *Store) MarkFailed(ctx context.Context, deliveryID, reason string) error {
	_, err := s.pool.Exec(ctx, `
UPDATE courier_deliveries
SET status = 'failed', attempts = attempts + 1, last_error = $2, locked_until = NULL, updated_at = now()
WHERE id = $1`, deliveryID, trunc(reason))
	return err
}

// MarkRetry records a transient error. The delivery goes back to pending with the next attempt at
// `next`, or to dead when it used maxAttempts; it returns the resulting status.
func (s *Store) MarkRetry(ctx context.Context, deliveryID, reason string, next time.Time, maxAttempts int) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx, `
UPDATE courier_deliveries
SET attempts = attempts + 1,
    status = CASE WHEN attempts + 1 >= $4 THEN 'dead' ELSE 'pending' END,
    last_error = $2, next_attempt_at = $3, enqueued_at = NULL, locked_until = NULL, updated_at = now()
WHERE id = $1 RETURNING status`, deliveryID, trunc(reason), next, maxAttempts).Scan(&status)
	return status, mapErr(err)
}

// Retry puts a failed or dead delivery back in the queue with a fresh attempt count.
func (s *Store) Retry(ctx context.Context, deliveryID string) (Delivery, error) {
	d, err := scanDelivery(s.pool.QueryRow(ctx, `
UPDATE courier_deliveries
SET status = 'pending', attempts = 0, next_attempt_at = now(), enqueued_at = NULL, locked_until = NULL, updated_at = now()
WHERE id = $1 AND status IN ('failed', 'dead')
RETURNING `+deliveryCols, deliveryID))
	if err == nil {
		return d, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Delivery{}, err
	}
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM courier_deliveries WHERE id = $1)`, deliveryID).Scan(&exists); err != nil {
		return Delivery{}, err
	}
	if exists {
		return Delivery{}, ErrNotRetryable
	}
	return Delivery{}, ErrNotFound
}

// MarkEnqueued stamps the deliveries just pushed to Redis (the fast path of SendNotification).
func (s *Store) MarkEnqueued(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE courier_deliveries SET enqueued_at = now() WHERE id = ANY($1) AND status = 'pending'`, ids)
	return err
}

// ClaimDue is the outbox read: it stamps and returns the pending deliveries that are due and were never
// pushed to Redis, or were pushed more than `stale` ago (a message lost with Redis). Concurrent relays
// get different rows.
func (s *Store) ClaimDue(ctx context.Context, limit int, stale time.Duration) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
UPDATE courier_deliveries SET enqueued_at = now()
WHERE id IN (
    SELECT id FROM courier_deliveries
    WHERE status = 'pending' AND next_attempt_at <= now()
      AND (enqueued_at IS NULL OR enqueued_at < now() - make_interval(secs => $2))
    ORDER BY next_attempt_at LIMIT $1 FOR UPDATE SKIP LOCKED)
RETURNING id`, limit, stale.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Release clears the outbox stamp of deliveries whose push to Redis failed, so the next relay pass retries.
func (s *Store) Release(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE courier_deliveries SET enqueued_at = NULL WHERE id = ANY($1) AND status = 'pending'`, ids)
	return err
}

// Purge deletes the events (and their deliveries) older than the cutoff, unless a delivery is still pending.
func (s *Store) Purge(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
DELETE FROM courier_events e
WHERE e.created_at < $1 AND NOT EXISTS (SELECT 1 FROM courier_deliveries d WHERE d.event_id = e.id AND d.status = 'pending')`, before)
	return tag.RowsAffected(), err
}

const deliveryCols = `id, event_id, channel, address, status, attempts, last_error, next_attempt_at, sent_at, user_id, locale, vars`

func scanDelivery(row pgx.Row) (Delivery, error) {
	var d Delivery
	var vars []byte
	err := row.Scan(&d.ID, &d.EventID, &d.Channel, &d.Address, &d.Status, &d.Attempts, &d.LastError, &d.NextAttemptAt, &d.SentAt, &d.UserID, &d.Locale, &vars)
	d.Vars = decodeData(vars)
	return d, mapErr(err)
}

func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func decodeData(raw []byte) map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal(raw, &m)
	return m
}

func trunc(s string) string {
	if len(s) > 1000 {
		return s[:1000]
	}
	return s
}

// Stats is what the metrics read from the database at a scrape.
type Stats struct {
	// Pending counts the deliveries waiting for a send or a retry, per channel.
	Pending map[string]int
	// Recent counts the deliveries created in the last 24 hours by status and channel (the log of the day).
	Recent map[[2]string]int
	// OldestDue is the age of the oldest pending delivery that is due now (0 when none): the sign of a stuck queue.
	OldestDue time.Duration
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	st := Stats{Pending: map[string]int{}, Recent: map[[2]string]int{}}
	rows, err := s.pool.Query(ctx, `
SELECT status, channel, count(*) FROM courier_deliveries
WHERE status = 'pending' OR created_at > now() - interval '24 hours' GROUP BY status, channel`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var status, channel string
		var n int
		if err := rows.Scan(&status, &channel, &n); err != nil {
			return st, err
		}
		if status == StatusPending {
			st.Pending[channel] = n
		}
		st.Recent[[2]string{status, channel}] += n
	}
	if err := rows.Err(); err != nil {
		return st, err
	}
	var age *float64
	if err := s.pool.QueryRow(ctx, `
SELECT extract(epoch FROM now() - min(next_attempt_at)) FROM courier_deliveries WHERE status = 'pending' AND next_attempt_at <= now()`).Scan(&age); err != nil {
		return st, err
	}
	if age != nil && *age > 0 {
		st.OldestDue = time.Duration(*age * float64(time.Second))
	}
	return st, nil
}
