package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type fakeBinder struct {
	got map[string]string
}

var errInvalid = errors.New("invalid")

func (f *fakeBinder) Bind(_ context.Context, code, chat string) error {
	if code != "GOODCODE" {
		return errInvalid
	}
	f.got[code] = chat
	return nil
}

// A fake Bot API: getUpdates serves the queued updates once and then stops the poller; sendMessage is recorded.
func TestPollerLinksTheChatWithTheCode(t *testing.T) {
	var mu sync.Mutex
	var sent []map[string]any
	var offsets []float64
	ctx, cancel := context.WithCancel(context.Background())
	updates := `[
	 {"update_id":10,"message":{"text":"/start goodcode","from":{"language_code":"vi"},"chat":{"id":42,"type":"private"}}},
	 {"update_id":11,"message":{"text":"/start nope","from":{"language_code":"en"},"chat":{"id":43,"type":"private"}}},
	 {"update_id":12,"message":{"text":"/start","chat":{"id":44,"type":"private"}}},
	 {"update_id":13,"message":{"text":"/start goodcode","chat":{"id":-100,"type":"group"}}},
	 {"update_id":14,"message":{"text":"hello","chat":{"id":45,"type":"private"}}}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in map[string]any
		_ = json.Unmarshal(body, &in)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			offsets = append(offsets, in["offset"].(float64))
			if len(offsets) == 1 {
				_, _ = w.Write([]byte(`{"ok":true,"result":` + updates + `}`))
				return
			}
			cancel()
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			sent = append(sent, in)
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
		}
	}))
	defer srv.Close()

	b := &fakeBinder{got: map[string]string{}}
	p := &Poller{Token: "T", BaseURL: srv.URL, Binder: b, IsInvalid: func(err error) bool { return errors.Is(err, errInvalid) }, Wait: 1}
	if err := p.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if b.got["GOODCODE"] != "42" || len(b.got) != 1 {
		t.Fatalf("only the private chat with a good code links (case-insensitive code): %v", b.got)
	}
	if len(offsets) < 2 || offsets[0] != 0 || offsets[1] != 15 {
		t.Fatalf("the offset moves past the last update: %v", offsets)
	}
	if len(sent) != 3 {
		t.Fatalf("answers to the 3 private /start: %v", sent)
	}
	if !strings.Contains(sent[0]["text"].(string), "Đã liên kết") {
		t.Errorf("language of the user: %v", sent[0])
	}
	if !strings.Contains(sent[1]["text"].(string), "not valid") || !strings.Contains(sent[2]["text"].(string), "Open the link") {
		t.Errorf("answers: %v %v", sent[1], sent[2])
	}
}

func TestPollerStopsOnTelegramError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Conflict: terminated by other getUpdates request"}`))
	}))
	defer srv.Close()
	p := &Poller{Token: "T", BaseURL: srv.URL, Binder: &fakeBinder{}, Wait: 1}
	if err := p.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "Conflict") {
		t.Fatalf("got %v", err)
	}
}
