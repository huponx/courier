package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/huponx/courier/internal/notify/webhook"
	"github.com/huponx/courier/internal/queue"
	"github.com/huponx/courier/internal/store"
	"github.com/huponx/courier/internal/tplsync"
	"github.com/huponx/gocommon/authn"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

type fakeLister struct{ byRole map[string][]string }

func (f fakeLister) ListUserIDs(_ context.Context, role string, fn func([]string) error) error {
	return fn(f.byRole[role])
}

// The admin screens against a real PostgreSQL and Redis: edit a template and see it render, announce, read the log.
func TestAdminEndToEnd(t *testing.T) {
	pgURL, redisURL := os.Getenv("COURIER_TEST_POSTGRES_URL"), os.Getenv("COURIER_TEST_REDIS_URL")
	if pgURL == "" || redisURL == "" {
		t.Skip("COURIER_TEST_POSTGRES_URL and COURIER_TEST_REDIS_URL are not set")
	}
	ctx := context.Background()
	schema := "courier_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	admin, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); admin.Close() })
	sep := "?"
	if strings.Contains(pgURL, "?") {
		sep = "&"
	}
	st, err := store.New(ctx, pgURL+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	pool, _ := pgxpool.New(ctx, pgURL+sep+"search_path="+schema)
	t.Cleanup(pool.Close)
	for _, f := range []string{"V41__courier_notifications.sql", "V42__courier_delivery_recipient.sql", "V43__courier_inbox.sql", "V44__courier_preferences.sql", "V45__courier_templates_broadcasts.sql", "V46__courier_webhooks.sql", "V47__courier_push.sql", "V48__courier_digest.sql", "V50__courier_broadcast_channels.sql"} {
		b, err := os.ReadFile("../../../db-migration/migrations/sql/" + f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatal(err)
		}
	}
	q, err := queue.New(ctx, redisURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })

	notifier := newServer(fakeUsers{
		"u1": {ID: "u1", Email: "an@x.io", Name: "An", Locale: "en"},
		"u2": {ID: "u2", Email: "bo@x.io", Name: "Bo", Locale: "vi"},
		"u3": {ID: "u3", Email: "", Name: "NoMail"},
	})
	notifier.store, notifier.queue, notifier.policy, notifier.hooks = st, q, st, st
	svc := NewAdmin(st, notifier.catalog, notifier.renderer, notifier, fakeLister{byRole: map[string][]string{"": {"u1", "u2", "u3"}, "editor": {"u2"}}},
		func(c context.Context) error { return tplsync.Load(c, st, notifier.renderer) })
	var hookHits []string
	hookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hookHits = append(hookHits, r.Header.Get("X-Courier-Signature")+"|"+string(b))
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(hookSrv.Close)
	svc.WithWebhooks(webhook.New(st, true), true)
	as := authn.WithPrincipal(ctx, &authn.Principal{Subject: "admin-1", ClientID: "gatekeeper"})
	code := func(err error) codes.Code { return status.Code(err) }

	// a ServiceAccount (no token principal) is trusted for the service API, not for the screens of the administrators
	if _, err := svc.ListEventTypes(ctx, &courierv1.ListEventTypesRequest{}); code(err) != codes.PermissionDenied {
		t.Fatalf("no principal: %v", err)
	}
	for name, call := range map[string]func() error{
		"save template": func() error { _, e := svc.SaveTemplate(ctx, &courierv1.SaveTemplateRequest{}); return e },
		"broadcast":     func() error { _, e := svc.CreateBroadcast(ctx, &courierv1.CreateBroadcastRequest{}); return e },
		"webhooks":      func() error { _, e := svc.SaveWebhook(ctx, &courierv1.SaveWebhookRequest{}); return e },
		"log":           func() error { _, e := svc.ListDeliveries(ctx, &courierv1.ListDeliveriesRequest{}); return e },
	} {
		if err := call(); code(err) != codes.PermissionDenied {
			t.Errorf("%s without a principal: %v", name, err)
		}
	}
	big := strings.Repeat("x", 70<<10)
	if _, err := svc.SaveTemplate(as, &courierv1.SaveTemplateRequest{Template: "system_announcement", Locale: "en", Channel: "in_app", Content: &courierv1.TemplateContent{Subject: "t", Text: big}}); code(err) != codes.InvalidArgument {
		t.Fatalf("an oversized template: %v", err)
	}

	// the catalog
	types, err := svc.ListEventTypes(as, &courierv1.ListEventTypesRequest{})
	if err != nil || len(types.GetLocales()) < 2 {
		t.Fatalf("types: %v %v", types, err)
	}
	var announcement *courierv1.EventTypeInfo
	for _, e := range types.GetEventTypes() {
		if e.GetType() == "system.announcement" {
			announcement = e
		}
	}
	if announcement == nil || !announcement.GetBroadcast() || announcement.GetTemplate() != "system_announcement" || len(announcement.GetTemplates()) != 2*len(types.GetLocales()) {
		t.Fatalf("announcement type: %v", announcement)
	}

	// the built-in template is the starting point
	get := func() *courierv1.GetTemplateResponse {
		t.Helper()
		g, err := svc.GetTemplate(as, &courierv1.GetTemplateRequest{Template: "system_announcement", Locale: "en", Channel: "in_app"})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	if g := get(); g.GetCustomized() || g.GetContent().GetSubject() != "{{.title}}\n" {
		t.Fatalf("built in: %v", g)
	}

	// bad drafts
	for name, req := range map[string]*courierv1.SaveTemplateRequest{
		"syntax":        {Template: "system_announcement", Locale: "en", Channel: "in_app", Content: &courierv1.TemplateContent{Subject: "{{.title", Text: "x"}},
		"no body":       {Template: "system_announcement", Locale: "en", Channel: "in_app", Content: &courierv1.TemplateContent{Subject: "t"}},
		"unknown tmpl":  {Template: "nope", Locale: "en", Channel: "in_app", Content: &courierv1.TemplateContent{Subject: "t", Text: "b"}},
		"unknown local": {Template: "system_announcement", Locale: "xx", Channel: "in_app", Content: &courierv1.TemplateContent{Subject: "t", Text: "b"}},
		"bad channel":   {Template: "system_announcement", Locale: "en", Channel: "telegram", Content: &courierv1.TemplateContent{Text: "b"}},
	} {
		if _, err := svc.SaveTemplate(as, req); err == nil || (code(err) != codes.InvalidArgument && code(err) != codes.NotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// preview does not save
	pv, err := svc.PreviewTemplate(as, &courierv1.PreviewTemplateRequest{
		Template: "system_announcement", Locale: "en", Channel: "in_app",
		Content: &courierv1.TemplateContent{Subject: "[{{.title}}]", Text: "{{.message}} / {{.name}}"}, Data: map[string]string{"title": "Maintenance"},
	})
	if err != nil || pv.GetRendered().GetSubject() != "[Maintenance]" || pv.GetRendered().GetText() != "sample message / Alex" {
		t.Fatalf("preview: %v %v", pv, err)
	}
	if get().GetCustomized() {
		t.Fatal("a preview must not save")
	}

	// save: version 1, rendered by the renderer at once
	v1, err := svc.SaveTemplate(as, &courierv1.SaveTemplateRequest{Template: "system_announcement", Locale: "en", Channel: "in_app", Content: &courierv1.TemplateContent{Subject: "NEWS: {{.title}}", Text: "{{.message}}"}})
	if err != nil || v1.GetVersion() != 1 {
		t.Fatalf("save: %v %v", v1, err)
	}
	msg, err := notifier.renderer.Render("en", "system_announcement", courierv1.Channel_CHANNEL_IN_APP, map[string]string{"title": "T", "message": "M"})
	if err != nil || msg.Subject != "NEWS: T" {
		t.Fatalf("the saved template is not used: %v %v", msg, err)
	}
	// the other locale still uses the built-in template
	if m, err := notifier.renderer.Render("vi", "system_announcement", courierv1.Channel_CHANNEL_IN_APP, map[string]string{"title": "T", "message": "M"}); err != nil || strings.TrimSpace(m.Subject) != "T" {
		t.Fatalf("vi must stay built in: %v %v", m, err)
	}
	if g := get(); !g.GetCustomized() || g.GetVersion() != 1 || g.GetUpdatedBy() != "admin-1" || g.GetContent().GetSubject() != "NEWS: {{.title}}" {
		t.Fatalf("get after save: %v", g)
	}
	v2, _ := svc.SaveTemplate(as, &courierv1.SaveTemplateRequest{Template: "system_announcement", Locale: "en", Channel: "in_app", Content: &courierv1.TemplateContent{Subject: "NEWS 2: {{.title}}", Text: "{{.message}}"}})
	if v2.GetVersion() != 2 {
		t.Fatalf("second save: %v", v2)
	}

	// an announcement to everyone: the edited English title reaches u1, the built-in Vietnamese one u2, u3 has no e-mail
	data, _ := structpb.NewStruct(map[string]any{"title": "Maintenance", "message": "Tonight at 22:00"})
	bad := []*courierv1.CreateBroadcastRequest{
		{EventType: "security.mfa_changed", Audience: &courierv1.Audience{All: true}, Data: data},
		{EventType: "system.announcement", Audience: &courierv1.Audience{}, Data: data},
		{EventType: "system.announcement", Audience: &courierv1.Audience{All: true, Role: "x"}, Data: data},
		{EventType: "system.announcement", Audience: &courierv1.Audience{Role: "nobody"}, Data: data},
	}
	for i, req := range bad {
		if _, err := svc.CreateBroadcast(as, req); code(err) != codes.InvalidArgument {
			t.Errorf("bad broadcast %d: %v", i, err)
		}
	}
	missing, _ := structpb.NewStruct(map[string]any{"title": "no message"})
	if _, err := svc.CreateBroadcast(as, &courierv1.CreateBroadcastRequest{EventType: "system.announcement", Audience: &courierv1.Audience{All: true}, Data: missing}); code(err) != codes.InvalidArgument {
		t.Errorf("data is validated against the catalog: %v", err)
	}
	b, err := svc.CreateBroadcast(as, &courierv1.CreateBroadcastRequest{EventType: "system.announcement", Audience: &courierv1.Audience{All: true}, Data: data})
	if err != nil || b.GetBroadcast().GetRecipients() != 3 || b.GetBroadcast().GetSkipped() != 0 || b.GetBroadcast().GetCreatedBy() != "admin-1" {
		t.Fatalf("broadcast: %v %v", b, err)
	}
	en, _, _ := st.ListInbox(ctx, "u1", 10, "", false)
	vi, _, _ := st.ListInbox(ctx, "u2", 10, "", false)
	no, _, _ := st.ListInbox(ctx, "u3", 10, "", false)
	if len(en) != 1 || en[0].Title != "NEWS 2: Maintenance" || len(vi) != 1 || vi[0].Title != "Maintenance" || len(no) != 1 {
		t.Fatalf("inbox: %v %v %v", en, vi, no)
	}
	// a role is an audience too
	if r, err := svc.CreateBroadcast(as, &courierv1.CreateBroadcastRequest{EventType: "system.announcement", Audience: &courierv1.Audience{Role: "editor"}, Data: data}); err != nil || r.GetBroadcast().GetRecipients() != 1 {
		t.Fatalf("role broadcast: %v %v", r, err)
	}
	list, err := svc.ListBroadcasts(as, &courierv1.ListBroadcastsRequest{PageSize: 1})
	if err != nil || len(list.GetBroadcasts()) != 1 || list.GetNextPageToken() == "" || list.GetBroadcasts()[0].GetAudience().GetRole() != "editor" {
		t.Fatalf("broadcasts: %v %v", list, err)
	}
	if next, _ := svc.ListBroadcasts(as, &courierv1.ListBroadcastsRequest{PageSize: 1, PageToken: list.GetNextPageToken()}); len(next.GetBroadcasts()) != 1 || !next.GetBroadcasts()[0].GetAudience().GetAll() {
		t.Fatalf("second page: %v", next)
	}

	// the delivery log: the e-mails of the announcements (u1, u2 twice for the role), filters and pages
	all, err := svc.ListDeliveries(as, &courierv1.ListDeliveriesRequest{})
	if err != nil || len(all.GetDeliveries()) != 3 {
		t.Fatalf("deliveries: %v %v", all, err)
	}
	if d := all.GetDeliveries()[0]; d.GetEventType() != "system.announcement" || d.GetUserId() == "" || d.GetNotificationId() == "" || d.GetCreatedAt() == 0 {
		t.Fatalf("record: %v", d)
	}
	if one, _ := svc.ListDeliveries(as, &courierv1.ListDeliveriesRequest{Address: "BO@"}); len(one.GetDeliveries()) != 2 {
		t.Fatalf("address filter is case insensitive: %v", one)
	}
	if one, _ := svc.ListDeliveries(as, &courierv1.ListDeliveriesRequest{UserId: "u1"}); len(one.GetDeliveries()) != 1 {
		t.Fatalf("user filter: %v", one)
	}
	if none, _ := svc.ListDeliveries(as, &courierv1.ListDeliveriesRequest{Status: courierv1.DeliveryStatus_DELIVERY_STATUS_DEAD}); len(none.GetDeliveries()) != 0 {
		t.Fatalf("status filter: %v", none)
	}
	p1, _ := svc.ListDeliveries(as, &courierv1.ListDeliveriesRequest{PageSize: 2})
	p2, _ := svc.ListDeliveries(as, &courierv1.ListDeliveriesRequest{PageSize: 2, PageToken: p1.GetNextPageToken()})
	if len(p1.GetDeliveries()) != 2 || len(p2.GetDeliveries()) != 1 || p2.GetNextPageToken() != "" || p1.GetDeliveries()[1].GetDelivery().GetId() == p2.GetDeliveries()[0].GetDelivery().GetId() {
		t.Fatalf("paging: %v %v", p1, p2)
	}
	if _, err := svc.ListDeliveries(as, &courierv1.ListDeliveriesRequest{PageToken: "garbage"}); code(err) != codes.InvalidArgument {
		t.Fatalf("bad token: %v", err)
	}

	// history and revert: back to the built-in template
	vs, err := svc.ListTemplateVersions(as, &courierv1.ListTemplateVersionsRequest{Template: "system_announcement", Locale: "en", Channel: "in_app"})
	if err != nil || len(vs.GetVersions()) != 2 || vs.GetVersions()[0].GetVersion() != 2 || vs.GetVersions()[1].GetContent().GetSubject() != "NEWS: {{.title}}" {
		t.Fatalf("versions: %v %v", vs, err)
	}
	if _, err := svc.RevertTemplate(as, &courierv1.RevertTemplateRequest{Template: "system_announcement", Locale: "en", Channel: "in_app"}); err != nil {
		t.Fatal(err)
	}
	if m, _ := notifier.renderer.Render("en", "system_announcement", courierv1.Channel_CHANNEL_IN_APP, map[string]string{"title": "T", "message": "M"}); strings.TrimSpace(m.Subject) != "T" {
		t.Fatalf("not reverted: %v", m)
	}
	if g := get(); g.GetCustomized() {
		t.Fatalf("still customized: %v", g)
	}
	if _, err := svc.RevertTemplate(as, &courierv1.RevertTemplateRequest{Template: "system_announcement", Locale: "en", Channel: "in_app"}); code(err) != codes.NotFound {
		t.Fatalf("second revert: %v", err)
	}
	vs, _ = svc.ListTemplateVersions(as, &courierv1.ListTemplateVersionsRequest{Template: "system_announcement", Locale: "en", Channel: "in_app"})
	if len(vs.GetVersions()) != 3 || !vs.GetVersions()[0].GetReverted() || vs.GetVersions()[0].GetVersion() != 3 {
		t.Fatalf("the revert is part of the history: %v", vs)
	}
	// saving again continues the numbering
	if v, _ := svc.SaveTemplate(as, &courierv1.SaveTemplateRequest{Template: "system_announcement", Locale: "en", Channel: "in_app", Content: &courierv1.TemplateContent{Subject: "again", Text: "b"}}); v.GetVersion() != 4 {
		t.Fatalf("numbering: %v", v)
	}

	// webhooks
	for name, in := range map[string]*courierv1.Webhook{
		"no name":         {Kind: "slack", Url: hookSrv.URL, Events: []string{"*"}},
		"bad kind":        {Name: "x", Kind: "sms", Url: hookSrv.URL, Events: []string{"*"}},
		"bad url":         {Name: "x", Kind: "slack", Url: "ftp://x", Events: []string{"*"}},
		"no events":       {Name: "x", Kind: "slack", Url: hookSrv.URL},
		"unknown event":   {Name: "x", Kind: "slack", Url: hookSrv.URL, Events: []string{"nope.x"}},
		"unknown prefix":  {Name: "x", Kind: "slack", Url: hookSrv.URL, Events: []string{"nope.*"}},
		"mandatory event": {Name: "x", Kind: "slack", Url: hookSrv.URL, Events: []string{"security.password_reset"}},
	} {
		if _, err := svc.SaveWebhook(as, &courierv1.SaveWebhookRequest{Webhook: in}); code(err) != codes.InvalidArgument {
			t.Errorf("webhook %s: %v", name, err)
		}
	}
	created, err := svc.SaveWebhook(as, &courierv1.SaveWebhookRequest{Webhook: &courierv1.Webhook{Name: "ops", Kind: "generic", Url: hookSrv.URL, Events: []string{"bvchain.*"}, Enabled: true}})
	if err != nil || created.GetSecret() == "" || !created.GetWebhook().GetHasSecret() || created.GetWebhook().GetId() == "" {
		t.Fatalf("create: %v %v", created, err)
	}
	id := created.GetWebhook().GetId()
	if lw, _ := svc.ListWebhooks(as, &courierv1.ListWebhooksRequest{}); len(lw.GetWebhooks()) != 1 || lw.GetWebhooks()[0].GetHasSecret() != true {
		t.Fatalf("list: %v", lw)
	}
	// an update keeps the secret; a rotation makes a new one
	up, err := svc.SaveWebhook(as, &courierv1.SaveWebhookRequest{Id: id, Webhook: &courierv1.Webhook{Name: "ops 2", Kind: "generic", Url: hookSrv.URL, Events: []string{"bvchain.*"}, Enabled: true}})
	if err != nil || up.GetSecret() != "" || up.GetWebhook().GetName() != "ops 2" {
		t.Fatalf("update: %v %v", up, err)
	}
	rot, _ := svc.SaveWebhook(as, &courierv1.SaveWebhookRequest{Id: id, RotateSecret: true, Webhook: &courierv1.Webhook{Name: "ops 2", Kind: "generic", Url: hookSrv.URL, Events: []string{"bvchain.*"}, Enabled: true}})
	if rot.GetSecret() == "" || rot.GetSecret() == created.GetSecret() {
		t.Fatalf("rotate: %v", rot)
	}
	if _, err := svc.SaveWebhook(as, &courierv1.SaveWebhookRequest{Id: uuid.NewString(), Webhook: &courierv1.Webhook{Name: "x", Kind: "slack", Url: hookSrv.URL, Events: []string{"*"}}}); code(err) != codes.NotFound {
		t.Fatalf("update of an unknown webhook: %v", err)
	}

	// the test message is signed with the current secret
	tw, err := svc.TestWebhook(as, &courierv1.TestWebhookRequest{Id: id})
	if err != nil || !tw.GetOk() || !strings.HasPrefix(tw.GetResult(), "HTTP 200") || len(hookHits) != 1 {
		t.Fatalf("test: %v %v %v", tw, err, hookHits)
	}
	if !strings.HasPrefix(hookHits[0], "sha256=") || !strings.Contains(hookHits[0], `"event_type":"courier.test"`) {
		t.Fatalf("hit: %s", hookHits[0])
	}

	// an event of a subscribed type gets a webhook delivery; another type and a mandatory one do not
	byEvent := func(typ string, d map[string]any, raw bool) []*courierv1.DeliveryRecord {
		t.Helper()
		st2, _ := structpb.NewStruct(d)
		req := &courierv1.NotifyRequest{EventType: typ, Data: st2}
		if raw {
			req.Recipients = []*courierv1.Recipient{{Channel: courierv1.Channel_CHANNEL_TELEGRAM, Address: "555"}}
		} else {
			req.UserIds = []string{"u1"}
		}
		r, err := notifier.Notify(as, req)
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		l, _ := svc.ListDeliveries(as, &courierv1.ListDeliveriesRequest{NotificationId: r.GetEventId()})
		return l.GetDeliveries()
	}
	has := func(recs []*courierv1.DeliveryRecord, channel string) *courierv1.DeliveryRecord {
		for _, r := range recs {
			if r.GetDelivery().GetChannel() == courierv1.Channel_CHANNEL_WEBHOOK && channel == "webhook" || r.GetDelivery().GetChannel() == courierv1.Channel_CHANNEL_TELEGRAM && channel == "telegram" {
				return r
			}
		}
		return nil
	}
	recs := byEvent("bvchain.failed", map[string]any{"step": "login", "error": "boom"}, true)
	if wh := has(recs, "webhook"); wh == nil || wh.GetDelivery().GetAddress() != id || has(recs, "telegram") == nil {
		t.Fatalf("bvchain.failed: %v", recs)
	}
	if recs := byEvent("system.hello", map[string]any{}, false); has(recs, "webhook") != nil {
		t.Fatalf("system.hello is not subscribed: %v", recs)
	}
	mfa := byEvent("security.mfa_changed", map[string]any{"change": "enabled"}, false)
	if has(mfa, "webhook") != nil {
		t.Fatalf("a mandatory event never goes to a webhook: %v", mfa)
	}
	// a disabled webhook gets nothing
	_, _ = svc.SaveWebhook(as, &courierv1.SaveWebhookRequest{Id: id, Webhook: &courierv1.Webhook{Name: "ops 2", Kind: "generic", Url: hookSrv.URL, Events: []string{"bvchain.*"}, Enabled: false}})
	if recs := byEvent("bvchain.failed", map[string]any{"step": "x", "error": "y"}, true); has(recs, "webhook") != nil {
		t.Fatalf("disabled: %v", recs)
	}

	if _, err := svc.DeleteWebhook(as, &courierv1.DeleteWebhookRequest{Id: id}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.DeleteWebhook(as, &courierv1.DeleteWebhookRequest{Id: id}); code(err) != codes.NotFound {
		t.Fatalf("second delete: %v", err)
	}

	// digest: two registrations in a row reach the admin as ONE e-mail (merged 1), but two items in the bell
	pending := func(name string) {
		t.Helper()
		d, _ := structpb.NewStruct(map[string]any{"applicant_name": name, "applicant_email": name + "@x.io"})
		if _, err := notifier.Notify(as, &courierv1.NotifyRequest{EventType: "account.registration_pending", UserIds: []string{"u1"}, Data: d}); err != nil {
			t.Fatal(err)
		}
	}
	pending("ann")
	pending("bob")
	emails, _ := svc.ListDeliveries(as, &courierv1.ListDeliveriesRequest{UserId: "u1", Channel: "email"})
	var group []*courierv1.DeliveryRecord
	for _, r := range emails.GetDeliveries() {
		if r.GetEventType() == "account.registration_pending" {
			group = append(group, r)
		}
	}
	if len(group) != 1 || group[0].GetMerged() != 1 || group[0].GetDelivery().GetStatus() != courierv1.DeliveryStatus_DELIVERY_STATUS_PENDING {
		t.Fatalf("one e-mail for two events: %v", group)
	}
	if next := group[0].GetDelivery().GetNextAttemptAt(); next == "" {
		t.Fatal("the leader waits for the end of the window")
	}
	items, _, _ := st.ListInbox(ctx, "u1", 50, "", false)
	bell := 0
	for _, it := range items {
		if it.EventType == "account.registration_pending" {
			bell++
		}
	}
	if bell != 2 {
		t.Fatalf("the bell is not grouped: %d", bell)
	}
	// the mandatory event of the same user is never held back or grouped
	countSecurity := func() (n, merged int) {
		l, _ := svc.ListDeliveries(as, &courierv1.ListDeliveriesRequest{UserId: "u1", Channel: "email"})
		for _, r := range l.GetDeliveries() {
			if r.GetEventType() == "security.mfa_changed" {
				n++
				merged += int(r.GetMerged())
			}
		}
		return
	}
	before, _ := countSecurity()
	mfaData, _ := structpb.NewStruct(map[string]any{"change": "enabled"})
	for i := 0; i < 2; i++ {
		if _, err := notifier.Notify(as, &courierv1.NotifyRequest{EventType: "security.mfa_changed", UserIds: []string{"u1"}, Data: mfaData}); err != nil {
			t.Fatal(err)
		}
	}
	if after, merged := countSecurity(); after != before+2 || merged != 0 {
		t.Fatalf("two more security e-mails, none grouped: %d -> %d, merged %d", before, after, merged)
	}

	// the digest template is edited like any other, with sample count and items
	types2, _ := svc.ListEventTypes(as, &courierv1.ListEventTypesRequest{})
	var pend *courierv1.EventTypeInfo
	for _, e := range types2.GetEventTypes() {
		if e.GetType() == "account.registration_pending" {
			pend = e
		}
	}
	digestTemplates := 0
	for _, tp := range pend.GetTemplates() {
		if tp.GetTemplate() == "identity_registration_pending_digest" {
			digestTemplates++
		}
	}
	if pend.GetDigestWindow() != "10m" || digestTemplates == 0 {
		t.Fatalf("digest in the catalog listing: %v", pend)
	}
	pv2, err := svc.PreviewTemplate(as, &courierv1.PreviewTemplateRequest{
		Template: "identity_registration_pending_digest", Locale: "en", Channel: "email",
		Content: &courierv1.TemplateContent{Subject: "{{.count}} waiting", Text: "{{.items}}"},
	})
	if err != nil || pv2.GetRendered().GetSubject() != "3 waiting" || !strings.Contains(pv2.GetRendered().GetText(), "sample line 2") {
		t.Fatalf("digest preview: %v %v", pv2, err)
	}

	// chosen users: the same id twice (and across pages) is one user
	dup, err := svc.CreateBroadcast(as, &courierv1.CreateBroadcastRequest{EventType: "system.announcement", Audience: &courierv1.Audience{UserIds: []string{"u1", " u1 ", "u2", "u1"}}, Data: data})
	if err != nil || dup.GetBroadcast().GetRecipients() != 2 {
		t.Fatalf("duplicate ids: %v %v", dup, err)
	}

	// the channels of an announcement: the bell only, no e-mail; a channel the type does not have is refused
	emailsOf := func(user string) int {
		l, _ := svc.ListDeliveries(as, &courierv1.ListDeliveriesRequest{UserId: user, Channel: "email"})
		return len(l.GetDeliveries())
	}
	inboxOf := func(user string) int {
		items, _, _ := st.ListInbox(ctx, user, 100, "", false)
		return len(items)
	}
	mails, bell := emailsOf("u2"), inboxOf("u2")
	only, err := svc.CreateBroadcast(as, &courierv1.CreateBroadcastRequest{
		EventType: "system.announcement", Audience: &courierv1.Audience{UserIds: []string{"u2"}}, Data: data, Channels: []string{"in_app", "in_app"},
	})
	if err != nil || len(only.GetBroadcast().GetChannels()) != 1 || only.GetBroadcast().GetChannels()[0] != "in_app" {
		t.Fatalf("bell only: %v %v", only, err)
	}
	if emailsOf("u2") != mails || inboxOf("u2") != bell+1 {
		t.Fatalf("no e-mail and one bell item: e-mails %d -> %d, bell %d -> %d", mails, emailsOf("u2"), bell, inboxOf("u2"))
	}
	for _, ch := range []string{"telegram", "sms", "webhook"} {
		if _, err := svc.CreateBroadcast(as, &courierv1.CreateBroadcastRequest{EventType: "system.announcement", Audience: &courierv1.Audience{UserIds: []string{"u2"}}, Data: data, Channels: []string{ch}}); code(err) != codes.InvalidArgument {
			t.Errorf("channel %s: %v", ch, err)
		}
	}
	if emailsOf("u2") != mails || inboxOf("u2") != bell+1 {
		t.Fatal("a refused announcement sent something")
	}
	// the history says which channels it used; an older one (none stored) shows none
	hist, _ := svc.ListBroadcasts(as, &courierv1.ListBroadcastsRequest{PageSize: 1})
	if got := hist.GetBroadcasts()[0].GetChannels(); len(got) != 1 || got[0] != "in_app" {
		t.Fatalf("history: %v", hist.GetBroadcasts()[0])
	}
	// e-mail and the bell, without push
	both, err := svc.CreateBroadcast(as, &courierv1.CreateBroadcastRequest{EventType: "system.announcement", Audience: &courierv1.Audience{UserIds: []string{"u2"}}, Data: data, Channels: []string{"email", "in_app"}})
	if err != nil || emailsOf("u2") != mails+1 || inboxOf("u2") != bell+2 || len(both.GetBroadcast().GetChannels()) != 2 {
		t.Fatalf("e-mail and bell: %v %v", both, err)
	}
}
