// Package push sends a browser notification (Web Push, RFC 8030 with VAPID, RFC 8292) to a subscription of a user.
package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/huponx/courier/internal/notify"
	"github.com/huponx/courier/internal/store"
	"github.com/huponx/gocommon/netsafe"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

// Lookup finds the subscription a delivery points at (its address is the id) and forgets one that is gone.
type Lookup interface {
	PushSub(ctx context.Context, id string) (store.PushSub, error)
}

type Config struct {
	// PublicKey and PrivateKey are the VAPID key pair (base64url); Subject is "mailto:..." or an https URL that the push
	// services can contact.
	PublicKey, PrivateKey, Subject string
	AllowPrivate                   bool
}

// Available reports whether the keys are set: without them there is no Web Push.
func (c Config) Available() bool { return c.PublicKey != "" && c.PrivateKey != "" }

type Sender struct {
	cfg    Config
	lookup Lookup
	client *http.Client
}

func New(cfg Config, lookup Lookup) *Sender {
	return &Sender{cfg: cfg, lookup: lookup, client: netsafe.Client(15*time.Second, cfg.AllowPrivate)}
}

func (s *Sender) ID() courierv1.Channel { return courierv1.Channel_CHANNEL_PUSH }

// Payload is what the service worker of the portal receives and shows.
type Payload struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// URL opens when the notification is clicked: a path of the portal or an absolute URL; empty: the portal.
	URL string `json:"url,omitempty"`
}

// Send encrypts and posts the notification. A subscription the push service no longer knows (404, 410) is a final
// error that also drops the subscription (notify.ErrRecipientGone); 429 and 5xx are retried, any other 4xx is final.
func (s *Sender) Send(ctx context.Context, dest notify.Destination, msg notify.Message) error {
	if !s.cfg.Available() {
		return notify.Permanent(errors.New("web push is not configured (VAPID keys)"))
	}
	sub, err := s.lookup.PushSub(ctx, dest.Address)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: %w", notify.ErrRecipientGone, notify.Permanent(errors.New("the subscription was removed")))
	}
	if err != nil {
		return err
	}
	body, err := json.Marshal(Payload{Title: strings.TrimSpace(msg.Subject), Body: strings.TrimSpace(msg.Text), URL: msg.Link})
	if err != nil {
		return notify.Permanent(err)
	}
	resp, err := webpush.SendNotificationWithContext(ctx, body, &webpush.Subscription{
		Endpoint: sub.Endpoint, Keys: webpush.Keys{P256dh: sub.P256dh, Auth: sub.Auth},
	}, &webpush.Options{
		HTTPClient: s.client, Subscriber: s.cfg.Subject, VAPIDPublicKey: s.cfg.PublicKey, VAPIDPrivateKey: s.cfg.PrivateKey,
		TTL: 24 * 3600, Urgency: webpush.UrgencyNormal,
	})
	if err != nil {
		err = netsafe.Scrub(err) // the endpoint is the address of the device
		if netsafe.IsRefused(err) {
			return fmt.Errorf("%w: %w", notify.ErrRecipientGone, notify.Permanent(err))
		}
		// bad keys of the subscription cannot be fixed by a retry either
		if strings.Contains(err.Error(), "invalid") || strings.Contains(err.Error(), "decode") {
			return notify.Permanent(err)
		}
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return fmt.Errorf("%w: %w", notify.ErrRecipientGone, notify.Permanent(fmt.Errorf("push service answered %d: the subscription is gone", resp.StatusCode)))
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("push service answered %d", resp.StatusCode)
	default:
		return notify.Permanent(fmt.Errorf("push service answered %d", resp.StatusCode))
	}
}
