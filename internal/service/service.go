package service

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/huponx/courier/internal/notify"
	"github.com/huponx/courier/internal/notify/render"
	"github.com/huponx/courier/internal/queue"
	"github.com/huponx/gocommon/apperror"
	"github.com/huponx/gocommon/requestctx"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

type Server struct {
	courierv1.UnimplementedCourierServiceServer
	queue    *queue.Client
	renderer *render.Renderer
}

func New(q *queue.Client, renderer *render.Renderer) *Server {
	return &Server{queue: q, renderer: renderer}
}

func (s *Server) SendNotification(ctx context.Context, req *courierv1.SendNotificationRequest) (*courierv1.SendNotificationResponse, error) {
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

	for _, rec := range req.GetRecipients() {
		if rec.GetChannel() == courierv1.Channel_CHANNEL_UNSPECIFIED {
			return nil, apperror.InvalidArgument("recipients.channel is required").WithReason("recipient_channel_required")
		}
		if _, ok := notify.Slug(rec.GetChannel()); !ok {
			return nil, apperror.InvalidArgument("recipients.channel is not supported").WithReason("recipient_channel_unsupported")
		}
		if strings.TrimSpace(rec.GetAddress()) == "" {
			return nil, apperror.InvalidArgument("recipients.address is required").WithReason("recipient_address_required")
		}
		if !s.renderer.Has(locale, templateID, rec.GetChannel()) {
			return nil, apperror.InvalidArgument("template not found for recipient channel").WithReason("template_not_found")
		}
	}

	notificationID := uuid.NewString()
	idempKey := strings.TrimSpace(req.GetIdempotencyKey())
	if idempKey != "" {
		existing, claimed, err := s.queue.ClaimIdempotency(ctx, idempKey, notificationID)
		if err != nil {
			return nil, apperror.Unavailable("idempotency store failed").WithReason("idempotency_store_failed")
		}
		if !claimed {
			return &courierv1.SendNotificationResponse{NotificationId: existing, Queued: 0}, nil
		}
		notificationID = existing
	}

	rc := requestctx.From(ctx)
	var queued int32
	for _, rec := range req.GetRecipients() {
		job := queue.Job{
			NotificationID: notificationID,
			Channel:        rec.GetChannel(),
			Address:        strings.TrimSpace(rec.GetAddress()),
			Locale:         locale,
			TemplateID:     templateID,
			TemplateData:   req.GetTemplateData(),
			CorrelationID:  rc.CorrelationID,
			UserID:         rc.UserID,
			TenantID:       rc.TenantID,
			AgencyID:       rc.AgencyID,
			RequestID:      rc.RequestID,
		}
		if err := s.queue.Enqueue(ctx, job); err != nil {
			if idempKey != "" {
				_ = s.queue.DeleteIdempotency(ctx, idempKey)
			}
			return nil, apperror.Unavailable("enqueue failed").WithReason("enqueue_failed")
		}
		queued++
	}

	return &courierv1.SendNotificationResponse{
		NotificationId: notificationID,
		Queued:         queued,
	}, nil
}
