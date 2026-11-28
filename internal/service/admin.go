package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/huponx/courier/internal/catalog"
	"github.com/huponx/courier/internal/notify/render"
	"github.com/huponx/courier/internal/notify/webhook"
	"github.com/huponx/courier/internal/store"
	"github.com/huponx/gocommon/apperror"
	"github.com/huponx/gocommon/authn"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// UserLister finds the audience of an announcement: the active users, optionally those holding a role.
type UserLister interface {
	ListUserIDs(ctx context.Context, role string, fn func(ids []string) error) error
}

// maxBroadcast bounds one announcement: it is sent while the request waits.
const maxBroadcast = 20000

// Admin is the AdminNotificationService.
type Admin struct {
	courierv1.UnimplementedAdminNotificationServiceServer
	store    *store.Store
	catalog  *catalog.Catalog
	renderer *render.Renderer
	notifier *Server
	users    UserLister
	// reload puts the edited templates in the renderer of this process right away (the others follow within their interval).
	reload func(ctx context.Context) error
	// hooks tests a webhook; nil until WithWebhooks.
	hooks        *webhook.Sender
	allowPrivate bool
}

func NewAdmin(st *store.Store, cat *catalog.Catalog, r *render.Renderer, notifier *Server, users UserLister, reload func(context.Context) error) *Admin {
	return &Admin{store: st, catalog: cat, renderer: r, notifier: notifier, users: users, reload: reload}
}

// needPrincipal refuses a caller that has no token principal. A ServiceAccount caller (identity, bvchain) is trusted for
// the service API by its allow list, but must not be able to edit templates, send announcements or configure webhooks:
// those are the screens of the administrators, who hold a token and the permission of the method.
func needPrincipal(ctx context.Context) error {
	_, err := authn.RequirePrincipal(ctx)
	return err
}

func actor(ctx context.Context) string {
	if p := authn.FromContext(ctx); p != nil {
		return p.Subject
	}
	return ""
}

func storeFailed() error { return apperror.Unavailable("store failed").WithReason("store_failed") }

// template checks that the template belongs to an event type, the locale has templates and the channel is one of the
// channels of that type, and returns the type (its sample data is the data of a preview).
func (s *Admin) template(template, locale, channel string) (catalog.EventType, string, error) {
	locale = render.NormalizeLocale(locale)
	var found *catalog.EventType
	for _, et := range s.catalog.Types() {
		if et.Template == template || (et.Digest != nil && et.Template+"_digest" == template) {
			et := et
			found = &et
			break
		}
	}
	if found == nil {
		return catalog.EventType{}, "", apperror.NotFound("unknown template " + template).WithReason("template_unknown")
	}
	known := false
	for _, l := range s.renderer.Locales() {
		known = known || l == locale
	}
	if !known {
		return catalog.EventType{}, "", apperror.InvalidArgument("unknown locale " + locale).WithReason("locale_unknown")
	}
	if _, ok := render.ChannelOf(channel); !ok || !contains(found.Channels, channel) {
		return catalog.EventType{}, "", apperror.InvalidArgument(fmt.Sprintf("%s has no %s channel", found.Type, channel)).WithReason("channel_unknown")
	}
	return *found, locale, nil
}

func (s *Admin) ListEventTypes(ctx context.Context, _ *courierv1.ListEventTypesRequest) (*courierv1.ListEventTypesResponse, error) {
	if err := needPrincipal(ctx); err != nil {
		return nil, err
	}
	rows, err := s.store.Templates(ctx)
	if err != nil {
		return nil, storeFailed()
	}
	version := map[[3]string]int{}
	for _, r := range rows {
		version[[3]string{r.Template, r.Locale, r.Channel}] = r.Version
	}
	locales := s.renderer.Locales()
	out := &courierv1.ListEventTypesResponse{Locales: locales}
	for _, et := range s.catalog.Types() {
		info := &courierv1.EventTypeInfo{
			Type: et.Type, Category: et.Category, Template: et.Template, Channels: et.Channels, Mandatory: et.Mandatory,
			Severity: et.Severity, Required: et.Required, Optional: et.Optional, Broadcast: et.Broadcast,
		}
		for _, l := range locales {
			for _, ch := range et.Channels {
				if ch == "push" {
					continue // a push notification shows the in-app title and body: one template for both
				}
				v := version[[3]string{et.Template, l, ch}]
				info.Templates = append(info.Templates, &courierv1.TemplateInfo{Locale: l, Channel: ch, Customized: v > 0, Version: int32(v)})
				if et.Digest != nil {
					dv := version[[3]string{et.Template + "_digest", l, ch}]
					info.Templates = append(info.Templates, &courierv1.TemplateInfo{Locale: l, Channel: ch, Customized: dv > 0, Version: int32(dv), Template: et.Template + "_digest"})
				}
			}
		}
		if et.Digest != nil {
			info.DigestWindow = et.Digest.WindowText
		}
		out.EventTypes = append(out.EventTypes, info)
	}
	return out, nil
}

