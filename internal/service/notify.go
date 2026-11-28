package service

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/huponx/courier/internal/catalog"
	"github.com/huponx/courier/internal/directory"
	"github.com/huponx/courier/internal/metrics"
	"github.com/huponx/courier/internal/queue"
	"github.com/huponx/courier/internal/store"
	"github.com/huponx/gocommon/apperror"
	"github.com/huponx/gocommon/logging"
	"github.com/huponx/gocommon/netsafe"
	"github.com/huponx/gocommon/requestctx"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/huponx/courier/internal/notify/render"
)

// Notify sends an event of the catalog to users (resolved through identity) and raw recipients.
func (s *Server) Notify(ctx context.Context, req *courierv1.NotifyRequest) (*courierv1.NotifyResponse, error) {
	resp, err := s.notify(ctx, req)
	metrics.Accepted(resultOf(err))
	return resp, err
}

func (s *Server) notify(ctx context.Context, req *courierv1.NotifyRequest) (*courierv1.NotifyResponse, error) {
	eventType := strings.TrimSpace(req.GetEventType())
	if eventType == "" {
		return nil, apperror.InvalidArgument("event_type is required").WithReason("event_type_required")
	}
	et, ok := s.catalog.Get(eventType)
	if !ok {
		return nil, apperror.InvalidArgument("unknown event_type " + eventType).WithReason("event_type_unknown")
	}
	data, err := stringData(req.GetData())
	if err != nil {
		return nil, apperror.InvalidArgument(err.Error()).WithReason("data_invalid")
	}
	if err := et.Validate(data); err != nil {
		return nil, apperror.InvalidArgument(err.Error()).WithReason("data_invalid")
	}
	// the caller may narrow the channels of the type (an announcement to the bell only), never widen them
	if len(req.GetChannels()) > 0 {
		narrowed := make([]string, 0, len(req.GetChannels()))
		for _, ch := range req.GetChannels() {
			if !contains(et.Channels, ch) {
				return nil, apperror.InvalidArgument(fmt.Sprintf("%s has no %s channel", et.Type, ch)).WithReason("channel_unknown")
			}
			if !contains(narrowed, ch) {
				narrowed = append(narrowed, ch)
			}
		}
		et.Channels = narrowed
	}
	if l := data["link"]; l != "" && !netsafe.SafeLink(l) {
		return nil, apperror.InvalidArgument("link must be a path of the app (/users/1) or an http(s) URL").WithReason("link_invalid")
	}
	if len(req.GetUserIds()) == 0 && len(req.GetRecipients()) == 0 {
		return nil, apperror.InvalidArgument("user_ids or recipients is required").WithReason("recipients_required")
	}

	locale := render.NormalizeLocale(req.GetLocaleOverride())
	if locale == "" {
		locale = s.defaultLocale
	}

	dels, err := s.rawDeliveries(req.GetRecipients(), locale, et.Template)
	if err != nil {
		return nil, err
	}
	userDels, inbox, skipped, err := s.userDeliveries(ctx, et, req.GetUserIds(), data, locale)
	if err != nil {
		return nil, err
	}
	dels = append(dels, userDels...)
	hookDels, err := s.webhookDeliveries(ctx, et)
	if err != nil {
		return nil, err
	}
	dels = append(dels, hookDels...)
	s.applyDigest(et, data, dels)

	if len(dels) == 0 && len(inbox) == 0 {
		return &courierv1.NotifyResponse{SkippedUserIds: skipped}, nil
	}

	rc := requestctx.From(ctx)
	eventID, created, items, err := s.store.CreateEventWithInbox(ctx, store.NewEvent{
		IdempotencyKey: strings.TrimSpace(req.GetIdempotencyKey()),
		EventType:      et.Type, // the worker maps it to the template through the catalog
		Locale:         locale,
		Data:           data,
		CorrelationID:  rc.CorrelationID,
		RequestID:      rc.RequestID,
		UserID:         rc.UserID,
		TenantID:       rc.TenantID,
		AgencyID:       rc.AgencyID,
	}, dels, inbox)
	if err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	if len(created) == 0 && len(items) == 0 {
		// a repeated idempotency_key, or every delivery was skipped by the choice of the users (kept as audit rows)
		return &courierv1.NotifyResponse{EventId: eventID, SkippedUserIds: skipped}, nil
	}
	s.push(ctx, created)
	s.publishInbox(ctx, items)
	return &courierv1.NotifyResponse{EventId: eventID, Queued: int32(len(created)), SkippedUserIds: skipped}, nil
}

