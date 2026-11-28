package store

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// PushSub is the subscription of one browser.
type PushSub struct {
	ID        string
	UserID    string
	Endpoint  string
	P256dh    string
	Auth      string
	UserAgent string
	CreatedAt time.Time
}

// maxPushPerUser bounds the devices of one user; the oldest subscription goes when a new one would exceed it.
const maxPushPerUser = 10

// SavePush stores the subscription; the same endpoint again is updated and belongs to the user from then on (the
// browser is now theirs).
func (s *Store) SavePush(ctx context.Context, sub PushSub) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `
INSERT INTO courier_push_subscriptions (id, user_id, endpoint, p256dh, auth, user_agent) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (endpoint) DO UPDATE SET user_id = $2, p256dh = $4, auth = $5, user_agent = $6`,
		uuid.NewString(), sub.UserID, sub.Endpoint, sub.P256dh, sub.Auth, sub.UserAgent); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
DELETE FROM courier_push_subscriptions WHERE user_id = $1 AND id NOT IN (
    SELECT id FROM courier_push_subscriptions WHERE user_id = $1 ORDER BY created_at DESC, id LIMIT $2)`, sub.UserID, maxPushPerUser); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PushSubs returns the subscriptions of the users, oldest first.
func (s *Store) PushSubs(ctx context.Context, userIDs []string) (map[string][]PushSub, error) {
	out := map[string][]PushSub{}
	if len(userIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
SELECT id::text, user_id, endpoint, p256dh, auth, user_agent, created_at FROM courier_push_subscriptions
WHERE user_id = ANY($1) ORDER BY created_at, id`, userIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p PushSub
		if err := rows.Scan(&p.ID, &p.UserID, &p.Endpoint, &p.P256dh, &p.Auth, &p.UserAgent, &p.CreatedAt); err != nil {
			return nil, err
		}
		out[p.UserID] = append(out[p.UserID], p)
	}
	return out, rows.Err()
}

// PushSub returns one subscription by id, or ErrNotFound.
func (s *Store) PushSub(ctx context.Context, id string) (PushSub, error) {
	if _, err := uuid.Parse(id); err != nil {
		return PushSub{}, ErrNotFound
	}
	var p PushSub
	err := s.pool.QueryRow(ctx, `
SELECT id::text, user_id, endpoint, p256dh, auth, user_agent, created_at FROM courier_push_subscriptions WHERE id = $1`, id).
		Scan(&p.ID, &p.UserID, &p.Endpoint, &p.P256dh, &p.Auth, &p.UserAgent, &p.CreatedAt)
	return p, mapErr(err)
}

// DeletePush removes the subscription of the endpoint of that user.
func (s *Store) DeletePush(ctx context.Context, userID, endpoint string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM courier_push_subscriptions WHERE user_id = $1 AND endpoint = $2`, userID, endpoint)
	return err
}

// DeletePushByID removes a subscription the push service reported gone.
func (s *Store) DeletePushByID(ctx context.Context, id string) error {
	if _, err := uuid.Parse(id); err != nil {
		return nil
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM courier_push_subscriptions WHERE id = $1`, id)
	return err
}
