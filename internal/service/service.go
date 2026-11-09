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
		return nil, apperror.InvalidArgument("locale is required")
	}
	templateID := strings.TrimSpace(req.GetTemplateId())
	if templateID == "" {
		return nil, apperror.InvalidArgument("template_id is required")
	}
	if len(req.GetRecipients()) == 0 {
		return nil, apperror.InvalidArgument("recipients is required")
	}

	for _, rec := range req.GetRecipients() {
		if rec.GetChannel() == courierv1.Channel_CHANNEL_UNSPECIFIED {
			return nil, apperror.InvalidArgument("recipients.channel is required")
		}
		if _, ok := notify.Slug(rec.GetChannel()); !ok {
			return nil, apperror.InvalidArgument("recipients.channel is not supported")
		}
		if strings.TrimSpace(rec.GetAddress()) == "" {
			return nil, apperror.InvalidArgument("recipients.address is required")
		}
		if !s.renderer.Has(locale, templateID, rec.GetChannel()) {
			return nil, apperror.InvalidArgument("template not found for recipient channel")
		}
	}

	notificationID := uuid.NewString()
	idempKey := strings.TrimSpace(req.GetIdempotencyKey())
	if idempKey != "" {
		existing, claimed, err := s.queue.ClaimIdempotency(ctx, idempKey, notificationID)
		if err != nil {
			return nil, apperror.Unavailable("idempotency store failed")
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
			return nil, apperror.Unavailable("enqueue failed")
		}
		queued++
	}

	return &courierv1.SendNotificationResponse{
		NotificationId: notificationID,
		Queued:         queued,
	}, nil
}
