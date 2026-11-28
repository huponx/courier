package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/huponx/courier/internal/appconfig"
	"github.com/huponx/courier/internal/catalog"
	"github.com/huponx/courier/internal/metrics"
	"github.com/huponx/courier/internal/notify"
	"github.com/huponx/courier/internal/notify/email"
	"github.com/huponx/courier/internal/notify/push"
	"github.com/huponx/courier/internal/notify/render"
	"github.com/huponx/courier/internal/notify/telegram"
	"github.com/huponx/courier/internal/notify/webhook"
	"github.com/huponx/courier/internal/queue"
	"github.com/huponx/courier/internal/relay"
	"github.com/huponx/courier/internal/store"
	"github.com/huponx/courier/internal/tplsync"
	"github.com/huponx/gocommon/config"
	"github.com/huponx/gocommon/healthz"
	"github.com/huponx/gocommon/logging"
	gometrics "github.com/huponx/gocommon/metrics"
	"github.com/huponx/gocommon/requestctx"
	"github.com/huponx/gocommon/worker"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
	"go.uber.org/zap"
)

func main() {
	cfg := config.MustLoad[appconfig.Worker]()
	if cfg.MaxRetry < 1 {
		cfg.MaxRetry = 5
	}

	log, err := logging.New(logging.Config{
		Level:    cfg.Level,
		Encoding: cfg.Encoding,
		Service:  serviceName(cfg.Service),
		Env:      cfg.Env,
	})
	if err != nil {
		panic(err)
	}
	defer logging.Sync(log)

	ctx, stop := worker.Signals()
	defer stop()

	q, err := queue.New(ctx, cfg.Redis.URL())
	if err != nil {
		log.Fatal("redis", zap.Error(err))
	}
	defer q.Close()
	if err := q.EnsureGroup(ctx); err != nil {
		log.Fatal("redis group", zap.Error(err))
	}

	st, err := store.New(ctx, cfg.Postgres.URL())
	if err != nil {
		log.Fatal("postgres", zap.Error(err))
	}
	defer st.Close()

	cat, err := catalog.Default()
	if err != nil {
		log.Fatal("catalog", zap.Error(err))
	}
	renderer := render.New(cfg.DefaultLocale)
	go tplsync.Run(ctx, st, renderer, cfg.TemplateReload, log)
	renderer.SetDefaults(map[string]string{"brand": cfg.MailBrand, "logo_url": cfg.MailLogoURL})
	var mailer notify.Channel = email.New(email.Config{
		Host:     cfg.SMTPHost,
		Port:     cfg.SMTPPort,
		User:     cfg.SMTPUser,
		Password: cfg.SMTPPassword,
		From:     cfg.SMTPFrom,
	})
	if cfg.MailDir != "" {
		log.Info("mails are written to files, not sent", zap.String("dir", cfg.MailDir))
		mailer = email.NewFile(cfg.MailDir)
	}
	reg := notify.NewRegistry(mailer, telegram.New(cfg.TelegramToken), webhook.New(st, cfg.WebhookAllowPrivate))
	if pc := (push.Config{PublicKey: cfg.VAPIDPublicKey, PrivateKey: cfg.VAPIDPrivateKey, Subject: cfg.VAPIDSubject, AllowPrivate: cfg.WebhookAllowPrivate}); pc.Available() {
		reg.Register(push.New(pc, st))
	} else {
		log.Warn("VAPID_PUBLIC_KEY and VAPID_PRIVATE_KEY are not set: browser notifications (Web Push) are not sent")
	}

	defer gometrics.Serve(cfg.MetricsAddr, metrics.Registry(st, q), log)()

	run := worker.New(worker.WithLogger(log))
	h := worker.NewHealth(cfg.HealthAddr,
		healthz.Check{Name: "redis", Fn: q.Ping},
		healthz.Check{Name: "postgres", Fn: st.Ping},
	)
	go func() {
		if err := h.ListenAndServe(); err != nil {
			log.Error("health server", zap.Error(err))
		}
	}()

	consumer := consumerName()
	log.Info("worker started", zap.String("consumer", consumer), zap.String("health", cfg.HealthAddr))

	rl := &relay.Relay{
		Store: st, Queue: q, Log: log,
		Interval: cfg.RelayInterval, Stale: cfg.RelayStale,
		Retention:      time.Duration(cfg.RetentionDays) * 24 * time.Hour,
		InboxRetention: time.Duration(cfg.InboxRetentionDays) * 24 * time.Hour,
	}
	relayDone := make(chan struct{})
	go func() {
		defer close(relayDone)
		rl.Run(ctx)
	}()

	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		if cfg.TelegramToken != "" && cfg.TelegramPoll {
			pollTelegram(ctx, log, st, cfg.TelegramToken)
		}
	}()

	consume(ctx, log, run, q, st, cat, renderer, reg, consumer, cfg)
	<-relayDone
	<-pollDone

	h.SetReady(false)
	drainCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := run.Drain(drainCtx); err != nil {
		log.Warn("drain", zap.Error(err))
	}
	_ = h.Shutdown(drainCtx)
}

