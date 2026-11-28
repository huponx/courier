package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/huponx/courier/internal/catalog"
	"github.com/huponx/courier/internal/directory"
	"github.com/huponx/courier/internal/notify/render"
	"github.com/huponx/courier/internal/store"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

type fakeUsers map[string]directory.User

func (f fakeUsers) Users(_ context.Context, ids []string) (map[string]directory.User, error) {
	out := map[string]directory.User{}
	for _, id := range ids {
		if u, ok := f[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func newServer(users directory.Resolver) *Server {
	cat, err := catalog.Default()
	if err != nil {
		panic(err)
	}
	return New(nil, nil, render.New("en"), cat, users, "en")
}

func et(template string, channels ...string) catalog.EventType {
	return catalog.EventType{Type: "t." + template, Template: template, Channels: channels, Severity: "info"}
}

func TestInboxItemIsRenderedInTheUsersLanguage(t *testing.T) {
	s := newServer(fakeUsers{"u1": {ID: "u1", Email: "an@x.io", Name: "An", Locale: "vi"}})
	dels, inbox, skipped, err := s.userDeliveries(context.Background(), et("identity_registration_pending", "in_app"),
		[]string{"u1"}, map[string]string{"applicant_name": "Bo", "applicant_email": "bo@x.io", "link": "https://admin/users/9"}, "en")
	if err != nil || len(dels) != 0 || len(skipped) != 0 || len(inbox) != 1 {
		t.Fatalf("%v %v %v %v", dels, inbox, skipped, err)
	}
	in := inbox[0]
	if in.UserID != "u1" || in.Title != "Đăng ký mới đang chờ phê duyệt" || in.Link != "https://admin/users/9" || in.Severity != "info" {
		t.Fatalf("%+v", in)
	}
	if want := "Bo (bo@x.io) đang chờ quyết định của bạn."; in.Body != want {
		t.Fatalf("body %q", in.Body)
	}
}

func TestUserDeliveries(t *testing.T) {
	s := newServer(fakeUsers{
		"u1": {ID: "u1", Email: "an@x.io", Name: "An", Locale: "vi"},
		"u2": {ID: "u2", Email: "", Name: "NoMail"},
		"u3": {ID: "u3", Email: "bo@x.io", Name: "Bo"},
	})
	dels, _, skipped, err := s.userDeliveries(context.Background(), et("identity_password_changed", "email"),
		[]string{"u1", "u2", "u3", "ghost", "u1"}, map[string]string{}, "en")
	if err != nil {
		t.Fatal(err)
	}
	if len(dels) != 2 {
		t.Fatalf("deliveries: %+v", dels)
	}
	if dels[0].Address != "an@x.io" || dels[0].Locale != "vi" || dels[0].Vars["name"] != "An" || dels[0].UserID != "u1" {
		t.Fatalf("u1: %+v", dels[0])
	}
	if dels[1].Locale != "" {
		t.Fatalf("a user without locale takes the event locale: %+v", dels[1])
	}
	if len(skipped) != 2 || skipped[0] != "u2" || skipped[1] != "ghost" {
		t.Fatalf("skipped: %v", skipped)
	}
}

func TestUserDeliveriesTelegramOnlyEventSkipsUsers(t *testing.T) {
	s := newServer(fakeUsers{"u1": {ID: "u1", Email: "an@x.io"}})
	dels, _, skipped, err := s.userDeliveries(context.Background(), et("bvchain_checkin", "telegram"), []string{"u1"}, nil, "en")
	if err != nil || len(dels) != 0 || len(skipped) != 1 {
		t.Fatalf("%v %v %v", dels, skipped, err)
	}
}

func TestUserDeliveriesCallerNameWins(t *testing.T) {
	s := newServer(fakeUsers{"u1": {ID: "u1", Email: "an@x.io", Name: "An"}})
	dels, _, _, _ := s.userDeliveries(context.Background(), et("hello", "email"), []string{"u1"}, map[string]string{"name": "Given"}, "en")
	if _, set := dels[0].Vars["name"]; set {
		t.Fatalf("the name sent by the caller must not be overridden: %+v", dels[0].Vars)
	}
}

func TestUserIDsWithoutResolverRefused(t *testing.T) {
	s := newServer(nil)
	if _, _, _, err := s.userDeliveries(context.Background(), et("hello", "email"), []string{"u1"}, nil, "en"); err == nil {
		t.Fatal("user_ids without IDENTITY_TARGET must be refused")
	}
}

func TestStringData(t *testing.T) {
	st, _ := structpb.NewStruct(map[string]any{"s": "x", "n": 30, "f": 1.5, "b": true, "z": nil})
	got, err := stringData(st)
	if err != nil || got["s"] != "x" || got["n"] != "30" || got["f"] != "1.5" || got["b"] != "true" {
		t.Fatalf("%v %v", got, err)
	}
	if _, ok := got["z"]; ok {
		t.Fatal("null must be absent")
	}
	bad, _ := structpb.NewStruct(map[string]any{"l": []any{"a"}})
	if _, err := stringData(bad); err == nil {
		t.Fatal("lists must be rejected")
	}
}

// fakePolicy is what the users chose and where they can be reached.
type fakePolicy struct {
	prefs  map[string]map[string]map[string]bool
	quiet  map[string]store.QuietHours
	chats  map[string]string
	pushes map[string][]store.PushSub
}

func (f fakePolicy) Prefs(context.Context, []string) (map[string]map[string]map[string]bool, error) {
	return f.prefs, nil
}
func (f fakePolicy) Quiet(context.Context, []string) (map[string]store.QuietHours, error) {
	return f.quiet, nil
}
func (f fakePolicy) PushSubs(context.Context, []string) (map[string][]store.PushSub, error) {
	return f.pushes, nil
}
func (f fakePolicy) Bindings(context.Context, []string, string) (map[string]string, error) {
	return f.chats, nil
}

func policyServer(p fakePolicy) *Server {
	s := newServer(fakeUsers{"u1": {ID: "u1", Email: "an@x.io", Name: "An", Locale: "en"}, "u2": {ID: "u2", Email: "bo@x.io", Name: "Bo"}})
	s.policy = p
	s.now = func() time.Time { return time.Date(2026, 11, 28, 16, 0, 0, 0, time.UTC) } // 23:00 in Ho Chi Minh
	return s
}

func TestChoiceOfTheUserTurnsAChannelOff(t *testing.T) {
	off := map[string]map[string]map[string]bool{"u1": {"admin": {"email": false}}}
	s := policyServer(fakePolicy{prefs: off})
	e := catalog.EventType{Type: "account.registration_pending", Category: "admin", Template: "identity_registration_pending", Channels: []string{"email", "in_app"}, Severity: "info"}
	data := map[string]string{"applicant_name": "X", "applicant_email": "x@x.io"}
	dels, inbox, skipped, err := s.userDeliveries(context.Background(), e, []string{"u1", "u2"}, data, "en")
	if err != nil {
		t.Fatal(err)
	}
	var u1 store.NewDelivery
	for _, d := range dels {
		if d.UserID == "u1" {
			u1 = d
		}
	}
	if u1.Skip == "" || len(dels) != 2 {
		t.Fatalf("u1's e-mail must be kept as a skipped delivery: %+v", dels)
	}
	if len(inbox) != 2 || len(skipped) != 0 {
		t.Fatalf("the inbox is another channel (still on): %v skipped %v", inbox, skipped)
	}
	for _, d := range dels {
		if d.UserID == "u2" && d.Skip != "" {
			t.Fatalf("u2 chose nothing: %+v", d)
		}
	}
}

func TestMandatoryEventIgnoresTheChoice(t *testing.T) {
	off := map[string]map[string]map[string]bool{"u1": {"security": {"email": false, "in_app": false}}}
	quiet := map[string]store.QuietHours{"u1": {Timezone: "Asia/Ho_Chi_Minh", FromMin: 22 * 60, ToMin: 7 * 60}}
	s := policyServer(fakePolicy{prefs: off, quiet: quiet})
	e := catalog.EventType{Type: "security.mfa_changed", Category: "security", Mandatory: true, Template: "identity_mfa_changed", Channels: []string{"email", "in_app"}, Severity: "warning"}
	dels, inbox, _, err := s.userDeliveries(context.Background(), e, []string{"u1"}, map[string]string{"change": "enabled"}, "en")
	if err != nil || len(dels) != 1 || len(inbox) != 1 {
		t.Fatalf("%v %v %v", dels, inbox, err)
	}
	if dels[0].Skip != "" || !dels[0].NotBefore.IsZero() {
		t.Fatalf("a mandatory mail is neither skipped nor held back: %+v", dels[0])
	}
}

func TestQuietHoursHoldBackTheMessageNotTheInbox(t *testing.T) {
	quiet := map[string]store.QuietHours{"u1": {Timezone: "Asia/Ho_Chi_Minh", FromMin: 22 * 60, ToMin: 7 * 60}}
	s := policyServer(fakePolicy{quiet: quiet})
	e := catalog.EventType{Type: "account.registration_pending", Category: "admin", Template: "identity_registration_pending", Channels: []string{"email", "in_app"}, Severity: "info"}
	data := map[string]string{"applicant_name": "X", "applicant_email": "x@x.io"}
	dels, inbox, _, err := s.userDeliveries(context.Background(), e, []string{"u1", "u2"}, data, "en")
	if err != nil || len(inbox) != 2 {
		t.Fatalf("%v %v", inbox, err)
	}
	want := time.Date(2026, 11, 29, 0, 0, 0, 0, time.UTC) // 07:00 in Ho Chi Minh
	for _, d := range dels {
		switch d.UserID {
		case "u1":
			if !d.NotBefore.Equal(want) {
				t.Errorf("u1 waits until %v, got %v", want, d.NotBefore)
			}
		case "u2":
			if !d.NotBefore.IsZero() {
				t.Errorf("u2 has no quiet hours: %v", d.NotBefore)
			}
		}
	}
}

func TestTelegramGoesToTheLinkedChat(t *testing.T) {
	s := policyServer(fakePolicy{chats: map[string]string{"u1": "555"}})
	e := catalog.EventType{Type: "bvchain.checkin", Category: "bvchain", Template: "bvchain_checkin", Channels: []string{"telegram"}, Severity: "info"}
	dels, _, skipped, err := s.userDeliveries(context.Background(), e, []string{"u1", "u2"}, map[string]string{"shift": "1", "time": "08:00"}, "en")
	if err != nil {
		t.Fatal(err)
	}
	if len(dels) != 1 || dels[0].Channel != "telegram" || dels[0].Address != "555" || dels[0].UserID != "u1" {
		t.Fatalf("%+v", dels)
	}
	if len(skipped) != 1 || skipped[0] != "u2" {
		t.Fatalf("a user without a linked chat is skipped: %v", skipped)
	}
}

func TestPushGoesToEveryDeviceOfTheUser(t *testing.T) {
	subs := map[string][]store.PushSub{"u1": {{ID: "s1", UserID: "u1"}, {ID: "s2", UserID: "u1"}}}
	off := map[string]map[string]map[string]bool{"u2": {"admin": {"push": false}}}
	s := policyServer(fakePolicy{pushes: map[string][]store.PushSub{"u1": subs["u1"], "u2": {{ID: "s3", UserID: "u2"}}}, prefs: off})
	e := catalog.EventType{Type: "account.registration_pending", Category: "admin", Template: "identity_registration_pending", Channels: []string{"push"}, Severity: "info"}
	data := map[string]string{"applicant_name": "X", "applicant_email": "x@x.io"}
	dels, inbox, skipped, err := s.userDeliveries(context.Background(), e, []string{"u1", "u2", "u3"}, data, "en")
	if err != nil || len(inbox) != 0 {
		t.Fatalf("%v %v", inbox, err)
	}
	got := map[string]string{}
	for _, d := range dels {
		if d.Channel != "push" {
			t.Fatalf("%+v", d)
		}
		got[d.Address] = d.Skip
	}
	if len(got) != 3 || got["s1"] != "" || got["s2"] != "" || got["s3"] == "" {
		t.Fatalf("one delivery per device, the one of u2 kept as skipped: %v", got)
	}
	if len(skipped) != 2 || skipped[0] != "u2" || skipped[1] != "u3" { // u2 turned it off, u3 has no device
		t.Fatalf("skipped %v", skipped)
	}
}

func TestNotifyRefusesAnUnsafeLink(t *testing.T) {
	s := newServer(nil)
	data, _ := structpb.NewStruct(map[string]any{"applicant_name": "X", "applicant_email": "x@x.io", "link": "javascript:alert(1)"})
	_, err := s.Notify(context.Background(), &courierv1.NotifyRequest{
		EventType: "account.registration_pending", Data: data,
		Recipients: []*courierv1.Recipient{{Channel: courierv1.Channel_CHANNEL_EMAIL, Address: "a@x.io"}},
	})
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "link must be") {
		t.Fatalf("%v", err)
	}
}
