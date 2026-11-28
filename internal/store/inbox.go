package store

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/huponx/gocommon/pagination"
)

type NewInbox struct {
	UserID   string
	Title    string
	Body     string
	Link     string
	Severity string // info | warning | critical
}

type InboxItem struct {
	ID        string
	UserID    string
	EventID   string
	EventType string
	Title     string
	Body      string
	Link      string
	Severity  string
	Read      bool
	CreatedAt time.Time
}

var ErrBadCursor = errors.New("store: bad page token")

// ListInbox returns one page of the not archived items of the user, newest first, and the cursor of the next page
// (empty at the end). The cursor is the (created_at, id) of the last item, so new items do not shift the pages.
func (s *Store) ListInbox(ctx context.Context, userID string, limit int, cursor string, unreadOnly bool) ([]InboxItem, string, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	args := []any{userID, limit + 1}
	where := "user_id = $1 AND archived_at IS NULL"
	if unreadOnly {
		where += " AND read_at IS NULL"
	}
	if cursor != "" {
		at, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		args = append(args, at, id)
		where += " AND (created_at, id) < ($3, $4)"
	}
	rows, err := s.pool.Query(ctx, `
SELECT id, user_id, COALESCE(event_id::text, ''), event_type, title, body, link, severity, read_at IS NOT NULL, created_at
FROM courier_inbox WHERE `+where+` ORDER BY created_at DESC, id DESC LIMIT $2`, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []InboxItem
	for rows.Next() {
		var it InboxItem
		if err := rows.Scan(&it.ID, &it.UserID, &it.EventID, &it.EventType, &it.Title, &it.Body, &it.Link, &it.Severity, &it.Read, &it.CreatedAt); err != nil {
			return nil, "", err
		}
		out = append(out, it)
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

// Unread counts the items of the user that are neither read nor archived.
func (s *Store) Unread(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM courier_inbox WHERE user_id = $1 AND read_at IS NULL AND archived_at IS NULL`, userID).Scan(&n)
	return n, err
}

// MarkRead marks the given items of the user (all=true: every unread one). Ids of other users match nothing.
func (s *Store) MarkRead(ctx context.Context, userID string, ids []string, all bool) error {
	if all {
		_, err := s.pool.Exec(ctx, `UPDATE courier_inbox SET read_at = now() WHERE user_id = $1 AND read_at IS NULL`, userID)
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE courier_inbox SET read_at = now() WHERE user_id = $1 AND id::text = ANY($2) AND read_at IS NULL`, userID, ids)
	return err
}

// Archive hides the given items of the user from the list (an archived item also leaves the unread count).
func (s *Store) Archive(ctx context.Context, userID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `UPDATE courier_inbox SET archived_at = now() WHERE user_id = $1 AND id::text = ANY($2) AND archived_at IS NULL`, userID, ids)
	return err
}

// PurgeInbox deletes the items older than the cutoff.
func (s *Store) PurgeInbox(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM courier_inbox WHERE created_at < $1`, before)
	return tag.RowsAffected(), err
}

// encodeCursor and decodeCursor make the page token of a keyset (time, id) with the shared token of gocommon/pagination.
func encodeCursor(at time.Time, id string) string {
	return pagination.Encode(strconv.FormatInt(at.UnixMicro(), 10), id)
}

func decodeCursor(c string) (time.Time, string, error) {
	parts, err := pagination.Decode(c, 2)
	if err != nil || parts[1] == "" {
		return time.Time{}, "", ErrBadCursor
	}
	n, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, "", ErrBadCursor
	}
	return time.UnixMicro(n).UTC(), parts[1], nil
}