// regroup creates the consumer group again when Redis lost it (a flush, a failover to an empty replica): without it
// the worker would read nothing for ever, while the relay keeps pushing work into the stream.
func regroup(ctx context.Context, log *zap.Logger, q *queue.Client, err error) {
	if !queue.IsNoGroup(err) {
		return
	}
	if gerr := q.EnsureGroup(ctx); gerr != nil {
		log.Error("redis group", zap.Error(gerr))
		return
	}
	log.Warn("the Redis consumer group was missing and has been created again")
}

func consume(
	ctx context.Context,
	log *zap.Logger,
	run *worker.Runner,
	q *queue.Client,
	st *store.Store,
	cat *catalog.Catalog,
	renderer *render.Renderer,
	reg *notify.Registry,
	consumer string,
	cfg appconfig.Worker,
) {
	for {
		if ctx.Err() != nil {
			return
		}

		claimed, err := q.AutoClaim(ctx, consumer, cfg.ClaimMinIdle, 16)
		if err != nil && ctx.Err() == nil {
			log.Error("autoclaim", zap.Error(err))
			regroup(ctx, log, q, err)
		}
		fresh, err := q.Read(ctx, consumer, 16, 2*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("xreadgroup", zap.Error(err))
			regroup(ctx, log, q, err)
			time.Sleep(time.Second)
			continue
		}

		for _, msg := range append(claimed, fresh...) {
			// the logger must be in the context: handle logs through logging.FromCtx (a nop logger otherwise)
			jobCtx := requestctx.WithValues(logging.WithCtx(context.Background(), log), requestctx.Values{
				CorrelationID: msg.Job.CorrelationID,
				UserID:        msg.Job.UserID,
				TenantID:      msg.Job.TenantID,
				AgencyID:      msg.Job.AgencyID,
				RequestID:     msg.Job.RequestID,
			})
			m := msg
			if !run.Go(jobCtx, func(c context.Context) error {
				return handle(c, q, st, cat, renderer, reg, m, cfg.MaxRetry)
			}) {
				return
			}
		}
	}
}

// telegramLeader is the advisory lock key of the process that listens to the bot.
const telegramLeader int64 = 0x636f7572696572 // "courier"

// storeBinder links a chat to the user of the code.
type storeBinder struct{ st *store.Store }

func (b storeBinder) Bind(ctx context.Context, code, chatID string) error {
	_, err := b.st.ConsumeBindingCode(ctx, "telegram", code, chatID)
	return err
}

// pollTelegram listens to the bot for `/start <code>` while this worker holds the lock (one poller per bot, whatever
// the number of workers); another worker takes over within seconds when this one stops.
func pollTelegram(ctx context.Context, log *zap.Logger, st *store.Store, token string) {
	pause := func(d time.Duration) {
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
	}
	for ctx.Err() == nil {
		release, ok, err := st.Leader(ctx, telegramLeader)
		if err != nil || !ok {
			if err != nil && ctx.Err() == nil {
				log.Warn("telegram leader", zap.Error(err))
			}
			pause(15 * time.Second)
			continue
		}
		log.Info("listening to the Telegram bot")
		p := &telegram.Poller{
			Token: token, Binder: storeBinder{st},
			IsInvalid: func(err error) bool { return errors.Is(err, store.ErrCodeInvalid) },
			Log:       func(msg string, err error) { log.Warn(msg, zap.Error(err)) },
		}
		err = p.Run(ctx)
		release()
		if err != nil {
			log.Warn("telegram poll stopped", zap.Error(err))
			pause(10 * time.Second)
		}
	}
}