func toContent(c render.Content) *courierv1.TemplateContent {
	return &courierv1.TemplateContent{Subject: c.Subject, Text: c.Text, Html: c.HTML}
}

func fromContent(c *courierv1.TemplateContent) render.Content {
	return render.Content{Subject: c.GetSubject(), Text: c.GetText(), HTML: c.GetHtml()}
}

func (s *Admin) GetTemplate(ctx context.Context, req *courierv1.GetTemplateRequest) (*courierv1.GetTemplateResponse, error) {
	if err := needPrincipal(ctx); err != nil {
		return nil, err
	}
	_, locale, err := s.template(req.GetTemplate(), req.GetLocale(), req.GetChannel())
	if err != nil {
		return nil, err
	}
	row, err := s.store.Template(ctx, req.GetTemplate(), locale, req.GetChannel())
	switch {
	case err == nil:
		return &courierv1.GetTemplateResponse{
			Content: toContent(render.ContentOf(req.GetChannel(), row.Files)), Customized: true, Version: int32(row.Version),
			UpdatedBy: row.UpdatedBy, UpdatedAt: row.UpdatedAt.Unix(),
		}, nil
	case errors.Is(err, store.ErrNotFound):
		return &courierv1.GetTemplateResponse{Content: toContent(render.ContentOf(req.GetChannel(), s.renderer.Source(locale, req.GetTemplate(), req.GetChannel())))}, nil
	}
	return nil, storeFailed()
}

// draft renders the content with the sample data (and the given values over it): the check of a save and the preview.
func (s *Admin) draft(et catalog.EventType, template, locale, channel string, content render.Content, data map[string]string) (render.Content, map[string]string, error) {
	files, err := render.Files(channel, content)
	if err != nil {
		return render.Content{}, nil, apperror.InvalidArgument(err.Error()).WithReason("template_invalid")
	}
	values := et.SampleData()
	if strings.HasSuffix(template, "_digest") {
		values["count"], values["items"] = "3", "sample line 1\nsample line 2\nsample line 3"
	}
	for k, v := range data {
		values[k] = v
	}
	rendered, err := s.renderer.Draft(locale, template, channel, files, values)
	if err != nil {
		return render.Content{}, nil, apperror.InvalidArgument("the template does not render: " + err.Error()).WithReason("template_invalid")
	}
	return rendered, files, nil
}

func (s *Admin) PreviewTemplate(ctx context.Context, req *courierv1.PreviewTemplateRequest) (*courierv1.PreviewTemplateResponse, error) {
	if err := needPrincipal(ctx); err != nil {
		return nil, err
	}
	et, locale, err := s.template(req.GetTemplate(), req.GetLocale(), req.GetChannel())
	if err != nil {
		return nil, err
	}
	rendered, _, err := s.draft(et, req.GetTemplate(), locale, req.GetChannel(), fromContent(req.GetContent()), req.GetData())
	if err != nil {
		return nil, err
	}
	return &courierv1.PreviewTemplateResponse{Rendered: toContent(rendered)}, nil
}

func (s *Admin) SaveTemplate(ctx context.Context, req *courierv1.SaveTemplateRequest) (*courierv1.SaveTemplateResponse, error) {
	if err := needPrincipal(ctx); err != nil {
		return nil, err
	}
	et, locale, err := s.template(req.GetTemplate(), req.GetLocale(), req.GetChannel())
	if err != nil {
		return nil, err
	}
	_, files, err := s.draft(et, req.GetTemplate(), locale, req.GetChannel(), fromContent(req.GetContent()), nil)
	if err != nil {
		return nil, err
	}
	v, err := s.store.SaveTemplate(ctx, req.GetTemplate(), locale, req.GetChannel(), files, actor(ctx))
	if err != nil {
		return nil, storeFailed()
	}
	if err := s.reload(ctx); err != nil {
		return nil, apperror.Unavailable("saved, but this process could not reload the templates; it will within half a minute").WithReason("reload_failed")
	}
	return &courierv1.SaveTemplateResponse{Version: int32(v)}, nil
}

func (s *Admin) RevertTemplate(ctx context.Context, req *courierv1.RevertTemplateRequest) (*courierv1.RevertTemplateResponse, error) {
	if err := needPrincipal(ctx); err != nil {
		return nil, err
	}
	_, locale, err := s.template(req.GetTemplate(), req.GetLocale(), req.GetChannel())
	if err != nil {
		return nil, err
	}
	err = s.store.RevertTemplate(ctx, req.GetTemplate(), locale, req.GetChannel(), actor(ctx))
	if errors.Is(err, store.ErrNotFound) {
		return nil, apperror.NotFound("the template is not customized").WithReason("template_not_customized")
	}
	if err != nil {
		return nil, storeFailed()
	}
	_ = s.reload(ctx) // the other processes reload by their interval; a failure here heals the same way
	return &courierv1.RevertTemplateResponse{}, nil
}

