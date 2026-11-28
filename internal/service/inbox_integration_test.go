package service

import (
	"context"
	"encoding/base64"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/huponx/courier/internal/directory"
	"github.com/huponx/courier/internal/queue"
	"github.com/huponx/courier/internal/store"
	"github.com/huponx/gocommon/authn"
	"github.com/huponx/gocommon/grpcserver"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

// The whole path of the bell against a real PostgreSQL and Redis (COURIER_TEST_POSTGRES_URL, COURIER_TEST_REDIS_URL):
// Notify writes the inbox and publishes, StreamInbox delivers it, the user reads and archives it, another user and a
// service can neither read nor change it.
func TestInboxEndToEnd(t *testing.T) {
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

	// the test auth: x-user is a signed-in user, x-service a service token (subject == client)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpcserver.New(
		grpcserver.WithAuth(func(ctx context.Context) (context.Context, error) {
			md, _ := metadata.FromIncomingContext(ctx)
			if v := md.Get("x-user"); len(v) == 1 {
				return authn.WithPrincipal(ctx, &authn.Principal{Subject: v[0], ClientID: "gatekeeper"}), nil
			}
			if v := md.Get("x-service"); len(v) == 1 {
				return authn.WithPrincipal(ctx, &authn.Principal{Subject: v[0], ClientID: v[0], Perms: []string{"courier:notification:send"}}), nil
			}
			return ctx, status.Error(codes.Unauthenticated, "no")
		}),
		grpcserver.WithMethodPermissions(Permissions),
	)
	users := fakeUsers{"u1": {ID: "u1", Email: "an@x.io", Name: "An", Locale: "en"}, "u2": {ID: "u2", Email: "bo@x.io", Name: "Bo"}}
	notifier := newServer(users)
	notifier.store, notifier.queue = st, q
	courierv1.RegisterCourierServiceServer(srv.GRPC, notifier)
	courierv1.RegisterInboxServiceServer(srv.GRPC, NewInbox(st, q))
	courierv1.RegisterPreferencesServiceServer(srv.GRPC, NewPreferences(st, notifier.catalog, "@MyBot").WithPush("BPublicKeyForTest", false))
	go func() { _ = srv.GRPC.Serve(lis) }()
	t.Cleanup(srv.GRPC.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	inbox := courierv1.NewInboxServiceClient(conn)
	courier := courierv1.NewCourierServiceClient(conn)
	as := func(user string) context.Context {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		t.Cleanup(cancel)
		return metadata.AppendToOutgoingContext(c, "x-user", user)
	}
	svc := func() context.Context {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		t.Cleanup(cancel)
		return metadata.AppendToOutgoingContext(c, "x-service", "identity")
	}

	// u1 opens the stream: the first message is the count (0)
	stream, err := inbox.StreamInbox(as("u1"), &courierv1.StreamInboxRequest{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || first.GetUnread() != 0 || first.GetItem() != nil {
		t.Fatalf("first message: %v %v", first, err)
	}

	// a service sends an event to u1 and u2
	data, _ := structpb.NewStruct(map[string]any{"change": "enabled"})
	resp, err := courier.Notify(svc(), &courierv1.NotifyRequest{EventType: "security.mfa_changed", UserIds: []string{"u1", "u2"}, Data: data})
	if err != nil || resp.GetQueued() != 2 { // two e-mails; the inbox rows are not deliveries
		t.Fatalf("notify: %v %v", resp, err)
	}

	// the stream of u1 delivers it, with the count, and nothing of u2
	got, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if got.GetUnread() != 1 || got.GetItem().GetTitle() != "Two-step verification turned on" || got.GetItem().GetSeverity() != courierv1.Severity_SEVERITY_WARNING {
		t.Fatalf("stream message: %v", got)
	}

	list, err := inbox.ListInbox(as("u1"), &courierv1.ListInboxRequest{})
	if err != nil || len(list.GetItems()) != 1 || list.GetUnread() != 1 || list.GetItems()[0].GetId() != got.GetItem().GetId() {
		t.Fatalf("list: %v %v", list, err)
	}
	id := list.GetItems()[0].GetId()

	// u2 cannot touch the item of u1 (their own is untouched too)
	if _, err := inbox.MarkRead(as("u2"), &courierv1.MarkReadRequest{Ids: []string{id}}); err != nil {
		t.Fatal(err)
	}
	if n, _ := inbox.GetUnreadCount(as("u1"), &courierv1.GetUnreadCountRequest{}); n.GetUnread() != 1 {
		t.Fatalf("u2 marked the item of u1: %d", n.GetUnread())
	}

	// u1 reads it: the count drops and the stream (another tab) hears it
	mr, err := inbox.MarkRead(as("u1"), &courierv1.MarkReadRequest{Ids: []string{id}})
	if err != nil || mr.GetUnread() != 0 {
		t.Fatalf("mark read: %v %v", mr, err)
	}
	changed, err := stream.Recv()
	if err != nil || changed.GetUnread() != 0 || changed.GetItem() != nil {
		t.Fatalf("change message: %v %v", changed, err)
	}
	if _, err := inbox.Archive(as("u1"), &courierv1.ArchiveRequest{Ids: []string{id}}); err != nil {
		t.Fatal(err)
	}
	if list, _ := inbox.ListInbox(as("u1"), &courierv1.ListInboxRequest{}); len(list.GetItems()) != 0 {
		t.Fatalf("archived item is listed: %v", list)
	}

	// a service has no inbox, and a user cannot call the service API
	if _, err := inbox.ListInbox(svc(), &courierv1.ListInboxRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("service reading an inbox: %v", err)
	}
	if _, err := courier.Notify(as("u1"), &courierv1.NotifyRequest{EventType: "security.mfa_changed", UserIds: []string{"u2"}, Data: data}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("user calling Notify: %v", err)
	}
	var _ directory.Resolver = users

	// preferences of u1
	prefs := courierv1.NewPreferencesServiceClient(conn)
	gp, err := prefs.GetPreferences(as("u1"), &courierv1.GetPreferencesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*courierv1.CategoryPreference{}
	for _, c := range gp.GetCategories() {
		byName[c.GetCategory()] = c
	}
	if !byName["security"].GetMandatory() || byName["admin"].GetMandatory() || len(byName["admin"].GetChannels()) != 3 {
		t.Fatalf("categories: %v", gp.GetCategories())
	}
	if !gp.GetTelegram().GetAvailable() || gp.GetTelegram().GetLinked() || gp.GetQuietHours().GetEnabled() {
		t.Fatalf("state: %v", gp)
	}
	if _, err := prefs.SetPreferences(as("u1"), &courierv1.SetPreferencesRequest{
		Categories: []*courierv1.CategoryPreference{
			{Category: "admin", DisabledChannels: []string{"email"}},
			{Category: "security", DisabledChannels: []string{"email"}}, // mandatory: ignored
		},
		QuietHours: &courierv1.QuietHours{Enabled: true, From: "22:00", To: "07:00", Timezone: "Asia/Ho_Chi_Minh"},
	}); err != nil {
		t.Fatal(err)
	}
	gp, _ = prefs.GetPreferences(as("u1"), &courierv1.GetPreferencesRequest{})
	for _, c := range gp.GetCategories() {
		switch c.GetCategory() {
		case "admin":
			if len(c.GetDisabledChannels()) != 1 || c.GetDisabledChannels()[0] != "email" {
				t.Fatalf("admin: %v", c)
			}
		case "security":
			if len(c.GetDisabledChannels()) != 0 {
				t.Fatalf("a mandatory category cannot be turned off: %v", c)
			}
		}
	}
	if qh := gp.GetQuietHours(); !qh.GetEnabled() || qh.GetFrom() != "22:00" || qh.GetTo() != "07:00" {
		t.Fatalf("quiet hours: %v", qh)
	}
	// the choice is the user's own
	if o, _ := prefs.GetPreferences(as("u2"), &courierv1.GetPreferencesRequest{}); o.GetQuietHours().GetEnabled() {
		t.Fatal("u2 got the quiet hours of u1")
	}
	for name, req := range map[string]*courierv1.SetPreferencesRequest{
		"unknown category": {Categories: []*courierv1.CategoryPreference{{Category: "nope"}}},
		"unknown channel":  {Categories: []*courierv1.CategoryPreference{{Category: "admin", DisabledChannels: []string{"sms"}}}},
		"empty window":     {QuietHours: &courierv1.QuietHours{Enabled: true, From: "08:00", To: "08:00", Timezone: "UTC"}},
		"bad time":         {QuietHours: &courierv1.QuietHours{Enabled: true, From: "8am", To: "09:00", Timezone: "UTC"}},
		"bad zone":         {QuietHours: &courierv1.QuietHours{Enabled: true, From: "08:00", To: "09:00", Timezone: "Mars/Base"}},
	} {
		if _, err := prefs.SetPreferences(as("u1"), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: %v", name, err)
		}
	}
	// an absent or disabled quiet window removes it
	_, _ = prefs.SetPreferences(as("u1"), &courierv1.SetPreferencesRequest{})
	if gp, _ := prefs.GetPreferences(as("u1"), &courierv1.GetPreferencesRequest{}); gp.GetQuietHours().GetEnabled() {
		t.Fatal("quiet hours not removed")
	}

	// Telegram: the link carries the code; a service cannot ask for one
	b, err := prefs.StartTelegramBinding(as("u1"), &courierv1.StartTelegramBindingRequest{})
	if err != nil || !strings.HasPrefix(b.GetLink(), "https://t.me/MyBot?start=") || !strings.HasSuffix(b.GetLink(), b.GetCode()) {
		t.Fatalf("binding: %v %v", b, err)
	}
	if _, err := st.ConsumeBindingCode(ctx, "telegram", b.GetCode(), "777"); err != nil {
		t.Fatal(err)
	}
	if gp, _ := prefs.GetPreferences(as("u1"), &courierv1.GetPreferencesRequest{}); !gp.GetTelegram().GetLinked() {
		t.Fatal("not linked")
	}
	if _, err := prefs.UnbindTelegram(as("u1"), &courierv1.UnbindTelegramRequest{}); err != nil {
		t.Fatal(err)
	}
	if gp, _ := prefs.GetPreferences(as("u1"), &courierv1.GetPreferencesRequest{}); gp.GetTelegram().GetLinked() {
		t.Fatal("still linked")
	}
	// Web Push: the key to subscribe with, the subscription of a device, and nobody else's
	if gp, _ := prefs.GetPreferences(as("u1"), &courierv1.GetPreferencesRequest{}); !gp.GetPush().GetAvailable() || gp.GetPush().GetPublicKey() != "BPublicKeyForTest" || len(gp.GetPush().GetEndpoints()) != 0 {
		t.Fatalf("push state: %v", gp.GetPush())
	}
	p256 := base64.RawURLEncoding.EncodeToString(make([]byte, 65))
	authKey := base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	good := &courierv1.SubscribePushRequest{Endpoint: "https://push.example.com/send/abc", P256Dh: p256, Auth: authKey, UserAgent: "Firefox"}
	for name, req := range map[string]*courierv1.SubscribePushRequest{
		"http endpoint":  {Endpoint: "http://push.example.com/x", P256Dh: p256, Auth: authKey},
		"no endpoint":    {P256Dh: p256, Auth: authKey},
		"short p256dh":   {Endpoint: good.Endpoint, P256Dh: authKey, Auth: authKey},
		"bad auth":       {Endpoint: good.Endpoint, P256Dh: p256, Auth: "!!"},
		"endpoint creds": {Endpoint: "https://u:p@push.example.com/x", P256Dh: p256, Auth: authKey},
	} {
		if _, err := prefs.SubscribePush(as("u1"), req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("push %s: %v", name, err)
		}
	}
	if _, err := prefs.SubscribePush(as("u1"), good); err != nil {
		t.Fatal(err)
	}
	if gp, _ := prefs.GetPreferences(as("u1"), &courierv1.GetPreferencesRequest{}); len(gp.GetPush().GetEndpoints()) != 1 || gp.GetPush().GetEndpoints()[0] != good.Endpoint {
		t.Fatalf("subscribed: %v", gp.GetPush())
	}
	// the browser is now u2's: the endpoint moves, u1 loses it
	if _, err := prefs.SubscribePush(as("u2"), good); err != nil {
		t.Fatal(err)
	}
	if gp, _ := prefs.GetPreferences(as("u1"), &courierv1.GetPreferencesRequest{}); len(gp.GetPush().GetEndpoints()) != 0 {
		t.Fatalf("the endpoint moved to u2: %v", gp.GetPush())
	}
	// u1 cannot remove the subscription of u2
	_, _ = prefs.UnsubscribePush(as("u1"), &courierv1.UnsubscribePushRequest{Endpoint: good.Endpoint})
	if gp, _ := prefs.GetPreferences(as("u2"), &courierv1.GetPreferencesRequest{}); len(gp.GetPush().GetEndpoints()) != 1 {
		t.Fatalf("u1 removed the device of u2: %v", gp.GetPush())
	}
	_, _ = prefs.UnsubscribePush(as("u2"), &courierv1.UnsubscribePushRequest{Endpoint: good.Endpoint})
	if gp, _ := prefs.GetPreferences(as("u2"), &courierv1.GetPreferencesRequest{}); len(gp.GetPush().GetEndpoints()) != 0 {
		t.Fatalf("not removed: %v", gp.GetPush())
	}
	if _, err := prefs.StartTelegramBinding(svc(), &courierv1.StartTelegramBindingRequest{}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("a service has no preferences: %v", err)
	}
}