// backoff is the wait before the next attempt after the n-th failure (n starts at 1).
func backoff(n int) time.Duration {
	steps := []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}
	if n < 1 {
		n = 1
	}
	if n > len(steps) {
		return steps[len(steps)-1]
	}
	return steps[n-1]
}

// sendTimeout bounds one send, so a hung server cannot hold a delivery past its lease and get it sent twice.
const sendTimeout = 90 * time.Second

// leaseFor bounds one send: a worker that dies holding the lease frees the delivery after it.
const leaseFor = 2 * time.Minute

func handle(
	ctx context.Context,
	q *queue.Client,
	st *store.Store,
	cat *catalog.Catalog,
	renderer *render.Renderer,
	reg *notify.Registry,
	msg queue.Message,
	maxRetry int,
) error {
	log := logging.FromCtx(ctx)
	job := msg.Job
	if job.DeliveryID == "" {
		id, err := importLegacy(ctx, st, job)
		if err != nil {
			log.Error("drop unreadable legacy job", zap.Error(err))
			return q.Ack(ctx, msg.ID)
		}
		job.DeliveryID = id
	}

	w, err := st.Claim(ctx, job.DeliveryID, leaseFor)
	if errors.Is(err, store.ErrNotClaimable) {
		// done, or another worker is on it, or a duplicate message
		return q.Ack(ctx, msg.ID)
	}
	if err != nil {
		return err // not acked: the message is claimed again
	}
	ctx = requestctx.WithValues(ctx, requestctx.Values{
		CorrelationID: w.Event.CorrelationID,
		UserID:        w.Event.UserID,
		TenantID:      w.Event.TenantID,
		AgencyID:      w.Event.AgencyID,
		RequestID:     w.Event.RequestID,
	})
	log = log.With(zap.String("notification_id", w.Event.ID), zap.String("delivery_id", w.Delivery.ID), zap.String("channel", w.Delivery.Channel))

	ch := channelOf(w.Delivery.Channel)
	var sendErr error
	var permanent bool
	var took time.Duration
	switch {
	case ch == courierv1.Channel_CHANNEL_UNSPECIFIED:
		sendErr, permanent = errors.New("unknown channel"), true
	case !reg.Has(ch):
		// not configured here (no VAPID keys) or an older worker during a rolling deploy: retried, not failed for good
		sendErr = fmt.Errorf("channel %s is not available on this worker", w.Delivery.Channel)
	default:
		locale := w.Event.Locale
		if w.Delivery.Locale != "" {
			locale = w.Delivery.Locale
		}
		items, err := st.DigestItems(ctx, w.Delivery.ID)
		if err != nil {
			return err // not acked: the message is claimed again
		}
		tpl, data := compose(renderer, cat, w, items, locale)
		var rendered notify.Message
		switch ch {
		case courierv1.Channel_CHANNEL_WEBHOOK:
			rendered = webhookMessage(renderer, w, locale, tpl, data, len(items) > 0)
		case courierv1.Channel_CHANNEL_PUSH:
			rendered, err = renderer.Render(locale, tpl, ch, data)
			rendered.Link = w.Event.Data["link"]
		default:
			rendered, err = renderer.Render(locale, tpl, ch, data)
		}
		if err != nil {
			sendErr, permanent = fmt.Errorf("render: %w", err), true
			break
		}
		began := time.Now()
		sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
		sendErr = reg.Send(sendCtx, ch, notify.Destination{Address: w.Delivery.Address}, rendered)
		cancel()
		permanent = notify.IsPermanent(sendErr)
		took = time.Since(began)
	}

	switch {
	case sendErr == nil:
		if err := st.MarkSent(ctx, w.Delivery.ID); err != nil {
			return err
		}
		metrics.Attempt(w.Delivery.Channel, metrics.OutcomeSent, took)
		log.Info("notification sent")
	case permanent:
		log.Error("send failed permanently", zap.Error(sendErr))
		metrics.Attempt(w.Delivery.Channel, metrics.OutcomeFailed, took)
		if err := st.MarkFailed(ctx, w.Delivery.ID, sendErr.Error()); err != nil {
			return err
		}
		if errors.Is(sendErr, notify.ErrRecipientGone) {
			// the chat blocked the bot, or the browser unsubscribed: forget the address, the user adds it again if they want
			var err error
			if w.Delivery.Channel == "push" {
				err = st.DeletePushByID(ctx, w.Delivery.Address)
			} else {
				err = st.UnbindAddress(ctx, w.Delivery.Channel, w.Delivery.Address)
			}
			if err != nil {
				log.Warn("unbind failed", zap.Error(err))
			}
		}
	default:
		status, err := st.MarkRetry(ctx, w.Delivery.ID, sendErr.Error(), time.Now().Add(backoff(w.Delivery.Attempts+1)), maxRetry)
		if err != nil {
			return err
		}
		if status == store.StatusDead {
			metrics.Attempt(w.Delivery.Channel, metrics.OutcomeDead, took)
			log.Error("send failed, out of attempts: dead", zap.Error(sendErr), zap.Int("attempts", w.Delivery.Attempts+1))
		} else {
			metrics.Attempt(w.Delivery.Channel, metrics.OutcomeRetry, took)
			log.Warn("send failed, will retry", zap.Error(sendErr), zap.Int("attempts", w.Delivery.Attempts+1))
		}
	}
	// the outcome is in the database: a retry comes back through the relay at next_attempt_at
	return q.Ack(ctx, msg.ID)
}

