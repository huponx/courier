package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/huponx/courier/internal/catalog"
	"github.com/huponx/courier/internal/directory"
	"github.com/huponx/courier/internal/metrics"
	"github.com/huponx/courier/internal/notify"
	"github.com/huponx/courier/internal/notify/render"
	"github.com/huponx/courier/internal/queue"
	"github.com/huponx/courier/internal/store"
	"github.com/huponx/gocommon/apperror"
	"github.com/huponx/gocommon/logging"
	"github.com/huponx/gocommon/requestctx"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
	"go.uber.org/zap"
)

type Server struct {
	courierv1.UnimplementedCourierServiceServer
	store    *store.Store
	queue    *queue.Client
	renderer *render.Renderer
	catalog  *catalog.Catalog
	// users resolves user_ids of Notify; nil when courier has no IDENTITY_TARGET (Notify then takes raw recipients only).
	users         directory.Resolver
	defaultLocale string
	// policy is what the users chose and where they can be reached (the store; a fake in unit tests).
	policy userPolicy
	// hooks lists the webhooks an event goes to (the store; nil in unit tests that have none).
	hooks webhookSource
	now   func() time.Time
}

// webhookSource is the part of the store that Notify reads to find the webhooks of an event.
type webhookSource interface {
	WebhooksFor(ctx context.Context, eventType string) ([]store.Webhook, error)
}

// userPolicy is the part of the store that Notify reads per user.
type userPolicy interface {
	Prefs(ctx context.Context, userIDs []string) (map[string]map[string]map[string]bool, error)
	Quiet(ctx context.Context, userIDs []string) (map[string]store.QuietHours, error)
	Bindings(ctx context.Context, userIDs []string, channel string) (map[string]string, error)
	PushSubs(ctx context.Context, userIDs []string) (map[string][]store.PushSub, error)
}

// New builds the service. users may be nil.
func New(st *store.Store, q *queue.Client, renderer *render.Renderer, cat *catalog.Catalog, users directory.Resolver, defaultLocale string) *Server {
	s := &Server{store: st, queue: q, renderer: renderer, catalog: cat, users: users, defaultLocale: render.NormalizeLocale(defaultLocale), now: time.Now}
	if st != nil { // a nil *Store in the interface would not be nil
		s.policy = st
		s.hooks = st
	}
	return s
}

func (s *Server) SendNotification(ctx context.Context, req *courierv1.SendNotificationRequest) (*courierv1.SendNotificationResponse, error) {
	resp, err := s.sendNotification(ctx, req)
	metrics.Accepted(resultOf(err))
	return resp, err
}

func (s *Server) sendNotification(ctx context.Context, req *courierv1.SendNotificationRequest) (*courierv1.SendNotificationResponse, error) {
	locale := render.NormalizeLocale(req.GetLocale())
	if locale == "" {
		return nil, apperror.InvalidArgument("locale is required").WithReason("locale_required")
	}
	templateID := strings.TrimSpace(req.GetTemplateId())
	if templateID == "" {
		return nil, apperror.InvalidArgument("template_id is required").WithReason("template_id_required")
	}
	if len(req.GetRecipients()) == 0 {
		return nil, apperror.InvalidArgument("recipients is required").WithReason("recipients_required")
	}

	dels, err := s.rawDeliveries(req.GetRecipients(), locale, templateID)
	if err != nil {
		return nil, err
	}

	rc := requestctx.From(ctx)
	eventID, created, err := s.store.CreateEvent(ctx, store.NewEvent{
		IdempotencyKey: strings.TrimSpace(req.GetIdempotencyKey()),
		EventType:      templateID,
		Locale:         locale,
		Data:           req.GetTemplateData(),
		CorrelationID:  rc.CorrelationID,
		RequestID:      rc.RequestID,
		UserID:         rc.UserID,
		TenantID:       rc.TenantID,
		AgencyID:       rc.AgencyID,
	}, dels)
	if err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	if len(created) == 0 {
		// same idempotency key: nothing new was queued
		return &courierv1.SendNotificationResponse{NotificationId: eventID, Queued: 0}, nil
	}

	s.push(ctx, created)
	return &courierv1.SendNotificationResponse{NotificationId: eventID, Queued: int32(len(created))}, nil
}

// push is the fast path: the deliveries are committed, so a Redis failure is not an error for the caller;
// the relay of the worker finds the rows that were not pushed.
func (s *Server) push(ctx context.Context, dels []store.Delivery) {
	log := logging.FromCtx(ctx)
	var pushed []string
	for _, d := range dels {
		if !d.NextAttemptAt.IsZero() && d.NextAttemptAt.After(time.Now()) {
			continue // delayed (quiet hours): the relay pushes it when it is due
		}
		if err := s.queue.Enqueue(ctx, d.ID); err != nil {
			log.Warn("enqueue failed, the relay will retry", zap.String("delivery_id", d.ID), zap.Error(err))
			continue
		}
		pushed = append(pushed, d.ID)
	}
	if err := s.store.MarkEnqueued(ctx, pushed...); err != nil {
		log.Warn("mark enqueued failed", zap.Error(err))
	}
}