// userDeliveries resolves the users and builds, per user, what each channel of the event does:
//   - e-mail goes to the address of identity, Telegram to the chat the user linked (no link: not reachable);
//   - a channel the user turned off for the category is kept as a skipped delivery (the audit), or nothing for in_app,
//     unless the event is mandatory;
//   - a non-mandatory e-mail or Telegram message inside the quiet hours of the user waits for their end;
//   - in_app becomes an inbox item rendered in the language of the user.
//
// A user who ends with nothing sent and no inbox item is in the skipped list.
func (s *Server) userDeliveries(ctx context.Context, et catalog.EventType, ids []string, data map[string]string, locale string) ([]store.NewDelivery, []store.NewInbox, []string, error) {
	if len(ids) == 0 {
		return nil, nil, nil, nil
	}
	if s.users == nil {
		return nil, nil, nil, apperror.FailedPrecondition("user_ids need IDENTITY_TARGET: courier cannot resolve users").WithReason("user_resolution_disabled")
	}
	if et.Mandatory {
		ctx = directory.Fresh(ctx)
	}
	users, err := s.users.Users(ctx, ids)
	if err != nil {
		return nil, nil, nil, apperror.Unavailable("identity lookup failed").WithReason("identity_unavailable")
	}
	found := make([]string, 0, len(users))
	for id := range users {
		found = append(found, id)
	}
	var prefs map[string]map[string]map[string]bool
	var quiet map[string]store.QuietHours
	var chats map[string]string
	var pushes map[string][]store.PushSub
	if s.policy != nil {
		if prefs, err = s.policy.Prefs(ctx, found); err == nil {
			if quiet, err = s.policy.Quiet(ctx, found); err == nil && contains(et.Channels, "telegram") {
				chats, err = s.policy.Bindings(ctx, found, "telegram")
			}
			if err == nil && contains(et.Channels, "push") {
				pushes, err = s.policy.PushSubs(ctx, found)
			}
		}
		if err != nil {
			return nil, nil, nil, apperror.Unavailable("store failed").WithReason("store_failed")
		}
	}

	var dels []store.NewDelivery
	var inbox []store.NewInbox
	var skipped []string
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		u, ok := users[id]
		if !ok {
			skipped = append(skipped, id)
			continue
		}
		userLocale := render.NormalizeLocale(u.Locale)
		effective := userLocale
		if effective == "" {
			effective = locale
		}
		vars := map[string]string{}
		if data["name"] == "" && u.Name != "" {
			vars["name"] = u.Name
		}
		var wait time.Time // end of the quiet hours of this user, when now is inside
		if q, has := quiet[id]; has && !et.Mandatory {
			if end, inside := quietUntil(s.now(), q); inside {
				wait = end
			}
		}
		off := func(ch string) bool {
			on, set := prefs[id][et.Category][ch]
			return !et.Mandatory && set && !on
		}

		reached := false
		for _, ch := range et.Channels {
			switch ch {
			case "email", "telegram":
				address := strings.TrimSpace(u.Email)
				enum := courierv1.Channel_CHANNEL_EMAIL
				if ch == "telegram" {
					address, enum = chats[id], courierv1.Channel_CHANNEL_TELEGRAM
				}
				if address == "" {
					continue
				}
				if !s.renderer.Has(effective, et.Template, enum) {
					return nil, nil, nil, apperror.InvalidArgument("template not found for recipient channel").WithReason("template_not_found")
				}
				d := store.NewDelivery{Channel: ch, Address: address, UserID: id, Locale: userLocale, Vars: vars, NotBefore: wait}
				if off(ch) {
					d.Skip, d.NotBefore = "turned off by the user", time.Time{}
				} else {
					reached = true
				}
				dels = append(dels, d)
			case "push":
				for _, sub := range pushes[id] {
					if !s.renderer.Has(effective, et.Template, courierv1.Channel_CHANNEL_PUSH) {
						return nil, nil, nil, apperror.InvalidArgument("template not found for recipient channel").WithReason("template_not_found")
					}
					d := store.NewDelivery{Channel: ch, Address: sub.ID, UserID: id, Locale: userLocale, Vars: vars, NotBefore: wait}
					if off(ch) {
						d.Skip, d.NotBefore = "turned off by the user", time.Time{}
					} else {
						reached = true
					}
					dels = append(dels, d)
				}
			case "in_app":
				if off(ch) {
					continue
				}
				if !s.renderer.Has(effective, et.Template, courierv1.Channel_CHANNEL_IN_APP) {
					return nil, nil, nil, apperror.InvalidArgument("template not found for recipient channel").WithReason("template_not_found")
				}
				merged := make(map[string]string, len(data)+len(vars))
				for k, v := range data {
					merged[k] = v
				}
				for k, v := range vars {
					merged[k] = v
				}
				msg, err := s.renderer.Render(effective, et.Template, courierv1.Channel_CHANNEL_IN_APP, merged)
				if err != nil {
					return nil, nil, nil, apperror.InvalidArgument("inbox template failed: " + err.Error()).WithReason("template_render_failed")
				}
				inbox = append(inbox, store.NewInbox{UserID: id, Title: strings.TrimSpace(msg.Subject), Body: strings.TrimSpace(msg.Text), Link: data["link"], Severity: et.Severity})
				reached = true
			}
		}
		if !reached {
			skipped = append(skipped, id)
		}
	}
	return dels, inbox, skipped, nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// publishInbox tells the web apps of each user that a new item arrived (the SSE stream of the gateway listens).
