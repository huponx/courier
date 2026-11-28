package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestStore needs a PostgreSQL (COURIER_TEST_POSTGRES_URL, for example the one of db-migration's
// docker compose). The tables come from the real migration, created in a schema of their own.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("COURIER_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("COURIER_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	schema := "courier_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")

	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	var sql []byte
	for _, f := range []string{"V41__courier_notifications.sql", "V42__courier_delivery_recipient.sql", "V43__courier_inbox.sql", "V44__courier_preferences.sql", "V45__courier_templates_broadcasts.sql", "V46__courier_webhooks.sql", "V47__courier_push.sql", "V48__courier_digest.sql", "V50__courier_broadcast_channels.sql"} {
		b, err := os.ReadFile("../../../db-migration/migrations/sql/" + f)
		if err != nil {
			t.Fatal(err)
		}
		sql = append(sql, b...)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	st, err := New(ctx, url+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := st.pool.Exec(ctx, string(sql)); err != nil {
		t.Fatal(err)
	}
	return st
}

func newEvent(key string) NewEvent {
	return NewEvent{IdempotencyKey: key, EventType: "hello", Locale: "en", Data: map[string]string{"link": "https://x/reset?t=secret"}}
}

func TestCreateEventIdempotent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	dels := []NewDelivery{{Channel: "email", Address: "a@x.io"}, {Channel: "telegram", Address: "42"}}

	id1, d1, err := st.CreateEvent(ctx, newEvent("k1"), dels)
	if err != nil || len(d1) != 2 {
		t.Fatalf("first: %v %v", err, d1)
	}
	id2, d2, err := st.CreateEvent(ctx, newEvent("k1"), dels)
	if err != nil || id2 != id1 || len(d2) != 0 {
		t.Fatalf("duplicate must return the first event and no deliveries: %s %s %v %v", id1, id2, d2, err)
	}
	// no key: every call is a new event
	a, _, _ := st.CreateEvent(ctx, newEvent(""), dels[:1])
	b, _, _ := st.CreateEvent(ctx, newEvent(""), dels[:1])
	if a == b {
		t.Fatal("events without a key must not collapse")
	}
}

func TestRetryToDeadAndManualRetry(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	_, dels, _ := st.CreateEvent(ctx, newEvent(""), []NewDelivery{{Channel: "email", Address: "a@x.io"}})
	id := dels[0].ID

	for i := 1; i <= 3; i++ {
		w, err := st.Claim(ctx, id, time.Minute)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if w.Event.Data["link"] == "" {
			t.Fatal("event data must reach the worker")
		}
		status, err := st.MarkRetry(ctx, id, "smtp down", time.Now().Add(-time.Second), 3)
		if err != nil {
			t.Fatal(err)
		}
		want := StatusPending
		if i == 3 {
			want = StatusDead
		}
		if status != want {
			t.Fatalf("attempt %d: status %s, want %s", i, status, want)
		}
	}
	if _, err := st.Claim(ctx, id, time.Minute); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("dead delivery must not be claimable: %v", err)
	}

	d, err := st.Retry(ctx, id)
	if err != nil || d.Status != StatusPending || d.Attempts != 0 {
		t.Fatalf("retry: %+v %v", d, err)
	}
	if _, err := st.Retry(ctx, id); !errors.Is(err, ErrNotRetryable) {
		t.Fatalf("retry of a pending delivery: %v", err)
	}
	if _, err := st.Retry(ctx, uuid.NewString()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("retry of an unknown delivery: %v", err)
	}
}

func TestClaimLeaseExcludesSecondWorker(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	_, dels, _ := st.CreateEvent(ctx, newEvent(""), []NewDelivery{{Channel: "email", Address: "a@x.io"}})
	if _, err := st.Claim(ctx, dels[0].ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Claim(ctx, dels[0].ID, time.Minute); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("second claim while leased: %v", err)
	}
}

