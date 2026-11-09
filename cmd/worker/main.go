package main

import (
	"context"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/huponx/courier/internal/appconfig"
	"github.com/huponx/courier/internal/notify"
	"github.com/huponx/courier/internal/notify/email"
	"github.com/huponx/courier/internal/notify/render"
	"github.com/huponx/courier/internal/notify/telegram"
	"github.com/huponx/courier/internal/queue"
	"github.com/huponx/gocommon/config"
	"github.com/huponx/gocommon/healthz"
	"github.com/huponx/gocommon/logging"
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

	q, err := queue.New(ctx, cfg.URL())
	if err != nil {
		log.Fatal("redis", zap.Error(err))
	}
	defer q.Close()
	if err := q.EnsureGroup(ctx); err != nil {
		log.Fatal("redis group", zap.Error(err))
	}

	renderer := render.New(cfg.DefaultLocale)
	reg := notify.NewRegistry(
		email.New(email.Config{
			Host:     cfg.SMTPHost,
			Port:     cfg.SMTPPort,
			User:     cfg.SMTPUser,
			Password: cfg.SMTPPassword,
			From:     cfg.SMTPFrom,
		}),
		telegram.New(cfg.TelegramToken),
	)

	run := worker.New(worker.WithLogger(log))
	h := worker.NewHealth(cfg.HealthAddr, healthz.Check{
		Name: "redis",
		Fn:   q.Ping,
	})
	go func() {
		if err := h.ListenAndServe(); err != nil {
			log.Error("health server", zap.Error(err))
		}
	}()

	consumer := consumerName()
	log.Info("worker started", zap.String("consumer", consumer), zap.String("health", cfg.HealthAddr))

	consume(ctx, log, run, q, renderer, reg, consumer, cfg)

	h.SetReady(false)
	drainCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := run.Drain(drainCtx); err != nil {
		log.Warn("drain", zap.Error(err))
	}
	_ = h.Shutdown(drainCtx)
}

func consume(
	ctx context.Context,
	log *zap.Logger,
	run *worker.Runner,
	q *queue.Client,
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
		}
		fresh, err := q.Read(ctx, consumer, 16, 2*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Error("xreadgroup", zap.Error(err))
			time.Sleep(time.Second)
			continue
		}

		for _, msg := range append(claimed, fresh...) {
			jobCtx := requestctx.WithValues(context.Background(), requestctx.Values{
				CorrelationID: msg.Job.CorrelationID,
				UserID:        msg.Job.UserID,
				TenantID:      msg.Job.TenantID,
				AgencyID:      msg.Job.AgencyID,
				RequestID:     msg.Job.RequestID,
			})
			m := msg
			if !run.Go(jobCtx, func(c context.Context) error {
				return handle(c, q, renderer, reg, m, cfg.MaxRetry)
			}) {
				return
			}
		}
	}
}

func handle(
	ctx context.Context,
	q *queue.Client,
	renderer *render.Renderer,
	reg *notify.Registry,
	msg queue.Message,
	maxRetry int,
) error {
	job := msg.Job
	if job.Channel == courierv1.Channel_CHANNEL_UNSPECIFIED || !reg.Has(job.Channel) {
		logging.FromCtx(ctx).Error("drop unknown channel", zap.String("channel", job.Channel.String()))
		_ = q.Ack(ctx, msg.ID)
		q.ClearAttempts(ctx, msg.ID)
		return nil
	}

	rendered, err := renderer.Render(job.Locale, job.TemplateID, job.Channel, job.TemplateData)
	if err != nil {
		logging.FromCtx(ctx).Error("render failed permanently", zap.Error(err))
		_ = q.Ack(ctx, msg.ID)
		q.ClearAttempts(ctx, msg.ID)
		return nil
	}

	if err := reg.Send(ctx, job.Channel, notify.Destination{Address: job.Address}, rendered); err != nil {
		n, incrErr := q.IncrAttempts(ctx, msg.ID)
		if incrErr == nil && int(n) >= maxRetry {
			logging.FromCtx(ctx).Error("send failed permanently", zap.Error(err), zap.Int64("attempts", n))
			_ = q.Ack(ctx, msg.ID)
			q.ClearAttempts(ctx, msg.ID)
			return nil
		}
		return err
	}

	if err := q.Ack(ctx, msg.ID); err != nil {
		return err
	}
	q.ClearAttempts(ctx, msg.ID)
	logging.FromCtx(ctx).Info("notification sent",
		zap.String("notification_id", job.NotificationID),
		zap.String("channel", job.Channel.String()),
	)
	return nil
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