// It runs after the commit and is best effort: the bell also polls the unread count.
func (s *Server) publishInbox(ctx context.Context, items []store.InboxItem) {
	for _, it := range items {
		unread, err := s.store.Unread(ctx, it.UserID)
		if err != nil {
			logging.FromCtx(ctx).Warn("inbox unread count failed", zap.Error(err))
			continue
		}
		if err := s.queue.PublishInbox(ctx, it.UserID, queue.InboxEvent{Unread: unread, Item: &queue.InboxItemJSON{
			ID: it.ID, Title: it.Title, Body: it.Body, Link: it.Link, Severity: it.Severity, CreatedAt: it.CreatedAt.Unix(),
		}}); err != nil {
			logging.FromCtx(ctx).Warn("inbox publish failed", zap.Error(err))
		}
	}
}

// stringData flattens the Struct of the request to the text values templates use. Numbers and booleans are
// formatted; nested lists and objects are rejected (the catalog has no such key).
func stringData(st *structpb.Struct) (map[string]string, error) {
	out := make(map[string]string, len(st.GetFields()))
	for k, v := range st.GetFields() {
		switch x := v.GetKind().(type) {
		case *structpb.Value_StringValue:
			out[k] = x.StringValue
		case *structpb.Value_NumberValue:
			out[k] = strconv.FormatFloat(x.NumberValue, 'f', -1, 64)
		case *structpb.Value_BoolValue:
			out[k] = strconv.FormatBool(x.BoolValue)
		case *structpb.Value_NullValue, nil:
			// an absent value
		default:
			return nil, fmt.Errorf("data.%s must be a string, number or boolean", k)
		}
	}
	return out, nil
}

func newID() string { return uuid.NewString() }

// resultOf classifies the result of a call for the metrics: ok, a caller mistake (invalid), or a failure of ours or of
// a dependency (error).
func resultOf(err error) string {
	switch status.Code(err) {
	case codes.OK:
		return "ok"
	case codes.InvalidArgument, codes.FailedPrecondition, codes.NotFound:
		return "invalid"
	}
	return "error"
}

type noWebhooksKey struct{}

// withoutWebhooks marks a call whose event must not go to the webhooks again: the later pages of an announcement
// (the first page sends it once for the whole announcement).
func withoutWebhooks(ctx context.Context) context.Context {
	return context.WithValue(ctx, noWebhooksKey{}, true)
}

// webhookDeliveries is one delivery per webhook subscribed to the event type, per event (not per user). A mandatory
// event (security, account) never goes to a webhook: it carries links and codes.
func (s *Server) webhookDeliveries(ctx context.Context, et catalog.EventType) ([]store.NewDelivery, error) {
	if s.hooks == nil || et.Mandatory || ctx.Value(noWebhooksKey{}) != nil {
		return nil, nil
	}
	hooks, err := s.hooks.WebhooksFor(ctx, et.Type)
	if err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	out := make([]store.NewDelivery, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, store.NewDelivery{Channel: "webhook", Address: h.ID})
	}
	return out, nil
}

// applyDigest puts the deliveries of a digest type in their group: the group key, and a first attempt at the end of
// the window (later than the end of the quiet hours when that is later). The first delivery of a group becomes its
// leader, the others are attached to it by the store.
func (s *Server) applyDigest(et catalog.EventType, data map[string]string, dels []store.NewDelivery) {
	if et.Digest == nil {
		return
	}
	key := et.Digest.Key(et.Type, data)
	end := s.now().Add(et.Digest.Window)
	for i := range dels {
		if dels[i].Skip != "" {
			continue
		}
		dels[i].DigestKey = key
		if dels[i].NotBefore.Before(end) {
			dels[i].NotBefore = end
		}
	}
}
