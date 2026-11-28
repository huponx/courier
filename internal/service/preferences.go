package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/huponx/courier/internal/catalog"
	"github.com/huponx/courier/internal/notify/webhook"
	"github.com/huponx/courier/internal/store"
	"github.com/huponx/gocommon/apperror"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

const bindingTTL = 15 * time.Minute

// Preferences is the PreferencesService: what the signed-in user chose about their notifications.
type Preferences struct {
	courierv1.UnimplementedPreferencesServiceServer
	store   *store.Store
	catalog *catalog.Catalog
	// botUsername is the Telegram bot the link opens; empty: Telegram is not available.
	botUsername string
	// vapidPublic is the public VAPID key browsers subscribe with; empty: Web Push is not available.
	vapidPublic  string
	allowPrivate bool
}

// WithPush turns on Web Push: publicKey is the public VAPID key, allowPrivate lets an endpoint be http or private
// (a local run, WEBHOOK_ALLOW_PRIVATE).
func (s *Preferences) WithPush(publicKey string, allowPrivate bool) *Preferences {
	s.vapidPublic, s.allowPrivate = strings.TrimSpace(publicKey), allowPrivate
	return s
}

func NewPreferences(st *store.Store, cat *catalog.Catalog, botUsername string) *Preferences {
	return &Preferences{store: st, catalog: cat, botUsername: strings.TrimPrefix(strings.TrimSpace(botUsername), "@")}
}

func (s *Preferences) GetPreferences(ctx context.Context, _ *courierv1.GetPreferencesRequest) (*courierv1.GetPreferencesResponse, error) {
	uid, err := me(ctx)
	if err != nil {
		return nil, err
	}
	prefs, err := s.store.Prefs(ctx, []string{uid})
	if err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	quiet, err := s.store.Quiet(ctx, []string{uid})
	if err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	chats, err := s.store.Bindings(ctx, []string{uid}, "telegram")
	if err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}

	pushes, err := s.store.PushSubs(ctx, []string{uid})
	if err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	out := &courierv1.GetPreferencesResponse{
		QuietHours: &courierv1.QuietHours{},
		Telegram:   &courierv1.TelegramState{Available: s.botUsername != "", Linked: chats[uid] != ""},
		Push:       &courierv1.PushState{Available: s.vapidPublic != "", PublicKey: s.vapidPublic},
	}
	for _, p := range pushes[uid] {
		out.Push.Endpoints = append(out.Push.Endpoints, p.Endpoint)
	}
	for _, c := range s.catalog.Categories() {
		if c.Name != "" && !s.visible(c) {
			continue
		}
		p := &courierv1.CategoryPreference{Category: c.Name, Mandatory: c.Mandatory, Channels: c.Channels}
		if !c.Mandatory {
			for _, ch := range c.Channels {
				if on, set := prefs[uid][c.Name][ch]; set && !on {
					p.DisabledChannels = append(p.DisabledChannels, ch)
				}
			}
		}
		out.Categories = append(out.Categories, p)
	}
	if q, ok := quiet[uid]; ok {
		out.QuietHours = &courierv1.QuietHours{Enabled: true, From: clock(q.FromMin), To: clock(q.ToMin), Timezone: q.Timezone}
	}
	return out, nil
}

// visible: a category the user can act on. A category whose channels the user could never receive (Telegram without a
// bot) is not listed.
func (s *Preferences) visible(c catalog.Category) bool {
	for _, ch := range c.Channels {
		if ch != "telegram" || s.botUsername != "" {
			return true
		}
	}
	return false
}