// webhookMessage is what a webhook gets: the title and body of the inbox template when there is one, else the event
// type and its values; with the event itself, for the endpoints that read the data. For a digest the data is the one
// of the digest (count, items).
func webhookMessage(r *render.Renderer, w store.Work, locale, template string, data map[string]string, digest bool) notify.Message {
	info := w.Event.Data
	if digest {
		info = data
	}
	msg := notify.Message{Event: &notify.EventInfo{ID: w.Event.ID, Type: w.Event.EventType, CreatedAt: w.Event.CreatedAt, Data: info}}
	if r.Has(locale, template, courierv1.Channel_CHANNEL_IN_APP) {
		if m, err := r.Render(locale, template, courierv1.Channel_CHANNEL_IN_APP, data); err == nil {
			msg.Subject, msg.Text = strings.TrimSpace(m.Subject), strings.TrimSpace(m.Text)
			return msg
		}
	}
	msg.Subject = w.Event.EventType
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+": "+data[k])
	}
	msg.Text = strings.Join(lines, "\n")
	return msg
}

// importLegacy moves a message of the pre-database format into the database.
func importLegacy(ctx context.Context, st *store.Store, job queue.Job) (string, error) {
	slug, ok := notify.Slug(job.Channel)
	if !ok {
		return "", fmt.Errorf("unknown channel %s", job.Channel)
	}
	_, dels, err := st.CreateEvent(ctx, store.NewEvent{
		EventType: job.TemplateID, Locale: job.Locale, Data: job.TemplateData,
		CorrelationID: job.CorrelationID, RequestID: job.RequestID,
		UserID: job.UserID, TenantID: job.TenantID, AgencyID: job.AgencyID,
	}, []store.NewDelivery{{Channel: slug, Address: job.Address}})
	if err != nil || len(dels) == 0 {
		return "", fmt.Errorf("import legacy job: %w", err)
	}
	return dels[0].ID, nil
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

func consumerName() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "worker"
	}
	return host + "-" + uuid.NewString()[:8]
}

func serviceName(name string) string {
	if name == "" || name == "app" {
		return "courier-worker"
	}
	return name
}
