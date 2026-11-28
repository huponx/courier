package main

import (
	"strings"
	"testing"

	"github.com/huponx/courier/internal/catalog"
	"github.com/huponx/courier/internal/notify/render"
	"github.com/huponx/courier/internal/store"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

func pending(name, email string) map[string]string {
	return map[string]string{"applicant_name": name, "applicant_email": email}
}

func TestComposeWithoutItemsIsTheEventItself(t *testing.T) {
	cat, _ := catalog.Default()
	r := render.New("en")
	w := store.Work{Event: store.Event{EventType: "account.registration_pending", Data: pending("A", "a@x.io")}, Delivery: store.Delivery{Vars: map[string]string{"name": "Admin"}}}
	tpl, data := compose(r, cat, w, nil, "en")
	if tpl != "identity_registration_pending" || data["name"] != "Admin" || data["applicant_name"] != "A" || data["count"] != "" {
		t.Fatalf("%s %v", tpl, data)
	}
}

func TestComposeDigestCountsAndListsTheEvents(t *testing.T) {
	cat, _ := catalog.Default()
	r := render.New("en")
	w := store.Work{
		Event:    store.Event{EventType: "account.registration_pending", Data: pending("A", "a@x.io")},
		Delivery: store.Delivery{Vars: map[string]string{"name": "Admin"}},
	}
	items := []store.Event{
		{EventType: "account.registration_pending", Data: pending("B", "b@x.io")},
		{EventType: "account.registration_pending", Data: pending("C", "c@x.io")},
	}
	tpl, data := compose(r, cat, w, items, "vi")
	if tpl != "identity_registration_pending_digest" || data["count"] != "3" || data["name"] != "Admin" {
		t.Fatalf("%s %v", tpl, data)
	}
	want := "A (a@x.io) đang chờ quyết định của bạn.\nB (b@x.io) đang chờ quyết định của bạn.\nC (c@x.io) đang chờ quyết định của bạn."
	if data["items"] != want {
		t.Fatalf("items:\n%s", data["items"])
	}
	// the digest template renders in every channel of the type
	m, err := r.Render("vi", tpl, courierv1.Channel_CHANNEL_EMAIL, data)
	if err != nil || strings.TrimSpace(m.Subject) != "3 đăng ký đang chờ phê duyệt" || !strings.Contains(m.Text, "Xin chào Admin") || !strings.Contains(m.Text, "B (b@x.io)") {
		t.Fatalf("email: %+v %v", m, err)
	}
	p, err := r.Render("vi", tpl, courierv1.Channel_CHANNEL_PUSH, data)
	if err != nil || strings.TrimSpace(p.Subject) != "3 đăng ký đang chờ phê duyệt" || strings.TrimSpace(p.Text) != want {
		t.Fatalf("push: %+v %v", p, err)
	}
}

func TestComposeDigestOfAnEventWithOnlyATelegramText(t *testing.T) {
	cat, _ := catalog.Default()
	r := render.New("en")
	fail := func(e string) map[string]string { return map[string]string{"step": "login", "error": e} }
	w := store.Work{Event: store.Event{EventType: "bvchain.failed", Data: fail("timeout")}}
	tpl, data := compose(r, cat, w, []store.Event{{EventType: "bvchain.failed", Data: fail("otp")}}, "en")
	if data["items"] != "❌ bvchain failed at login: timeout\n❌ bvchain failed at login: otp" {
		t.Fatalf("items: %q", data["items"])
	}
	m, err := r.Render("en", tpl, courierv1.Channel_CHANNEL_TELEGRAM, data)
	if err != nil || strings.TrimSpace(m.Text) != "❌ bvchain failed 2 times at login:\n❌ bvchain failed at login: timeout\n❌ bvchain failed at login: otp" {
		t.Fatalf("telegram: %q %v", m.Text, err)
	}
	// a webhook gets the digest as its message, with the count in the data
	msg := webhookMessage(r, w, "en", tpl, data, true)
	if strings.TrimSpace(msg.Subject) != "bvchain failed 2 times at login" || msg.Event.Data["count"] != "2" {
		t.Fatalf("webhook: %+v", msg)
	}
}
