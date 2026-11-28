// Package webhook delivers an event to an HTTP endpoint of the administrators: a Slack incoming webhook, or any service
// that accepts a signed JSON POST.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/huponx/courier/internal/notify"
	"github.com/huponx/courier/internal/store"
	"github.com/huponx/gocommon/netsafe"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

// Lookup finds the webhook a delivery points at (its address is the id).
type Lookup interface {
	Webhook(ctx context.Context, id string) (store.Webhook, error)
}

type Sender struct {
	lookup Lookup
	client *http.Client
	now    func() time.Time
}

// New builds the sender. allowPrivate lets it reach loopback and private addresses (a local run); otherwise a
// webhook can only point to the public internet, so an administrator cannot aim courier at the cluster.
func New(lookup Lookup, allowPrivate bool) *Sender {
	return &Sender{lookup: lookup, now: time.Now, client: netsafe.Client(10*time.Second, allowPrivate)}
}

// Public reports whether the address belongs to the public internet.
func Public(ip net.IP) bool { return netsafe.Public(ip) }

// ValidateURL checks the address of a webhook: https (http only when allowPrivate), with a host and no credentials.
func ValidateURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return errors.New("the URL is not valid")
	}
	if u.User != nil {
		return errors.New("the URL must not carry a user and a password")
	}
	if u.Scheme != "https" && !(allowPrivate && u.Scheme == "http") {
		return errors.New("the URL must be https")
	}
	return nil
}

func (s *Sender) ID() courierv1.Channel { return courierv1.Channel_CHANNEL_WEBHOOK }

// Send posts the message. The delivery stays retryable for a network error, a 429 and a 5xx; any other answer is final.
func (s *Sender) Send(ctx context.Context, dest notify.Destination, msg notify.Message) error {
	w, err := s.lookup.Webhook(ctx, dest.Address)
	if errors.Is(err, store.ErrNotFound) {
		return notify.Permanent(errors.New("the webhook was deleted"))
	}
	if err != nil {
		return err
	}
	if !w.Enabled {
		return notify.Permanent(errors.New("the webhook is disabled"))
	}
	body, err := Body(w.Kind, msg)
	if err != nil {
		return notify.Permanent(err)
	}
	status, _, err := s.post(ctx, w, body, msg.Event)
	if err != nil {
		return err
	}
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusTooManyRequests || status >= 500:
		return fmt.Errorf("webhook answered %d", status)
	default:
		return notify.Permanent(fmt.Errorf("webhook answered %d", status))
	}
}

// Test posts a message to the webhook and returns what it answered (the status, a start of the body).
func (s *Sender) Test(ctx context.Context, w store.Webhook) (int, string, error) {
	msg := notify.Message{
		Subject: "Courier test", Text: "This is a test message from courier.",
		Event: &notify.EventInfo{ID: "test", Type: "courier.test", CreatedAt: s.now(), Data: map[string]string{}},
	}
	body, err := Body(w.Kind, msg)
	if err != nil {
		return 0, "", err
	}
	return s.post(ctx, w, body, msg.Event)
}

// Body is the JSON of the message for the kind of webhook.
func Body(kind string, msg notify.Message) ([]byte, error) {
	switch kind {
	case "slack":
		text := msg.Text
		if msg.Subject != "" {
			text = "*" + msg.Subject + "*\n" + msg.Text
		}
		return json.Marshal(map[string]string{"text": strings.TrimSpace(text)})
	case "generic":
		doc := map[string]any{"title": msg.Subject, "text": msg.Text}
		if e := msg.Event; e != nil {
			doc["id"], doc["event_type"], doc["created_at"], doc["data"] = e.ID, e.Type, e.CreatedAt.UTC().Format(time.RFC3339), e.Data
		}
		return json.Marshal(doc)
	}
	return nil, fmt.Errorf("unknown webhook kind %q", kind)
}

// Sign is the signature of a generic webhook: HMAC-SHA256 of "<timestamp>.<body>", hex, prefixed "sha256=". The
// receiver recomputes it with the shared secret and refuses a timestamp that is too old.
func Sign(secret, timestamp string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(timestamp + "."))
	m.Write(body)
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

func (s *Sender) post(ctx context.Context, w store.Webhook, body []byte, e *notify.EventInfo) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return 0, "", notify.Permanent(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "courier")
	if w.Kind == "generic" {
		ts := strconv.FormatInt(s.now().Unix(), 10)
		req.Header.Set("X-Courier-Timestamp", ts)
		if w.Secret != "" {
			req.Header.Set("X-Courier-Signature", Sign(w.Secret, ts, body))
		}
		if e != nil {
			req.Header.Set("X-Courier-Event", e.Type)
		}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		err = netsafe.Scrub(err) // the URL of a Slack webhook is the secret
		// a dial refused by the address check is final: retrying cannot make the address public
		if netsafe.IsRefused(err) {
			return 0, "", notify.Permanent(err)
		}
		return 0, "", err
	}
	defer resp.Body.Close()
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	return resp.StatusCode, strings.TrimSpace(string(snippet)), nil
}
