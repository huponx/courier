package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"
)

// Pref is one user choice that differs from the default.
type Pref struct {
	Category string
	Channel  string
	Enabled  bool
}

// QuietHours is the window of a user, in minutes after midnight of the zone.
type QuietHours struct {
	Timezone string
	FromMin  int
	ToMin    int
}

// Prefs returns the choices of the users as user -> category -> channel -> enabled. A missing entry is on.
func (s *Store) Prefs(ctx context.Context, userIDs []string) (map[string]map[string]map[string]bool, error) {
	out := map[string]map[string]map[string]bool{}
	if len(userIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT user_id, category, channel, enabled FROM courier_user_prefs WHERE user_id = ANY($1)`, userIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u, c, ch string
		var on bool
		if err := rows.Scan(&u, &c, &ch, &on); err != nil {
			return nil, err
		}
		if out[u] == nil {
			out[u] = map[string]map[string]bool{}
		}
		if out[u][c] == nil {
			out[u][c] = map[string]bool{}
		}
		out[u][c][ch] = on
	}
	return out, rows.Err()
}

// SetPrefs saves the choices of one user in one transaction: a row per category and channel, and "on" deletes the
// row, as it is the default.
func (s *Store) SetPrefs(ctx context.Context, userID string, prefs []Pref) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, p := range prefs {
		if p.Enabled {
			_, err = tx.Exec(ctx, `DELETE FROM courier_user_prefs WHERE user_id = $1 AND category = $2 AND channel = $3`, userID, p.Category, p.Channel)
		} else {
			_, err = tx.Exec(ctx, `
INSERT INTO courier_user_prefs (user_id, category, channel, enabled) VALUES ($1, $2, $3, false)
ON CONFLICT (user_id, category, channel) DO UPDATE SET enabled = false, updated_at = now()`, userID, p.Category, p.Channel)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Quiet returns the quiet hours of the users (a user without a window is absent).
func (s *Store) Quiet(ctx context.Context, userIDs []string) (map[string]QuietHours, error) {
	out := map[string]QuietHours{}
	if len(userIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT user_id, tz, from_min, to_min FROM courier_user_quiet_hours WHERE user_id = ANY($1)`, userIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u string
		var q QuietHours
		if err := rows.Scan(&u, &q.Timezone, &q.FromMin, &q.ToMin); err != nil {
			return nil, err
		}
		out[u] = q
	}
	return out, rows.Err()
}

// SetQuiet saves the window; nil removes it.
func (s *Store) SetQuiet(ctx context.Context, userID string, q *QuietHours) error {
	if q == nil {
		_, err := s.pool.Exec(ctx, `DELETE FROM courier_user_quiet_hours WHERE user_id = $1`, userID)
		return err
	}
	_, err := s.pool.Exec(ctx, `
INSERT INTO courier_user_quiet_hours (user_id, tz, from_min, to_min) VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id) DO UPDATE SET tz = $2, from_min = $3, to_min = $4, updated_at = now()`, userID, q.Timezone, q.FromMin, q.ToMin)
	return err
}

// Bindings returns the address of the users on the channel (user -> address); an unbound user is absent.
func (s *Store) Bindings(ctx context.Context, userIDs []string, channel string) (map[string]string, error) {
	out := map[string]string{}
	if len(userIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT user_id, address FROM courier_channel_bindings WHERE user_id = ANY($1) AND channel = $2`, userIDs, channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u, a string
		if err := rows.Scan(&u, &a); err != nil {
			return nil, err
		}
		out[u] = a
	}
	return out, rows.Err()
}

func (s *Store) Unbind(ctx context.Context, userID, channel string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM courier_channel_bindings WHERE user_id = $1 AND channel = $2`, userID, channel)
	return err
}

// UnbindAddress removes the binding of the address (the chat blocked the bot): whoever it was linked to.
func (s *Store) UnbindAddress(ctx context.Context, channel, address string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM courier_channel_bindings WHERE channel = $1 AND address = $2`, channel, address)
	return err
}

var ErrCodeInvalid = errors.New("store: binding code is unknown or expired")

const codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O/1/I: read aloud and typed by hand

// NewBindingCode creates a one-time code of the user (the older ones of the user are dropped) valid for ttl.
func (s *Store) NewBindingCode(ctx context.Context, userID, channel string, ttl time.Duration) (string, time.Time, error) {
	code := make([]byte, 10)
	for i := range code {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(codeAlphabet))))
		if err != nil {
			return "", time.Time{}, err
		}
		code[i] = codeAlphabet[n.Int64()]
	}
	exp := time.Now().Add(ttl)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM courier_binding_codes WHERE user_id = $1 AND channel = $2 OR expires_at < now()`, userID, channel); err != nil {
		return "", time.Time{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO courier_binding_codes (code, user_id, channel, expires_at) VALUES ($1, $2, $3, $4)`, string(code), userID, channel, exp); err != nil {
		return "", time.Time{}, err
	}
	return string(code), exp, tx.Commit(ctx)
}

// ConsumeBindingCode uses the code once: it binds the address to the user of the code (replacing an older binding of
// that user, and taking the address from another user) and returns the user.
func (s *Store) ConsumeBindingCode(ctx context.Context, channel, code, address string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	var userID string
	err = tx.QueryRow(ctx, `DELETE FROM courier_binding_codes WHERE code = $1 AND channel = $2 AND expires_at > now() RETURNING user_id`, code, channel).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrCodeInvalid
	}
	if err != nil {
		return "", err
	}
	// a chat belongs to one user
	if _, err := tx.Exec(ctx, `DELETE FROM courier_channel_bindings WHERE channel = $1 AND address = $2 AND user_id <> $3`, channel, address, userID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO courier_channel_bindings (user_id, channel, address) VALUES ($1, $2, $3)
ON CONFLICT (user_id, channel) DO UPDATE SET address = $3, verified_at = now()`, userID, channel, address); err != nil {
		return "", fmt.Errorf("bind: %w", err)
	}
	return userID, tx.Commit(ctx)
}

// Leader takes the advisory lock `key` on a connection of its own. Exactly one holder exists at a time (across
// workers); the lock is released by the returned function, or by PostgreSQL when the process dies.
func (s *Store) Leader(ctx context.Context, key int64) (release func(), ok bool, err error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&ok); err != nil || !ok {
		conn.Release()
		return nil, false, err
	}
	return func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
		conn.Release()
	}, true, nil
}
