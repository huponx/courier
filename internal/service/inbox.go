package service

import (
	"context"
	"errors"
	"time"

	"github.com/huponx/courier/internal/queue"
	"github.com/huponx/courier/internal/store"
	"github.com/huponx/gocommon/apperror"
	"github.com/huponx/gocommon/authn"
	"github.com/huponx/gocommon/grpcserver"
	"github.com/huponx/gocommon/logging"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
	"go.uber.org/zap"
)

// Permissions is the method map of the server: the rules of courier.proto and inbox.proto, deny by default.
var Permissions = grpcserver.PermissionsFromProto(
	courierv1.File_courier_v1_courier_proto,
	courierv1.File_courier_v1_inbox_proto,
	courierv1.File_courier_v1_preferences_proto,
	courierv1.File_courier_v1_admin_proto,
	courierv1.File_courier_v1_permissions_proto,
)

// Inbox is the InboxService: the notifications of the signed-in user.
type Inbox struct {
	courierv1.UnimplementedInboxServiceServer
	store *store.Store
	queue *queue.Client
}

func NewInbox(st *store.Store, q *queue.Client) *Inbox { return &Inbox{store: st, queue: q} }

// me is the user of the token (authn.RequireUser): the rule of an inbox method is `public` (any signed-in caller), which
// also lets a service through; a service has no inbox, and a user can never name another user.
func me(ctx context.Context) (string, error) { return authn.RequireUser(ctx) }

func (s *Inbox) ListInbox(ctx context.Context, req *courierv1.ListInboxRequest) (*courierv1.ListInboxResponse, error) {
	uid, err := me(ctx)
	if err != nil {
		return nil, err
	}
	items, next, err := s.store.ListInbox(ctx, uid, int(req.GetPageSize()), req.GetPageToken(), req.GetUnreadOnly())
	if errors.Is(err, store.ErrBadCursor) {
		return nil, apperror.InvalidArgument("page_token is not valid").WithReason("page_token_invalid")
	}
	if err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	unread, err := s.store.Unread(ctx, uid)
	if err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	out := &courierv1.ListInboxResponse{NextPageToken: next, Unread: int32(unread)}
	for _, it := range items {
		out.Items = append(out.Items, &courierv1.InboxItem{
			Id: it.ID, Title: it.Title, Body: it.Body, Link: it.Link, Severity: severityOf(it.Severity), Read: it.Read, CreatedAt: it.CreatedAt.Unix(),
		})
	}
	return out, nil
}

// heartbeat keeps proxies and the browser from closing an idle stream.
const heartbeat = 25 * time.Second

func (s *Inbox) StreamInbox(_ *courierv1.StreamInboxRequest, stream courierv1.InboxService_StreamInboxServer) error {
	ctx := stream.Context()
	uid, err := me(ctx)
	if err != nil {
		return err
	}
	// subscribe first, then read the count: a change between the two is seen, not lost
	events, err := s.queue.SubscribeInbox(ctx, uid)
	if err != nil {
		return apperror.Unavailable("realtime is unavailable").WithReason("stream_unavailable")
	}
	unread, err := s.store.Unread(ctx, uid)
	if err != nil {
		return apperror.Unavailable("store failed").WithReason("store_failed")
	}
	if err := stream.Send(&courierv1.StreamInboxResponse{Unread: int32(unread)}); err != nil {
		return err
	}
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := stream.Send(&courierv1.StreamInboxResponse{Heartbeat: true}); err != nil {
				return err
			}
		case ev, ok := <-events:
			if !ok {
				return apperror.Unavailable("realtime is unavailable").WithReason("stream_unavailable")
			}
			if err := stream.Send(toProtoEvent(ev)); err != nil {
				return err
			}
		}
	}
}

func toProtoEvent(ev queue.InboxEvent) *courierv1.StreamInboxResponse {
	out := &courierv1.StreamInboxResponse{Unread: int32(ev.Unread)}
	if it := ev.Item; it != nil {
		out.Item = &courierv1.InboxItem{Id: it.ID, Title: it.Title, Body: it.Body, Link: it.Link, Severity: severityOf(it.Severity), CreatedAt: it.CreatedAt}
	}
	return out
}

func (s *Inbox) GetUnreadCount(ctx context.Context, _ *courierv1.GetUnreadCountRequest) (*courierv1.GetUnreadCountResponse, error) {
	uid, err := me(ctx)
	if err != nil {
		return nil, err
	}
	n, err := s.store.Unread(ctx, uid)
	if err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	return &courierv1.GetUnreadCountResponse{Unread: int32(n)}, nil
}

func (s *Inbox) MarkRead(ctx context.Context, req *courierv1.MarkReadRequest) (*courierv1.MarkReadResponse, error) {
	uid, err := me(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.store.MarkRead(ctx, uid, req.GetIds(), req.GetAll()); err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	return &courierv1.MarkReadResponse{Unread: s.changed(ctx, uid)}, nil
}

func (s *Inbox) Archive(ctx context.Context, req *courierv1.ArchiveRequest) (*courierv1.ArchiveResponse, error) {
	uid, err := me(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.store.Archive(ctx, uid, req.GetIds()); err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	return &courierv1.ArchiveResponse{Unread: s.changed(ctx, uid)}, nil
}

// changed returns the new unread count and tells the other tabs of the user (an event without an item).
func (s *Inbox) changed(ctx context.Context, uid string) int32 {
	n, err := s.store.Unread(ctx, uid)
	if err != nil {
		return 0
	}
	if err := s.queue.PublishInbox(ctx, uid, queue.InboxEvent{Unread: n}); err != nil {
		logging.FromCtx(ctx).Warn("inbox publish failed", zap.Error(err))
	}
	return int32(n)
}

func severityOf(s string) courierv1.Severity {
	switch s {
	case "warning":
		return courierv1.Severity_SEVERITY_WARNING
	case "critical":
		return courierv1.Severity_SEVERITY_CRITICAL
	}
	return courierv1.Severity_SEVERITY_INFO
}
