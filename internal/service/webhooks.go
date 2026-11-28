package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/huponx/courier/internal/notify/webhook"
	"github.com/huponx/courier/internal/store"
	"github.com/huponx/gocommon/apperror"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

// WithWebhooks gives the admin what it needs to test a webhook and to check an address; without it the webhook RPCs are
// refused. allowPrivate is WEBHOOK_ALLOW_PRIVATE.
func (s *Admin) WithWebhooks(sender *webhook.Sender, allowPrivate bool) *Admin {
	s.hooks, s.allowPrivate = sender, allowPrivate
	return s
}

func toWebhook(w store.Webhook) *courierv1.Webhook {
	return &courierv1.Webhook{
		Id: w.ID, Name: w.Name, Url: w.URL, Kind: w.Kind, Events: w.Events, Enabled: w.Enabled,
		HasSecret: w.Secret != "", CreatedAt: w.CreatedAt.Unix(),
	}
}

func (s *Admin) ListWebhooks(ctx context.Context, _ *courierv1.ListWebhooksRequest) (*courierv1.ListWebhooksResponse, error) {
	if err := needPrincipal(ctx); err != nil {
		return nil, err
	}
	ws, err := s.store.Webhooks(ctx)
	if err != nil {
		return nil, storeFailed()
	}
	out := &courierv1.ListWebhooksResponse{}
	for _, w := range ws {
		out.Webhooks = append(out.Webhooks, toWebhook(w))
	}
	return out, nil
}

// checkEvents accepts "*", a prefix "x.*" that covers some type, or the exact name of a type that may go to a webhook.
func (s *Admin) checkEvents(events []string) ([]string, error) {
	if len(events) == 0 {
		return nil, apperror.InvalidArgument("choose at least one event").WithReason("events_required")
	}
	var out []string
	for _, e := range events {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		switch {
		case e == "*":
		case strings.HasSuffix(e, ".*"):
			covers := false
			for _, et := range s.catalog.Types() {
				covers = covers || strings.HasPrefix(et.Type, strings.TrimSuffix(e, "*"))
			}
			if !covers {
				return nil, apperror.InvalidArgument("no event type starts with " + strings.TrimSuffix(e, "*")).WithReason("event_unknown")
			}
		default:
			et, ok := s.catalog.Get(e)
			if !ok {
				return nil, apperror.InvalidArgument("unknown event type " + e).WithReason("event_unknown")
			}
			if et.Mandatory {
				return nil, apperror.InvalidArgument(e + " is mandatory (security, account): it never goes to a webhook").WithReason("event_not_for_webhooks")
			}
		}
		out = append(out, e)
	}
	if len(out) == 0 {
		return nil, apperror.InvalidArgument("choose at least one event").WithReason("events_required")
	}
	return out, nil
}

func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (s *Admin) SaveWebhook(ctx context.Context, req *courierv1.SaveWebhookRequest) (*courierv1.SaveWebhookResponse, error) {
	if err := needPrincipal(ctx); err != nil {
		return nil, err
	}
	in := req.GetWebhook()
	name := strings.TrimSpace(in.GetName())
	if name == "" || len(name) > 100 {
		return nil, apperror.InvalidArgument("a name of 1 to 100 characters is required").WithReason("webhook_name_invalid")
	}
	if k := in.GetKind(); k != "slack" && k != "generic" {
		return nil, apperror.InvalidArgument("kind is slack or generic").WithReason("webhook_kind_invalid")
	}
	if err := webhook.ValidateURL(in.GetUrl(), s.allowPrivate); err != nil {
		return nil, apperror.InvalidArgument(err.Error()).WithReason("webhook_url_invalid")
	}
	events, err := s.checkEvents(in.GetEvents())
	if err != nil {
		return nil, err
	}

	w := store.Webhook{ID: strings.TrimSpace(req.GetId()), Name: name, URL: strings.TrimSpace(in.GetUrl()), Kind: in.GetKind(), Events: events, Enabled: in.GetEnabled(), CreatedBy: actor(ctx)}
	var secret string
	if w.Kind == "generic" {
		var existing store.Webhook
		if w.ID != "" {
			if existing, err = s.store.Webhook(ctx, w.ID); errors.Is(err, store.ErrNotFound) {
				return nil, apperror.NotFound("webhook not found").WithReason("webhook_not_found")
			} else if err != nil {
				return nil, storeFailed()
			}
		}
		// a new generic webhook, a rotation, or a webhook that became generic gets a secret, shown once
		if w.ID == "" || req.GetRotateSecret() || existing.Secret == "" {
			if secret, err = newSecret(); err != nil {
				return nil, apperror.Internal("could not make a secret")
			}
			w.Secret = secret
		}
	}
	saved, err := s.store.SaveWebhook(ctx, w)
	if errors.Is(err, store.ErrNotFound) {
		return nil, apperror.NotFound("webhook not found").WithReason("webhook_not_found")
	}
	if err != nil {
		return nil, storeFailed()
	}
	return &courierv1.SaveWebhookResponse{Webhook: toWebhook(saved), Secret: secret}, nil
}

func (s *Admin) DeleteWebhook(ctx context.Context, req *courierv1.DeleteWebhookRequest) (*courierv1.DeleteWebhookResponse, error) {
	if err := needPrincipal(ctx); err != nil {
		return nil, err
	}
	err := s.store.DeleteWebhook(ctx, req.GetId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, apperror.NotFound("webhook not found").WithReason("webhook_not_found")
	}
	if err != nil {
		return nil, storeFailed()
	}
	return &courierv1.DeleteWebhookResponse{}, nil
}

func (s *Admin) TestWebhook(ctx context.Context, req *courierv1.TestWebhookRequest) (*courierv1.TestWebhookResponse, error) {
	if err := needPrincipal(ctx); err != nil {
		return nil, err
	}
	if s.hooks == nil {
		return nil, apperror.FailedPrecondition("webhooks are not available").WithReason("webhooks_unavailable")
	}
	w, err := s.store.Webhook(ctx, req.GetId())
	if errors.Is(err, store.ErrNotFound) {
		return nil, apperror.NotFound("webhook not found").WithReason("webhook_not_found")
	}
	if err != nil {
		return nil, storeFailed()
	}
	status, body, err := s.hooks.Test(ctx, w)
	if err != nil {
		return &courierv1.TestWebhookResponse{Ok: false, Result: err.Error()}, nil
	}
	result := fmt.Sprintf("HTTP %d", status)
	if body != "" {
		result += ": " + body
	}
	return &courierv1.TestWebhookResponse{Ok: status >= 200 && status < 300, Result: result}, nil
}
