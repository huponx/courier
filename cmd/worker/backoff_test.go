package main

import (
	"testing"
	"time"

	"github.com/huponx/courier/internal/catalog"
	"github.com/huponx/courier/internal/notify/render"
	"github.com/huponx/courier/internal/store"
)

func TestBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{0: time.Minute, 1: time.Minute, 2: 5 * time.Minute, 3: 30 * time.Minute, 4: 2 * time.Hour, 9: 2 * time.Hour} {
		if got := backoff(n); got != want {
			t.Errorf("backoff(%d) = %s, want %s", n, got, want)
		}
	}
}

func TestWebhookMessage(t *testing.T) {
	cat, err := catalog.Default()
	if err != nil {
		t.Fatal(err)
	}
	r := render.New("en")
	// an event with an inbox template: its title and body, in the language of the event
	w := store.Work{Event: store.Event{ID: "e1", EventType: "system.announcement", Locale: "vi", Data: map[string]string{"title": "Bảo trì", "message": "Tối nay"}}}
	m := webhookMessage(r, w, "vi", cat.TemplateFor(w.Event.EventType), w.Event.Data, false)
	if m.Subject != "Bảo trì" || m.Text != "Tối nay" || m.Event.ID != "e1" || m.Event.Type != "system.announcement" {
		t.Fatalf("%+v", m)
	}
	// an event without one: the type and its values, in a stable order
	w = store.Work{Event: store.Event{ID: "e2", EventType: "bvchain.failed", Data: map[string]string{"step": "login", "error": "boom"}}}
	m = webhookMessage(r, w, "en", cat.TemplateFor(w.Event.EventType), w.Event.Data, false)
	if m.Subject != "bvchain.failed" || m.Text != "error: boom\nstep: login" || m.Event.Data["step"] != "login" {
		t.Fatalf("%+v", m)
	}
}