// rawDeliveries validates raw recipients (channel supported, address set, a template for that channel) and
// turns them into deliveries.
func (s *Server) rawDeliveries(recs []*courierv1.Recipient, locale, templateID string) ([]store.NewDelivery, error) {
	dels := make([]store.NewDelivery, 0, len(recs))
	for _, rec := range recs {
		if rec.GetChannel() == courierv1.Channel_CHANNEL_UNSPECIFIED {
			return nil, apperror.InvalidArgument("recipients.channel is required").WithReason("recipient_channel_required")
		}
		slug, ok := notify.Slug(rec.GetChannel())
		if !ok {
			return nil, apperror.InvalidArgument("recipients.channel is not supported").WithReason("recipient_channel_unsupported")
		}
		address := strings.TrimSpace(rec.GetAddress())
		if address == "" {
			return nil, apperror.InvalidArgument("recipients.address is required").WithReason("recipient_address_required")
		}
		if !s.renderer.Has(locale, templateID, rec.GetChannel()) {
			return nil, apperror.InvalidArgument("template not found for recipient channel").WithReason("template_not_found")
		}
		dels = append(dels, store.NewDelivery{Channel: slug, Address: address})
	}
	return dels, nil
}

func (s *Server) GetNotification(ctx context.Context, req *courierv1.GetNotificationRequest) (*courierv1.GetNotificationResponse, error) {
	id := strings.TrimSpace(req.GetNotificationId())
	if id == "" {
		return nil, apperror.InvalidArgument("notification_id is required").WithReason("notification_id_required")
	}
	ev, dels, err := s.store.GetEvent(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, apperror.NotFound("notification not found").WithReason("notification_not_found")
	}
	if err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	out := &courierv1.GetNotificationResponse{
		NotificationId: ev.ID,
		TemplateId:     ev.EventType,
		CreatedAt:      ev.CreatedAt.UTC().Format(time.RFC3339),
	}
	for _, d := range dels {
		out.Deliveries = append(out.Deliveries, toProto(d))
	}
	return out, nil
}

func (s *Server) RetryDelivery(ctx context.Context, req *courierv1.RetryDeliveryRequest) (*courierv1.RetryDeliveryResponse, error) {
	id := strings.TrimSpace(req.GetDeliveryId())
	if id == "" {
		return nil, apperror.InvalidArgument("delivery_id is required").WithReason("delivery_id_required")
	}
	d, err := s.store.Retry(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, apperror.NotFound("delivery not found").WithReason("delivery_not_found")
	case errors.Is(err, store.ErrNotRetryable):
		return nil, apperror.FailedPrecondition("only failed or dead deliveries can be retried").WithReason("delivery_not_retryable")
	case err != nil:
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	s.push(ctx, []store.Delivery{d})
	return &courierv1.RetryDeliveryResponse{Delivery: toProto(d)}, nil
}

func toProto(d store.Delivery) *courierv1.Delivery {
	out := &courierv1.Delivery{
		Id:        d.ID,
		Channel:   channelOf(d.Channel),
		Address:   d.Address,
		Status:    statusOf(d.Status),
		Attempts:  int32(d.Attempts),
		LastError: d.LastError,
	}
	if d.Status == store.StatusPending && !d.NextAttemptAt.IsZero() {
		out.NextAttemptAt = d.NextAttemptAt.UTC().Format(time.RFC3339)
	}
	if d.SentAt != nil {
		out.SentAt = d.SentAt.UTC().Format(time.RFC3339)
	}
	return out
}

func channelOf(slug string) courierv1.Channel {
	switch slug {
	case "email":
		return courierv1.Channel_CHANNEL_EMAIL
	case "telegram":
		return courierv1.Channel_CHANNEL_TELEGRAM
	case "webhook":
		return courierv1.Channel_CHANNEL_WEBHOOK
	case "push":
		return courierv1.Channel_CHANNEL_PUSH
	}
	return courierv1.Channel_CHANNEL_UNSPECIFIED
}

func statusOf(s string) courierv1.DeliveryStatus {
	switch s {
	case store.StatusPending:
		return courierv1.DeliveryStatus_DELIVERY_STATUS_PENDING
	case store.StatusSent:
		return courierv1.DeliveryStatus_DELIVERY_STATUS_SENT
	case store.StatusFailed:
		return courierv1.DeliveryStatus_DELIVERY_STATUS_FAILED
	case store.StatusDead:
		return courierv1.DeliveryStatus_DELIVERY_STATUS_DEAD
	case store.StatusSkipped:
		return courierv1.DeliveryStatus_DELIVERY_STATUS_SKIPPED
	}
	return courierv1.DeliveryStatus_DELIVERY_STATUS_UNSPECIFIED
}