func TestMarkSentDropsDataWhenLastDeliveryDone(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, dels, _ := st.CreateEvent(ctx, newEvent(""), []NewDelivery{{Channel: "email", Address: "a@x.io"}, {Channel: "telegram", Address: "1"}})

	_, _ = st.Claim(ctx, dels[0].ID, time.Minute)
	if err := st.MarkSent(ctx, dels[0].ID); err != nil {
		t.Fatal(err)
	}
	ev, _, _ := st.GetEvent(ctx, id)
	if len(ev.Data) == 0 {
		t.Fatal("data is still needed by the second delivery")
	}
	_, _ = st.Claim(ctx, dels[1].ID, time.Minute)
	if err := st.MarkSent(ctx, dels[1].ID); err != nil {
		t.Fatal(err)
	}
	ev, got, _ := st.GetEvent(ctx, id)
	if len(ev.Data) != 0 {
		t.Fatalf("data must be dropped after the last send: %v", ev.Data)
	}
	if got[0].Status != StatusSent || got[0].SentAt == nil {
		t.Fatalf("delivery not marked sent: %+v", got[0])
	}
}

func TestOutboxClaimDue(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	_, dels, _ := st.CreateEvent(ctx, newEvent(""), []NewDelivery{{Channel: "email", Address: "a@x.io"}})
	id := dels[0].ID

	ids, err := st.ClaimDue(ctx, 10, time.Hour)
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("never pushed row must be due: %v %v", ids, err)
	}
	if ids, _ := st.ClaimDue(ctx, 10, time.Hour); len(ids) != 0 {
		t.Fatalf("just pushed row is not due again: %v", ids)
	}
	if err := st.Release(ctx, id); err != nil {
		t.Fatal(err)
	}
	if ids, _ := st.ClaimDue(ctx, 10, time.Hour); len(ids) != 1 {
		t.Fatalf("released row must be due: %v", ids)
	}
	// Redis lost the message: stale window passed
	if _, err := st.pool.Exec(ctx, `UPDATE courier_deliveries SET enqueued_at = now() - interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	if ids, _ := st.ClaimDue(ctx, 10, time.Hour); len(ids) != 1 {
		t.Fatalf("stale row must be pushed again: %v", ids)
	}
	// a retry in the future is not due
	_, _ = st.Claim(ctx, id, time.Minute)
	_, _ = st.MarkRetry(ctx, id, "e", time.Now().Add(time.Hour), 5)
	if ids, _ := st.ClaimDue(ctx, 10, time.Hour); len(ids) != 0 {
		t.Fatalf("future retry must wait: %v", ids)
	}
}

func TestPurgeKeepsPending(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	oldDone, dels, _ := st.CreateEvent(ctx, newEvent(""), []NewDelivery{{Channel: "email", Address: "a@x.io"}})
	_, _ = st.Claim(ctx, dels[0].ID, time.Minute)
	_ = st.MarkSent(ctx, dels[0].ID)
	oldPending, _, _ := st.CreateEvent(ctx, newEvent(""), []NewDelivery{{Channel: "email", Address: "b@x.io"}})
	if _, err := st.pool.Exec(ctx, `UPDATE courier_events SET created_at = now() - interval '100 days'`); err != nil {
		t.Fatal(err)
	}
	n, err := st.Purge(ctx, time.Now().Add(-90*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("purge: %d %v", n, err)
	}
	if _, _, err := st.GetEvent(ctx, oldDone); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finished old event must be gone: %v", err)
	}
	if _, _, err := st.GetEvent(ctx, oldPending); err != nil {
		t.Fatal(fmt.Errorf("pending old event must stay: %w", err))
	}
}

func TestDeliveryRecipientFieldsReachWorker(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	_, dels, err := st.CreateEvent(ctx, newEvent(""), []NewDelivery{
		{Channel: "email", Address: "a@x.io", UserID: "u1", Locale: "vi", Vars: map[string]string{"name": "An"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, err := st.Claim(ctx, dels[0].ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if w.Delivery.UserID != "u1" || w.Delivery.Locale != "vi" || w.Delivery.Vars["name"] != "An" {
		t.Fatalf("recipient fields lost: %+v", w.Delivery)
	}
}

func TestInbox(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _, items, err := st.CreateEventWithInbox(ctx, newEvent(""), nil, []NewInbox{
			{UserID: "u1", Title: fmt.Sprintf("t%d", i), Severity: "info"},
			{UserID: "u2", Title: "other", Severity: "warning"},
		})
		if err != nil || len(items) != 2 || items[0].CreatedAt.IsZero() {
			t.Fatalf("create: %v %v", items, err)
		}
	}
	if n, _ := st.Unread(ctx, "u1"); n != 5 {
		t.Fatalf("unread %d", n)
	}

	// pages of 2, newest first, no overlap
	var titles []string
	cursor := ""
	for page := 0; ; page++ {
		items, next, err := st.ListInbox(ctx, "u1", 2, cursor, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			if it.UserID != "u1" {
				t.Fatal("another user's item in the list")
			}
			titles = append(titles, it.Title)
		}
		if next == "" {
			break
		}
		if page > 5 {
			t.Fatal("cursor loops")
		}
		cursor = next
	}
	if len(titles) != 5 || titles[0] != "t4" || titles[4] != "t0" {
		t.Fatalf("pages: %v", titles)
	}
	if _, _, err := st.ListInbox(ctx, "u1", 2, "garbage", false); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("bad cursor: %v", err)
	}

	items, _, _ := st.ListInbox(ctx, "u1", 10, "", false)
	other, _, _ := st.ListInbox(ctx, "u2", 10, "", false)
	// a user cannot mark or archive the item of somebody else
	_ = st.MarkRead(ctx, "u1", []string{other[0].ID}, false)
	_ = st.Archive(ctx, "u1", []string{other[0].ID})
	if n, _ := st.Unread(ctx, "u2"); n != 5 {
		t.Fatalf("u1 touched the inbox of u2: %d", n)
	}

	_ = st.MarkRead(ctx, "u1", []string{items[0].ID}, false)
	if n, _ := st.Unread(ctx, "u1"); n != 4 {
		t.Fatalf("after one read: %d", n)
	}
	_ = st.Archive(ctx, "u1", []string{items[1].ID}) // unread and archived: leaves the count
	if n, _ := st.Unread(ctx, "u1"); n != 3 {
		t.Fatalf("after archive: %d", n)
	}
	if list, _, _ := st.ListInbox(ctx, "u1", 10, "", false); len(list) != 4 {
		t.Fatalf("archived item still listed: %d", len(list))
	}
	if list, _, _ := st.ListInbox(ctx, "u1", 10, "", true); len(list) != 3 {
		t.Fatalf("unread only: %d", len(list))
	}
	_ = st.MarkRead(ctx, "u1", nil, true)
	if n, _ := st.Unread(ctx, "u1"); n != 0 {
		t.Fatalf("after read all: %d", n)
	}

	if _, err := st.pool.Exec(ctx, `UPDATE courier_inbox SET created_at = now() - interval '400 days' WHERE user_id = 'u2'`); err != nil {
		t.Fatal(err)
	}
	if n, err := st.PurgeInbox(ctx, time.Now().Add(-365*24*time.Hour)); err != nil || n != 5 {
		t.Fatalf("purge: %d %v", n, err)
	}
}

func TestPrefsAndQuietHours(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if err := st.SetPrefs(ctx, "u1", []Pref{{"admin", "email", false}, {"admin", "in_app", true}}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Prefs(ctx, []string{"u1", "u2"})
	if on, set := got["u1"]["admin"]["email"]; !set || on {
		t.Fatalf("email must be off: %v", got)
	}
	if _, set := got["u1"]["admin"]["in_app"]; set {
		t.Fatalf("on is the default and keeps no row: %v", got)
	}
	if len(got["u2"]) != 0 {
		t.Fatalf("u2 chose nothing: %v", got)
	}
	_ = st.SetPrefs(ctx, "u1", []Pref{{"admin", "email", true}})
	if got, _ := st.Prefs(ctx, []string{"u1"}); len(got["u1"]) != 0 {
		t.Fatalf("turning it on again removes the row: %v", got)
	}

	q := &QuietHours{Timezone: "Asia/Ho_Chi_Minh", FromMin: 1320, ToMin: 420}
	if err := st.SetQuiet(ctx, "u1", q); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Quiet(ctx, []string{"u1"}); got["u1"] != *q {
		t.Fatalf("quiet: %v", got)
	}
	if err := st.SetQuiet(ctx, "u1", &QuietHours{Timezone: "UTC", FromMin: 5, ToMin: 5}); err == nil {
		t.Fatal("an empty window must be refused by the database")
	}
	_ = st.SetQuiet(ctx, "u1", nil)
	if got, _ := st.Quiet(ctx, []string{"u1"}); len(got) != 0 {
		t.Fatalf("quiet not removed: %v", got)
	}
}

func TestBindingCodes(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	code, exp, err := st.NewBindingCode(ctx, "u1", "telegram", time.Minute)
	if err != nil || len(code) != 10 || !exp.After(time.Now()) {
		t.Fatalf("%q %v %v", code, exp, err)
	}
	// a second code replaces the first
	code2, _, _ := st.NewBindingCode(ctx, "u1", "telegram", time.Minute)
	if _, err := st.ConsumeBindingCode(ctx, "telegram", code, "100"); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("old code: %v", err)
	}
	uid, err := st.ConsumeBindingCode(ctx, "telegram", code2, "100")
	if err != nil || uid != "u1" {
		t.Fatalf("consume: %q %v", uid, err)
	}
	if _, err := st.ConsumeBindingCode(ctx, "telegram", code2, "100"); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("a code works once: %v", err)
	}
	if b, _ := st.Bindings(ctx, []string{"u1"}, "telegram"); b["u1"] != "100" {
		t.Fatalf("bindings: %v", b)
	}

	// a chat belongs to one user: u2 links the same chat, u1 loses it
	c3, _, _ := st.NewBindingCode(ctx, "u2", "telegram", time.Minute)
	if _, err := st.ConsumeBindingCode(ctx, "telegram", c3, "100"); err != nil {
		t.Fatal(err)
	}
	b, _ := st.Bindings(ctx, []string{"u1", "u2"}, "telegram")
	if _, has := b["u1"]; has || b["u2"] != "100" {
		t.Fatalf("the chat moved to u2: %v", b)
	}

	// expired
	c4, _, _ := st.NewBindingCode(ctx, "u3", "telegram", time.Minute)
	if _, err := st.pool.Exec(ctx, `UPDATE courier_binding_codes SET expires_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConsumeBindingCode(ctx, "telegram", c4, "200"); !errors.Is(err, ErrCodeInvalid) {
		t.Fatalf("expired: %v", err)
	}

	// the chat blocked the bot
	_ = st.UnbindAddress(ctx, "telegram", "100")
	if b, _ := st.Bindings(ctx, []string{"u2"}, "telegram"); len(b) != 0 {
		t.Fatalf("not unbound: %v", b)
	}
}

