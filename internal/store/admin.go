package store

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TemplateRow is an edited template: the files of one channel and locale of a template directory.
type TemplateRow struct {
	Template  string
	Locale    string
	Channel   string
	Version   int
	Files     map[string]string
	UpdatedBy string
	UpdatedAt time.Time
}

// TemplateVersion is one entry of the history; Files is nil for a revert.
type TemplateVersion struct {
	Version   int
	Files     map[string]string
	UpdatedBy string
	UpdatedAt time.Time
}

// Templates returns every edited template (the sending processes load them all: there are few).
func (s *Store) Templates(ctx context.Context) ([]TemplateRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT template, locale, channel, version, files, updated_by, updated_at FROM courier_templates`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TemplateRow
	for rows.Next() {
		var r TemplateRow
		var raw []byte
		if err := rows.Scan(&r.Template, &r.Locale, &r.Channel, &r.Version, &raw, &r.UpdatedBy, &r.UpdatedAt); err != nil {
			return nil, err
		}
		r.Files = decodeData(raw)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Template returns the edited template, or ErrNotFound when the built-in one is in use.
func (s *Store) Template(ctx context.Context, template, locale, channel string) (TemplateRow, error) {
	var r TemplateRow
	var raw []byte
	err := s.pool.QueryRow(ctx, `
SELECT template, locale, channel, version, files, updated_by, updated_at FROM courier_templates
WHERE template = $1 AND locale = $2 AND channel = $3`, template, locale, channel).
		Scan(&r.Template, &r.Locale, &r.Channel, &r.Version, &raw, &r.UpdatedBy, &r.UpdatedAt)
	if err != nil {
		return TemplateRow{}, mapErr(err)
	}
	r.Files = decodeData(raw)
	return r, nil
}

// SaveTemplate makes the files the current template and a new entry of the history; it returns the version.
func (s *Store) SaveTemplate(ctx context.Context, template, locale, channel string, files map[string]string, by string) (int, error) {
	raw, err := json.Marshal(files)
	if err != nil {
		return 0, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := lockTemplate(ctx, tx, template, locale, channel); err != nil {
		return 0, err
	}
	var v int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM courier_template_versions WHERE template = $1 AND locale = $2 AND channel = $3`,
		template, locale, channel).Scan(&v); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO courier_template_versions (template, locale, channel, version, files, updated_by) VALUES ($1, $2, $3, $4, $5, $6)`,
		template, locale, channel, v, raw, by); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO courier_templates (template, locale, channel, version, files, updated_by) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (template, locale, channel) DO UPDATE SET version = $4, files = $5, updated_by = $6, updated_at = now()`,
		template, locale, channel, v, raw, by); err != nil {
		return 0, err
	}
	return v, tx.Commit(ctx)
}

// RevertTemplate drops the edited template (the built-in one is used again) and records it in the history.
// ErrNotFound when nothing was edited.
func (s *Store) RevertTemplate(ctx context.Context, template, locale, channel, by string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := lockTemplate(ctx, tx, template, locale, channel); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM courier_templates WHERE template = $1 AND locale = $2 AND channel = $3`, template, locale, channel)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO courier_template_versions (template, locale, channel, version, files, updated_by)
SELECT $1, $2, $3, COALESCE(max(version), 0) + 1, NULL, $4 FROM courier_template_versions WHERE template = $1 AND locale = $2 AND channel = $3`,
		template, locale, channel, by); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func lockTemplate(ctx context.Context, tx pgx.Tx, template, locale, channel string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "courier-template|"+template+"|"+locale+"|"+channel)
	return err
}

