package webhook

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/huponx/courier/internal/notify"
	"github.com/huponx/courier/internal/store"
)

type fakeLookup map[string]store.Webhook

func (f fakeLookup) Webhook(_ context.Context, id string) (store.Webhook, error) {
	if w, ok := f[id]; ok {
		return w, nil
	}
	return store.Webhook{}, store.ErrNotFound
}

var event = &notify.EventInfo{ID: "ev1", Type: "bvchain.failed", CreatedAt: time.Date(2026, 11, 28, 1, 2, 3, 0, time.UTC), Data: map[string]string{"step": "login"}}

func send(t *testing.T, w store.Webhook, allowPrivate bool) error {
	t.Helper()
	w.ID, w.Enabled = "w1", true
	return New(fakeLookup{"w1": w}, allowPrivate).Send(context.Background(), notify.Destination{Address: "w1"},
		notify.Message{Subject: "Failed", Text: "step login", Event: event})
}

func TestSlackBody(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		if r.Header.Get("X-Courier-Signature") != "" {
			t.Error("a Slack webhook is not signed")
		}
	}))
	defer srv.Close()
	if err := send(t, store.Webhook{URL: srv.URL, Kind: "slack"}, true); err != nil {
		t.Fatal(err)
	}
	if got["text"] != "*Failed*\nstep login" {
		t.Fatalf("%v", got)
	}
}

func TestGenericBodyIsSigned(t *testing.T) {
	var body []byte
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		hdr = r.Header.Clone()
	}))
	defer srv.Close()
	if err := send(t, store.Webhook{URL: srv.URL, Kind: "generic", Secret: "s3cret"}, true); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		ID        string            `json:"id"`
		EventType string            `json:"event_type"`
		CreatedAt string            `json:"created_at"`
		Title     string            `json:"title"`
		Data      map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.ID != "ev1" || doc.EventType != "bvchain.failed" || doc.Title != "Failed" || doc.Data["step"] != "login" || doc.CreatedAt != "2026-11-28T01:02:03Z" {
		t.Fatalf("body %s (%v)", body, err)
	}
	ts := hdr.Get("X-Courier-Timestamp")
	if n, err := strconv.ParseInt(ts, 10, 64); err != nil || time.Since(time.Unix(n, 0)) > time.Minute {
		t.Fatalf("timestamp %q", ts)
	}
	// the receiver recomputes the signature with the shared secret
	if want := Sign("s3cret", ts, body); hdr.Get("X-Courier-Signature") != want {
		t.Fatalf("signature %q, want %q", hdr.Get("X-Courier-Signature"), want)
	}
	if Sign("other", ts, body) == hdr.Get("X-Courier-Signature") || hdr.Get("X-Courier-Event") != "bvchain.failed" {
		t.Fatal("another secret must give another signature")
	}
}

func TestAnswersDecideWhetherToRetry(t *testing.T) {
	for status, wantPermanent := range map[int]bool{http.StatusInternalServerError: false, http.StatusBadGateway: false, http.StatusTooManyRequests: false, http.StatusNotFound: true, http.StatusForbidden: true, http.StatusFound: true} {
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
			if status == http.StatusFound {
				rw.Header().Set("Location", "http://127.0.0.1:1/elsewhere")
			}
			rw.WriteHeader(status)
		}))
		err := send(t, store.Webhook{URL: srv.URL, Kind: "slack"}, true)
		srv.Close()
		if err == nil || notify.IsPermanent(err) != wantPermanent {
			t.Errorf("%d: err=%v permanent=%v, want permanent=%v", status, err, notify.IsPermanent(err), wantPermanent)
		}
	}
}

func TestSuccessStatuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	if err := send(t, store.Webhook{URL: srv.URL, Kind: "slack"}, true); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateAddressesAreRefusedUnlessAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		t.Error("the request must not reach a loopback address")
	}))
	defer srv.Close()
	err := send(t, store.Webhook{URL: srv.URL, Kind: "slack"}, false)
	if err == nil || !notify.IsPermanent(err) || !strings.Contains(err.Error(), "not a public address") {
		t.Fatalf("%v", err)
	}
}

func TestDeletedAndDisabledWebhooksFailForGood(t *testing.T) {
	s := New(fakeLookup{"off": {ID: "off", Enabled: false, URL: "https://x.example", Kind: "slack"}}, true)
	for _, id := range []string{"gone", "off"} {
		if err := s.Send(context.Background(), notify.Destination{Address: id}, notify.Message{}); err == nil || !notify.IsPermanent(err) {
			t.Errorf("%s: %v", id, err)
		}
	}
}

func TestValidateURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://hooks.slack.com/services/T/B/x": true, "http://example.com/x": false, "ftp://example.com": false, "https://user:pw@example.com/": false, "not a url": false, "https:///path": false,
	} {
		if err := ValidateURL(raw, false); (err == nil) != ok {
			t.Errorf("%q: %v", raw, err)
		}
	}
	if err := ValidateURL("http://localhost:8080/x", true); err != nil {
		t.Errorf("http is allowed locally: %v", err)
	}
}

func TestPublic(t *testing.T) {
	for ip, want := range map[string]bool{"8.8.8.8": true, "127.0.0.1": false, "10.1.2.3": false, "192.168.0.1": false, "169.254.169.254": false, "172.16.0.1": false, "::1": false, "0.0.0.0": false, "2606:4700::1111": true} {
		if got := Public(net.ParseIP(ip)); got != want {
			t.Errorf("%s: %v", ip, got)
		}
	}
}
