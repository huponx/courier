package push

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/huponx/courier/internal/notify"
	"github.com/huponx/courier/internal/store"
)

type fakeLookup map[string]store.PushSub

func (f fakeLookup) PushSub(_ context.Context, id string) (store.PushSub, error) {
	if s, ok := f[id]; ok {
		return s, nil
	}
	return store.PushSub{}, store.ErrNotFound
}

// a browser's keys: a P-256 public key (uncompressed, base64url) and 16 bytes of auth secret
func browserKeys(t *testing.T) (p256dh, auth string) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := make([]byte, 16)
	_, _ = rand.Read(a)
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(a)
}

func config(t *testing.T) Config {
	t.Helper()
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		t.Fatal(err)
	}
	return Config{PublicKey: pub, PrivateKey: priv, Subject: "mailto:ops@example.com", AllowPrivate: true}
}

func sender(t *testing.T, status int, seen *http.Header) (*Sender, notify.Destination) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			*seen = r.Header.Clone()
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	p, a := browserKeys(t)
	return New(config(t), fakeLookup{"s1": {ID: "s1", UserID: "u1", Endpoint: srv.URL + "/push/abc", P256dh: p, Auth: a}}), notify.Destination{Address: "s1"}
}

func TestSendEncryptsAndSignsWithVAPID(t *testing.T) {
	var h http.Header
	s, dest := sender(t, http.StatusCreated, &h)
	if err := s.Send(context.Background(), dest, notify.Message{Subject: "Title", Text: "Body", Link: "/x"}); err != nil {
		t.Fatal(err)
	}
	if h.Get("Content-Encoding") != "aes128gcm" || h.Get("Ttl") != "86400" || !strings.HasPrefix(h.Get("Authorization"), "vapid t=") {
		t.Fatalf("headers %v", h)
	}
}

func TestAnswersDecideWhetherToRetry(t *testing.T) {
	for status, c := range map[int]string{
		http.StatusNotFound: "gone", http.StatusGone: "gone",
		http.StatusInternalServerError: "retry", http.StatusServiceUnavailable: "retry", http.StatusTooManyRequests: "retry",
		http.StatusBadRequest: "final", http.StatusUnauthorized: "final", http.StatusRequestEntityTooLarge: "final",
	} {
		s, dest := sender(t, status, nil)
		err := s.Send(context.Background(), dest, notify.Message{Subject: "T", Text: "B"})
		switch c {
		case "gone":
			if !errors.Is(err, notify.ErrRecipientGone) || !notify.IsPermanent(err) {
				t.Errorf("%d: %v", status, err)
			}
		case "retry":
			if err == nil || notify.IsPermanent(err) {
				t.Errorf("%d must be retried: %v", status, err)
			}
		case "final":
			if err == nil || !notify.IsPermanent(err) || errors.Is(err, notify.ErrRecipientGone) {
				t.Errorf("%d must be final: %v", status, err)
			}
		}
	}
}

func TestMissingSubscriptionAndKeys(t *testing.T) {
	s, _ := sender(t, http.StatusCreated, nil)
	if err := s.Send(context.Background(), notify.Destination{Address: "nope"}, notify.Message{}); !errors.Is(err, notify.ErrRecipientGone) {
		t.Fatalf("a removed subscription: %v", err)
	}
	none := New(Config{}, fakeLookup{})
	if err := none.Send(context.Background(), notify.Destination{Address: "s1"}, notify.Message{}); err == nil || !notify.IsPermanent(err) {
		t.Fatalf("without VAPID keys: %v", err)
	}
}

func TestPrivateEndpointsAreRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("reached a loopback address") }))
	defer srv.Close()
	p, a := browserKeys(t)
	cfg := config(t)
	cfg.AllowPrivate = false
	s := New(cfg, fakeLookup{"s1": {ID: "s1", Endpoint: srv.URL, P256dh: p, Auth: a}})
	err := s.Send(context.Background(), notify.Destination{Address: "s1"}, notify.Message{Subject: "T"})
	if !errors.Is(err, notify.ErrRecipientGone) || !notify.IsPermanent(err) {
		t.Fatalf("%v", err)
	}
}