func (s *Preferences) SetPreferences(ctx context.Context, req *courierv1.SetPreferencesRequest) (*courierv1.SetPreferencesResponse, error) {
	uid, err := me(ctx)
	if err != nil {
		return nil, err
	}
	known := map[string]catalog.Category{}
	for _, c := range s.catalog.Categories() {
		known[c.Name] = c
	}

	var prefs []store.Pref
	for _, in := range req.GetCategories() {
		c, ok := known[in.GetCategory()]
		if !ok {
			return nil, apperror.InvalidArgument("unknown category " + in.GetCategory()).WithReason("category_unknown")
		}
		if c.Mandatory {
			continue // stays on
		}
		off := map[string]bool{}
		for _, ch := range in.GetDisabledChannels() {
			if !contains(c.Channels, ch) {
				return nil, apperror.InvalidArgument(fmt.Sprintf("category %s has no channel %s", c.Name, ch)).WithReason("channel_unknown")
			}
			off[ch] = true
		}
		for _, ch := range c.Channels {
			prefs = append(prefs, store.Pref{Category: c.Name, Channel: ch, Enabled: !off[ch]})
		}
	}

	var quiet *store.QuietHours
	if q := req.GetQuietHours(); q != nil && q.GetEnabled() {
		from, err1 := parseClock(q.GetFrom())
		to, err2 := parseClock(q.GetTo())
		if err1 != nil || err2 != nil || from == to {
			return nil, apperror.InvalidArgument("quiet hours need two different times as HH:MM").WithReason("quiet_hours_invalid")
		}
		if _, err := time.LoadLocation(q.GetTimezone()); err != nil || q.GetTimezone() == "" || q.GetTimezone() == "Local" {
			return nil, apperror.InvalidArgument("quiet hours need an IANA time zone").WithReason("timezone_invalid")
		}
		quiet = &store.QuietHours{Timezone: q.GetTimezone(), FromMin: from, ToMin: to}
	}

	if err := s.store.SetPrefs(ctx, uid, prefs); err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	// the quiet hours are part of the request: absent or disabled removes them
	if err := s.store.SetQuiet(ctx, uid, quiet); err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	return &courierv1.SetPreferencesResponse{}, nil
}

func (s *Preferences) StartTelegramBinding(ctx context.Context, _ *courierv1.StartTelegramBindingRequest) (*courierv1.StartTelegramBindingResponse, error) {
	uid, err := me(ctx)
	if err != nil {
		return nil, err
	}
	if s.botUsername == "" {
		return nil, apperror.FailedPrecondition("Telegram is not available").WithReason("telegram_unavailable")
	}
	code, exp, err := s.store.NewBindingCode(ctx, uid, "telegram", bindingTTL)
	if err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	return &courierv1.StartTelegramBindingResponse{Code: code, Link: "https://t.me/" + s.botUsername + "?start=" + code, ExpiresAt: exp.Unix()}, nil
}

func (s *Preferences) UnbindTelegram(ctx context.Context, _ *courierv1.UnbindTelegramRequest) (*courierv1.UnbindTelegramResponse, error) {
	uid, err := me(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.store.Unbind(ctx, uid, "telegram"); err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	return &courierv1.UnbindTelegramResponse{}, nil
}

func clock(min int) string { return fmt.Sprintf("%02d:%02d", min/60, min%60) }

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

// maxEndpoint bounds what a browser sends; real endpoints are a few hundred characters.
const maxEndpoint = 2048

func (s *Preferences) SubscribePush(ctx context.Context, req *courierv1.SubscribePushRequest) (*courierv1.SubscribePushResponse, error) {
	uid, err := me(ctx)
	if err != nil {
		return nil, err
	}
	if s.vapidPublic == "" {
		return nil, apperror.FailedPrecondition("Web Push is not available").WithReason("push_unavailable")
	}
	endpoint := strings.TrimSpace(req.GetEndpoint())
	if len(endpoint) > maxEndpoint || webhook.ValidateURL(endpoint, s.allowPrivate) != nil {
		return nil, apperror.InvalidArgument("the endpoint must be an https URL").WithReason("push_endpoint_invalid")
	}
	if !validKey(req.GetP256Dh(), 65) || !validKey(req.GetAuth(), 16) {
		return nil, apperror.InvalidArgument("the subscription keys are not valid").WithReason("push_keys_invalid")
	}
	ua := req.GetUserAgent()
	if len(ua) > 300 {
		ua = ua[:300]
	}
	if err := s.store.SavePush(ctx, store.PushSub{UserID: uid, Endpoint: endpoint, P256dh: req.GetP256Dh(), Auth: req.GetAuth(), UserAgent: ua}); err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	return &courierv1.SubscribePushResponse{}, nil
}

// validKey checks a base64url key of the given length in bytes.
func validKey(s string, size int) bool {
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
	return err == nil && len(b) == size
}

func (s *Preferences) UnsubscribePush(ctx context.Context, req *courierv1.UnsubscribePushRequest) (*courierv1.UnsubscribePushResponse, error) {
	uid, err := me(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.store.DeletePush(ctx, uid, strings.TrimSpace(req.GetEndpoint())); err != nil {
		return nil, apperror.Unavailable("store failed").WithReason("store_failed")
	}
	return &courierv1.UnsubscribePushResponse{}, nil
}