// TemplateVersions returns the history of a template, newest first.
func (s *Store) TemplateVersions(ctx context.Context, template, locale, channel string) ([]TemplateVersion, error) {
	rows, err := s.pool.Query(ctx, `
SELECT version, files, updated_by, updated_at FROM courier_template_versions
WHERE template = $1 AND locale = $2 AND channel = $3 ORDER BY version DESC`, template, locale, channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TemplateVersion
	for rows.Next() {
		var v TemplateVersion
		var raw []byte
		if err := rows.Scan(&v.Version, &raw, &v.UpdatedBy, &v.UpdatedAt); err != nil {
			return nil, err
		}
		if raw != nil {
			v.Files = decodeData(raw)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Broadcast is an announcement of an administrator.
type Broadcast struct {
	ID         string
	EventType  string
	Audience   json.RawMessage
	Data       map[string]string
	CreatedBy  string
	CreatedAt  time.Time
	Recipients int
	Skipped    int
	// Channels it was sent on; empty: every channel of the type.
	Channels []string
}

func (s *Store) InsertBroadcast(ctx context.Context, b Broadcast) (Broadcast, error) {
	if b.ID == "" {
		b.ID = uuid.NewString()
	}
	data, err := json.Marshal(nonNil(b.Data))
	if err != nil {
		return b, err
	}
	err = s.pool.QueryRow(ctx, `
INSERT INTO courier_broadcasts (id, event_type, audience, data, created_by, recipients, skipped, channels) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING created_at`, b.ID, b.EventType, b.Audience, data, b.CreatedBy, b.Recipients, b.Skipped, nonNilList(b.Channels)).Scan(&b.CreatedAt)
	return b, err
}

// ListBroadcasts returns a page of announcements, newest first.
func (s *Store) ListBroadcasts(ctx context.Context, limit int, cursor string) ([]Broadcast, string, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	args := []any{limit + 1}
	where := "true"
	if cursor != "" {
		at, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		args = append(args, at, id)
		where = "(created_at, id::text) < ($2, $3)"
	}
	rows, err := s.pool.Query(ctx, `
SELECT id::text, event_type, audience, created_by, created_at, recipients, skipped, channels FROM courier_broadcasts
WHERE `+where+` ORDER BY created_at DESC, id DESC LIMIT $1`, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []Broadcast
	for rows.Next() {
		var b Broadcast
		if err := rows.Scan(&b.ID, &b.EventType, &b.Audience, &b.CreatedBy, &b.CreatedAt, &b.Recipients, &b.Skipped, &b.Channels); err != nil {
			return nil, "", err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, next, nil
}

// DeliveryFilter narrows the delivery log; an empty field matches everything.
type DeliveryFilter struct {
	Status         string
	Channel        string
	NotificationID string
	UserID         string
	Address        string // part of it, case insensitive
}

// DeliveryRecord is a delivery with the event it belongs to.
type DeliveryRecord struct {
	Delivery       Delivery
	NotificationID string
	EventType      string
	CreatedAt      time.Time
	// Merged is how many other events went out with this delivery (a digest).
	Merged int
}

// ListDeliveries returns a page of the delivery log, newest first.
func (s *Store) ListDeliveries(ctx context.Context, f DeliveryFilter, limit int, cursor string) ([]DeliveryRecord, string, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	args := []any{limit + 1}
	var where []string
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.ReplaceAll(cond, "?", "$"+strconv.Itoa(len(args))))
	}
	if f.Status != "" {
		add("d.status = ?", f.Status)
	}
	if f.Channel != "" {
		add("d.channel = ?", f.Channel)
	}
	if f.NotificationID != "" {
		add("d.event_id::text = ?", f.NotificationID)
	}
	if f.UserID != "" {
		add("d.user_id = ?", f.UserID)
	}
	if f.Address != "" {
		add("d.address ILIKE ?", "%"+strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(f.Address)+"%")
	}
	if cursor != "" {
		at, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		args = append(args, at, id)
		where = append(where, "(d.created_at, d.id::text) < ($"+strconv.Itoa(len(args)-1)+", $"+strconv.Itoa(len(args))+")")
	}
	cond := "true"
	if len(where) > 0 {
		cond = strings.Join(where, " AND ")
	}
	rows, err := s.pool.Query(ctx, `
SELECT d.id, d.event_id, d.channel, d.address, d.status, d.attempts, d.last_error, d.next_attempt_at, d.sent_at, d.user_id, d.locale, d.created_at, e.event_type,
       (SELECT count(*) FROM courier_digest_items i WHERE i.delivery_id = d.id)
FROM courier_deliveries d JOIN courier_events e ON e.id = d.event_id
WHERE `+cond+` ORDER BY d.created_at DESC, d.id DESC LIMIT $1`, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []DeliveryRecord
	for rows.Next() {
		var r DeliveryRecord
		d := &r.Delivery
		if err := rows.Scan(&d.ID, &d.EventID, &d.Channel, &d.Address, &d.Status, &d.Attempts, &d.LastError, &d.NextAttemptAt, &d.SentAt, &d.UserID, &d.Locale, &r.CreatedAt, &r.EventType, &r.Merged); err != nil {
			return nil, "", err
		}
		r.NotificationID = d.EventID
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		next = encodeCursor(last.CreatedAt, last.Delivery.ID)
	}
	return out, next, nil
}

// DigestItems returns the events attached to a delivery (a digest), oldest first; the event of the delivery itself is
// not among them.
func (s *Store) DigestItems(ctx context.Context, deliveryID string) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `
SELECT e.id::text, e.event_type, e.locale, e.data, e.created_at
FROM courier_digest_items i JOIN courier_events e ON e.id = i.event_id
WHERE i.delivery_id = $1 ORDER BY i.created_at, e.id`, deliveryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var raw []byte
		if err := rows.Scan(&e.ID, &e.EventType, &e.Locale, &raw, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Data = decodeData(raw)
		out = append(out, e)
	}
	return out, rows.Err()
}

func nonNilList(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}
