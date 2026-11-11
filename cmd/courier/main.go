package main

import (
	"context"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/huponx/courier/internal/appconfig"
	"github.com/huponx/courier/internal/auth"
	"github.com/huponx/courier/internal/notify/render"
	"github.com/huponx/courier/internal/queue"
	"github.com/huponx/courier/internal/service"
	"github.com/huponx/gocommon/config"
	"github.com/huponx/gocommon/grpcserver"
	"github.com/huponx/gocommon/logging"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

func main() {
	cfg := config.MustLoad[appconfig.Courier]()
	if err := config.ValidatePort(cfg.Port); err != nil {
		panic(err)
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

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
	srv := grpcserver.New(
		grpcserver.WithLogger(log),
		grpcserver.WithReflection(true),
		auth.ServerOption(cfg, log),
	)
	courierv1.RegisterCourierServiceServer(srv.GRPC, service.New(q, renderer))

	lis, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		log.Fatal("listen", zap.Error(err), zap.String("addr", cfg.Addr()))
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("courier grpc listening", zap.String("addr", cfg.Addr()))
		errCh <- srv.GRPC.Serve(lis)
	}()

	select {
	case <-ctx.Done():
	case err := <-errCh:
		if err != nil && err != grpc.ErrServerStopped {
			log.Fatal("grpc serve", zap.Error(err))
		}
	}

	shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	srv.Stop(shutCtx)
}

func serviceName(name string) string {
	if name == "" || name == "app" {
		return "courier"
	}
	return name
}