func TestSkippedAndDelayedDeliveries(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	_, dels, err := st.CreateEvent(ctx, newEvent(""), []NewDelivery{
		{Channel: "email", Address: "a@x.io", UserID: "u1", Skip: "turned off by the user"},
		{Channel: "email", Address: "b@x.io", UserID: "u2", NotBefore: time.Now().Add(time.Hour)},
		{Channel: "email", Address: "c@x.io", UserID: "u3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dels) != 2 {
		t.Fatalf("a skipped delivery is not pushed: %+v", dels)
	}
	ids, _ := st.ClaimDue(ctx, 10, time.Hour)
	if len(ids) != 1 || ids[0] != dels[1].ID {
		t.Fatalf("only the immediate one is due: %v", ids)
	}
	if _, err := st.Claim(ctx, dels[0].ID, time.Minute); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("a delayed delivery must not be claimed early: %v", err)
	}
	var skipped int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM courier_deliveries WHERE status = 'skipped' AND last_error = 'turned off by the user'`).Scan(&skipped); err != nil || skipped != 1 {
		t.Fatalf("skipped audit row: %d %v", skipped, err)
	}
}

func TestLeaderIsExclusive(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	release, ok, err := st.Leader(ctx, 4242)
	if err != nil || !ok {
		t.Fatalf("first: %v %v", ok, err)
	}
	if _, ok2, _ := st.Leader(ctx, 4242); ok2 {
		t.Fatal("a second holder")
	}
	release()
	r2, ok3, _ := st.Leader(ctx, 4242)
	if !ok3 {
		t.Fatal("not free after release")
	}
	r2()
}

func TestStats(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if s, err := st.Stats(ctx); err != nil || len(s.Pending) != 0 || s.OldestDue != 0 {
		t.Fatalf("empty: %+v %v", s, err)
	}
	_, dels, _ := st.CreateEvent(ctx, newEvent(""), []NewDelivery{
		{Channel: "email", Address: "a@x.io"}, {Channel: "email", Address: "b@x.io"}, {Channel: "telegram", Address: "1"},
		{Channel: "email", Address: "c@x.io", NotBefore: time.Now().Add(time.Hour)},
	})
	_, _ = st.Claim(ctx, dels[0].ID, time.Minute)
	_ = st.MarkSent(ctx, dels[0].ID)
	_, _ = st.pool.Exec(ctx, `UPDATE courier_deliveries SET next_attempt_at = now() - interval '5 minutes' WHERE id = $1`, dels[1].ID)

	s, err := st.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Pending["email"] != 2 || s.Pending["telegram"] != 1 { // one e-mail was sent, one waits for its time
		t.Fatalf("pending: %v", s.Pending)
	}
	if s.Recent[[2]string{"sent", "email"}] != 1 || s.Recent[[2]string{"pending", "email"}] != 2 {
		t.Fatalf("recent: %v", s.Recent)
	}
	if s.OldestDue < 4*time.Minute || s.OldestDue > 6*time.Minute {
		t.Fatalf("the oldest due delivery waited 5 minutes: %s", s.OldestDue)
	}
}

func TestPushSubscriptions(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sub := func(user, endpoint string) PushSub {
		return PushSub{UserID: user, Endpoint: endpoint, P256dh: "k", Auth: "a", UserAgent: "UA"}
	}
	if err := st.SavePush(ctx, sub("u1", "https://push.example.com/1")); err != nil {
		t.Fatal(err)
	}
	subs, _ := st.PushSubs(ctx, []string{"u1", "u2"})
	if len(subs["u1"]) != 1 || len(subs["u2"]) != 0 {
		t.Fatalf("%v", subs)
	}
	id := subs["u1"][0].ID
	// the same endpoint of another user moves it, and keeps the row
	if err := st.SavePush(ctx, sub("u2", "https://push.example.com/1")); err != nil {
		t.Fatal(err)
	}
	subs, _ = st.PushSubs(ctx, []string{"u1", "u2"})
	if len(subs["u1"]) != 0 || len(subs["u2"]) != 1 || subs["u2"][0].ID != id {
		t.Fatalf("moved: %v", subs)
	}
	// only the owner removes it by endpoint
	_ = st.DeletePush(ctx, "u1", "https://push.example.com/1")
	if s, _ := st.PushSubs(ctx, []string{"u2"}); len(s["u2"]) != 1 {
		t.Fatal("removed by another user")
	}
	if p, err := st.PushSub(ctx, id); err != nil || p.UserID != "u2" {
		t.Fatalf("by id: %v %v", p, err)
	}
	if err := st.DeletePushByID(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PushSub(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if _, err := st.PushSub(ctx, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad id: %v", err)
	}
	// at most 10 devices per user: the oldest goes
	for i := 0; i < 12; i++ {
		if err := st.SavePush(ctx, sub("u3", fmt.Sprintf("https://push.example.com/d%02d", i))); err != nil {
			t.Fatal(err)
		}
	}
	subs, _ = st.PushSubs(ctx, []string{"u3"})
	if len(subs["u3"]) != 10 {
		t.Fatalf("devices: %d", len(subs["u3"]))
	}
	for _, s := range subs["u3"] {
		if strings.HasSuffix(s.Endpoint, "/d00") || strings.HasSuffix(s.Endpoint, "/d01") {
			t.Fatalf("the oldest must go: %s", s.Endpoint)
		}
	}
}

func TestDigestGroupsEventsIntoOneDelivery(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	later := time.Now().Add(10 * time.Minute)
	newD := func(addr, key string) []NewDelivery {
		return []NewDelivery{{Channel: "email", Address: addr, UserID: "u-" + addr, DigestKey: key, NotBefore: later}}
	}
	ev := func(n string) NewEvent {
		return NewEvent{EventType: "account.registration_pending", Locale: "en", Data: map[string]string{"applicant_name": n}}
	}

	// the first event of a group is its leader, due at the end of the window
	e1, d1, err := st.CreateEvent(ctx, ev("A"), newD("a@x.io", "k"))
	if err != nil || len(d1) != 1 {
		t.Fatalf("leader: %v %v", d1, err)
	}
	leader := d1[0].ID
	// the next ones, same key and address, are attached: no delivery
	e2, d2, err := st.CreateEvent(ctx, ev("B"), newD("a@x.io", "k"))
	if err != nil || len(d2) != 0 {
		t.Fatalf("attached: %v %v", d2, err)
	}
	e3, d3, _ := st.CreateEvent(ctx, ev("C"), newD("a@x.io", "k"))
	if len(d3) != 0 {
		t.Fatalf("attached: %v", d3)
	}
	// another address or another key is another group
	_, d4, _ := st.CreateEvent(ctx, ev("D"), newD("b@x.io", "k"))
	_, d5, _ := st.CreateEvent(ctx, ev("E"), newD("a@x.io", "other"))
	if len(d4) != 1 || len(d5) != 1 {
		t.Fatalf("other groups: %v %v", d4, d5)
	}
	// a delivery without a key is never grouped
	_, d6, _ := st.CreateEvent(ctx, ev("F"), []NewDelivery{{Channel: "email", Address: "a@x.io"}})
	_, d7, _ := st.CreateEvent(ctx, ev("G"), []NewDelivery{{Channel: "email", Address: "a@x.io"}})
	if len(d6) != 1 || len(d7) != 1 {
		t.Fatalf("no key: %v %v", d6, d7)
	}

	recs, _, _ := st.ListDeliveries(ctx, DeliveryFilter{Address: "a@x.io"}, 50, "")
	merged := 0
	for _, r := range recs {
		if r.Delivery.ID == leader {
			merged = r.Merged
		}
	}
	if merged != 2 {
		t.Fatalf("the log says how many events went with the leader: %d", merged)
	}

	// not due yet: nothing to claim before the window ends
	if _, err := st.Claim(ctx, leader, time.Minute); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("the window has not ended: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `UPDATE courier_deliveries SET next_attempt_at = now() WHERE id = $1`, leader); err != nil {
		t.Fatal(err)
	}
	w, err := st.Claim(ctx, leader, time.Minute)
	if err != nil || w.Event.ID != e1 {
		t.Fatalf("claim: %v %v", w, err)
	}
	items, _ := st.DigestItems(ctx, leader)
	if len(items) != 2 || items[0].ID != e2 || items[1].ID != e3 || items[0].Data["applicant_name"] != "B" {
		t.Fatalf("items: %+v", items)
	}

	// the claim closed the group: the next event starts a new one
	_, d8, _ := st.CreateEvent(ctx, ev("H"), newD("a@x.io", "k"))
	if len(d8) != 1 || d8[0].ID == leader {
		t.Fatalf("a new group after the claim: %v", d8)
	}
	// a failed send keeps the items: the retry sends the same digest
	if _, err := st.MarkRetry(ctx, leader, "smtp", time.Now().Add(-time.Second), 5); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.DigestItems(ctx, leader); len(again) != 2 {
		t.Fatalf("items after a retry: %d", len(again))
	}

	// the data of the events goes when the digest is sent, not before
	_, _ = st.Claim(ctx, leader, time.Minute)
	if err := st.MarkSent(ctx, leader); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{e1, e2, e3} {
		ev, _, _ := st.GetEvent(ctx, id)
		if len(ev.Data) != 0 {
			t.Fatalf("event %s still has its data: %v", id, ev.Data)
		}
	}
}

// An event that two people get is carried by two digests; its data stays until both are sent.
func TestDigestKeepsTheDataUntilEveryCarrierIsSent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	soon := time.Now().Add(time.Minute)
	mk := func(addr string) []NewDelivery {
		return []NewDelivery{{Channel: "email", Address: addr, DigestKey: "k", NotBefore: soon}}
	}
	first := NewEvent{EventType: "t", Locale: "en", Data: map[string]string{"n": "1"}}
	_, la, _ := st.CreateEvent(ctx, first, mk("a@x.io"))
	_, lb, _ := st.CreateEvent(ctx, first, mk("b@x.io"))
	shared, da, _ := st.CreateEvent(ctx, NewEvent{EventType: "t", Locale: "en", Data: map[string]string{"n": "2"}}, append(mk("a@x.io"), mk("b@x.io")...))
	if len(da) != 0 {
		t.Fatalf("both are attached: %v", da)
	}
	for _, id := range []string{la[0].ID, lb[0].ID} {
		_, _ = st.pool.Exec(ctx, `UPDATE courier_deliveries SET next_attempt_at = now() WHERE id = $1`, id)
	}
	_, _ = st.Claim(ctx, la[0].ID, time.Minute)
	_ = st.MarkSent(ctx, la[0].ID)
	if ev, _, _ := st.GetEvent(ctx, shared); len(ev.Data) == 0 {
		t.Fatal("the data went while the digest of b still needs it")
	}
	_, _ = st.Claim(ctx, lb[0].ID, time.Minute)
	_ = st.MarkSent(ctx, lb[0].ID)
	if ev, _, _ := st.GetEvent(ctx, shared); len(ev.Data) != 0 {
		t.Fatalf("the data must go after the last digest: %v", ev.Data)
	}
}

// Many events at once for one group give one leader and the rest attached, never two leaders.
func TestDigestConcurrentEventsHaveOneLeader(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	soon := time.Now().Add(time.Minute)
	const n = 12
	leaders := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			_, d, err := st.CreateEvent(ctx, NewEvent{EventType: "t", Locale: "en"}, []NewDelivery{{Channel: "email", Address: "a@x.io", DigestKey: "k", NotBefore: soon}})
			if err != nil {
				t.Error(err)
			}
			leaders <- len(d)
		}()
	}
	total := 0
	for i := 0; i < n; i++ {
		total += <-leaders
	}
	if total != 1 {
		t.Fatalf("%d leaders", total)
	}
	var items int
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM courier_digest_items`).Scan(&items); err != nil || items != n-1 {
		t.Fatalf("items %d %v", items, err)
	}
}