func (s *Admin) ListTemplateVersions(ctx context.Context, req *courierv1.ListTemplateVersionsRequest) (*courierv1.ListTemplateVersionsResponse, error) {
	if err := needPrincipal(ctx); err != nil {
		return nil, err
	}
	_, locale, err := s.template(req.GetTemplate(), req.GetLocale(), req.GetChannel())
	if err != nil {
		return nil, err
	}
	vs, err := s.store.TemplateVersions(ctx, req.GetTemplate(), locale, req.GetChannel())
	if err != nil {
		return nil, storeFailed()
	}
	out := &courierv1.ListTemplateVersionsResponse{}
	for _, v := range vs {
		tv := &courierv1.TemplateVersion{Version: int32(v.Version), UpdatedBy: v.UpdatedBy, UpdatedAt: v.UpdatedAt.Unix(), Reverted: v.Files == nil}
		if v.Files != nil {
			tv.Content = toContent(render.ContentOf(req.GetChannel(), v.Files))
		}
		out.Versions = append(out.Versions, tv)
	}
	return out, nil
}

func (s *Admin) CreateBroadcast(ctx context.Context, req *courierv1.CreateBroadcastRequest) (*courierv1.CreateBroadcastResponse, error) {
	if err := needPrincipal(ctx); err != nil {
		return nil, err
	}
	et, ok := s.catalog.Get(req.GetEventType())
	if !ok || !et.Broadcast {
		return nil, apperror.InvalidArgument("event_type is not an announcement type").WithReason("event_type_not_broadcast")
	}
	a := req.GetAudience()
	choices := 0
	if a.GetAll() {
		choices++
	}
	if a.GetRole() != "" {
		choices++
	}
	if len(a.GetUserIds()) > 0 {
		choices++
	}
	if choices != 1 {
		return nil, apperror.InvalidArgument("audience is exactly one of all, role and user_ids").WithReason("audience_invalid")
	}

	// the channels of the announcement: a subset of those of the type (Notify refuses another one; here the admin gets the
	// error before the audience is looked up)
	channels := make([]string, 0, len(req.GetChannels()))
	for _, ch := range req.GetChannels() {
		if !contains(et.Channels, ch) {
			return nil, apperror.InvalidArgument(fmt.Sprintf("%s has no %s channel", et.Type, ch)).WithReason("channel_unknown")
		}
		if !contains(channels, ch) {
			channels = append(channels, ch)
		}
	}

	var ids []string
	if len(a.GetUserIds()) > 0 {
		// each page is a separate Notify (which removes duplicates inside the page only): remove them here
		seen := map[string]bool{}
		for _, id := range a.GetUserIds() {
			if id = strings.TrimSpace(id); id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		if len(ids) > maxBroadcast {
			return nil, apperror.InvalidArgument(fmt.Sprintf("an announcement is limited to %d users", maxBroadcast)).WithReason("audience_too_large")
		}
	} else {
		if s.users == nil {
			return nil, apperror.FailedPrecondition("audiences need IDENTITY_TARGET").WithReason("user_resolution_disabled")
		}
		err := s.users.ListUserIDs(ctx, a.GetRole(), func(page []string) error {
			if len(ids)+len(page) > maxBroadcast {
				return apperror.InvalidArgument(fmt.Sprintf("an announcement is limited to %d users", maxBroadcast)).WithReason("audience_too_large")
			}
			ids = append(ids, page...)
			return nil
		})
		var ae *apperror.Error
		if errors.As(err, &ae) {
			return nil, err
		}
		if err != nil {
			return nil, apperror.Unavailable("identity lookup failed").WithReason("identity_unavailable")
		}
	}
	if len(ids) == 0 {
		return nil, apperror.InvalidArgument("the audience has no users").WithReason("audience_empty")
	}

	id := newID()
	skipped := 0
	const page = 200
	for i := 0; i*page < len(ids); i++ {
		end := min((i+1)*page, len(ids))
		callCtx := ctx
		if i > 0 {
			callCtx = withoutWebhooks(ctx) // the webhooks hear of the announcement once
		}
		resp, err := s.notifier.Notify(callCtx, &courierv1.NotifyRequest{
			IdempotencyKey: fmt.Sprintf("broadcast-%s-%d", id, i),
			EventType:      et.Type, UserIds: ids[i*page : end], Data: req.GetData(), Channels: channels,
		})
		if err != nil {
			if i == 0 {
				return nil, err // nothing was sent: the request can simply be repeated
			}
			// the pages before this one are sent and cannot be recalled: keep the record, and say how far it got so that
			// nobody sends the announcement again to the same people
			sent := i * page
			audience, _ := protojson.Marshal(a)
			data, _ := stringData(req.GetData())
			_, _ = s.store.InsertBroadcast(ctx, store.Broadcast{ID: id, EventType: et.Type, Audience: audience, Data: data, CreatedBy: actor(ctx), Recipients: sent, Skipped: skipped, Channels: channels})
			return nil, apperror.Unavailable(fmt.Sprintf("the announcement was sent to the first %d of %d users and stopped: %s", sent, len(ids), err.Error())).WithReason("broadcast_partial")
		}
		skipped += len(resp.GetSkippedUserIds())
	}

	audience, _ := protojson.Marshal(a)
	data, _ := stringData(req.GetData())
	b, err := s.store.InsertBroadcast(ctx, store.Broadcast{ID: id, EventType: et.Type, Audience: audience, Data: data, CreatedBy: actor(ctx), Recipients: len(ids), Skipped: skipped, Channels: channels})
	if err != nil {
		return nil, storeFailed()
	}
	return &courierv1.CreateBroadcastResponse{Broadcast: toBroadcast(b)}, nil
}

func toBroadcast(b store.Broadcast) *courierv1.Broadcast {
	a := &courierv1.Audience{}
	_ = protojson.Unmarshal(b.Audience, a)
	return &courierv1.Broadcast{Id: b.ID, EventType: b.EventType, Audience: a, CreatedBy: b.CreatedBy, CreatedAt: b.CreatedAt.Unix(), Recipients: int32(b.Recipients), Skipped: int32(b.Skipped), Channels: b.Channels}
}

func (s *Admin) ListBroadcasts(ctx context.Context, req *courierv1.ListBroadcastsRequest) (*courierv1.ListBroadcastsResponse, error) {
	if err := needPrincipal(ctx); err != nil {
		return nil, err
	}
	bs, next, err := s.store.ListBroadcasts(ctx, int(req.GetPageSize()), req.GetPageToken())
	if errors.Is(err, store.ErrBadCursor) {
		return nil, apperror.InvalidArgument("page_token is not valid").WithReason("page_token_invalid")
	}
	if err != nil {
		return nil, storeFailed()
	}
	out := &courierv1.ListBroadcastsResponse{NextPageToken: next}
	for _, b := range bs {
		out.Broadcasts = append(out.Broadcasts, toBroadcast(b))
	}
	return out, nil
}

func (s *Admin) ListDeliveries(ctx context.Context, req *courierv1.ListDeliveriesRequest) (*courierv1.ListDeliveriesResponse, error) {
	if err := needPrincipal(ctx); err != nil {
		return nil, err
	}
	f := store.DeliveryFilter{
		Channel: strings.TrimSpace(req.GetChannel()), NotificationID: strings.TrimSpace(req.GetNotificationId()),
		UserID: strings.TrimSpace(req.GetUserId()), Address: strings.TrimSpace(req.GetAddress()),
	}
	switch req.GetStatus() {
	case courierv1.DeliveryStatus_DELIVERY_STATUS_UNSPECIFIED:
	case courierv1.DeliveryStatus_DELIVERY_STATUS_PENDING:
		f.Status = store.StatusPending
	case courierv1.DeliveryStatus_DELIVERY_STATUS_SENT:
		f.Status = store.StatusSent
	case courierv1.DeliveryStatus_DELIVERY_STATUS_FAILED:
		f.Status = store.StatusFailed
	case courierv1.DeliveryStatus_DELIVERY_STATUS_DEAD:
		f.Status = store.StatusDead
	case courierv1.DeliveryStatus_DELIVERY_STATUS_SKIPPED:
		f.Status = store.StatusSkipped
	}
	recs, next, err := s.store.ListDeliveries(ctx, f, int(req.GetPageSize()), req.GetPageToken())
	if errors.Is(err, store.ErrBadCursor) {
		return nil, apperror.InvalidArgument("page_token is not valid").WithReason("page_token_invalid")
	}
	if err != nil {
		return nil, storeFailed()
	}
	out := &courierv1.ListDeliveriesResponse{NextPageToken: next}
	for _, r := range recs {
		out.Deliveries = append(out.Deliveries, &courierv1.DeliveryRecord{
			Delivery: toProto(r.Delivery), NotificationId: r.NotificationID, EventType: r.EventType, UserId: r.Delivery.UserID, CreatedAt: r.CreatedAt.Unix(), Merged: int32(r.Merged),
		})
	}
	return out, nil
}