// Two workers finish the last two deliveries of an event at the same moment: the data must go, whoever is last.
func TestMarkSentDropsTheDataEvenWhenTheLastTwoFinishTogether(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for round := 0; round < 25; round++ {
		id, dels, err := st.CreateEvent(ctx, newEvent(""), []NewDelivery{{Channel: "email", Address: "a@x.io"}, {Channel: "telegram", Address: "1"}})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range dels {
			if _, err := st.Claim(ctx, d.ID, time.Minute); err != nil {
				t.Fatal(err)
			}
		}
		done := make(chan error, 2)
		for _, d := range dels {
			go func() { done <- st.MarkSent(ctx, d.ID) }()
		}
		for range dels {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		}
		if ev, _, _ := st.GetEvent(ctx, id); len(ev.Data) != 0 {
			t.Fatalf("round %d: the data of the event stayed: %v", round, ev.Data)
		}
	}
}

// a delivery the user turned off is finished: it does not keep the data of the event
func TestSkippedDeliveryDoesNotKeepTheData(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	id, dels, _ := st.CreateEvent(ctx, newEvent(""), []NewDelivery{{Channel: "email", Address: "a@x.io"}, {Channel: "email", Address: "b@x.io", Skip: "turned off by the user"}})
	if len(dels) != 1 {
		t.Fatal(dels)
	}
	_, _ = st.Claim(ctx, dels[0].ID, time.Minute)
	_ = st.MarkSent(ctx, dels[0].ID)
	if ev, _, _ := st.GetEvent(ctx, id); len(ev.Data) != 0 {
		t.Fatalf("the data stayed: %v", ev.Data)
	}
}
