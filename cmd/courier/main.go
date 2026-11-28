package main

import (
	"context"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/huponx/courier/internal/appconfig"
	"github.com/huponx/courier/internal/auth"
	"github.com/huponx/courier/internal/catalog"
	"github.com/huponx/courier/internal/directory"
	"github.com/huponx/courier/internal/metrics"
	"github.com/huponx/courier/internal/notify/render"
	"github.com/huponx/courier/internal/notify/webhook"
	"github.com/huponx/courier/internal/queue"
	"github.com/huponx/courier/internal/service"
	"github.com/huponx/courier/internal/store"
	"github.com/huponx/courier/internal/tplsync"
	"github.com/huponx/gocommon/config"
	"github.com/huponx/gocommon/grpcclient"
	"github.com/huponx/gocommon/grpcserver"
	"github.com/huponx/gocommon/jwtx"
	"github.com/huponx/gocommon/logging"
	gometrics "github.com/huponx/gocommon/metrics"
	"github.com/huponx/gocommon/permsync"
	"github.com/huponx/gocommon/tokenexchange"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
	identityv1 "github.com/huponx/protogen/go/identity/v1"
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
	users, registrar := identityResolver(ctx, cfg, log)

	renderer := render.New(cfg.DefaultLocale)
	// the templates edited in the admin app, over the built-in ones; kept current while the process runs
	go tplsync.Run(ctx, st, renderer, cfg.TemplateReload, log)
	srv := grpcserver.New(
		grpcserver.WithLogger(log),
		grpcserver.WithReflection(true),
		auth.ServerOption(cfg, log, jwtVerifier(cfg, log)),
		grpcserver.WithMethodPermissions(service.Permissions),
	)
	notifier := service.New(st, q, renderer, cat, users, cfg.DefaultLocale)
	courierv1.RegisterCourierServiceServer(srv.GRPC, notifier)
	var lister service.UserLister
	if l, ok := users.(service.UserLister); ok {
		lister = l
	}
	courierv1.RegisterAdminNotificationServiceServer(srv.GRPC, service.NewAdmin(st, cat, renderer, notifier, lister,
		func(c context.Context) error { return tplsync.Load(c, st, renderer) }).
		WithWebhooks(webhook.New(st, cfg.WebhookAllowPrivate), cfg.WebhookAllowPrivate))
	courierv1.RegisterInboxServiceServer(srv.GRPC, service.NewInbox(st, q))
	courierv1.RegisterPreferencesServiceServer(srv.GRPC, service.NewPreferences(st, cat, cfg.TelegramBotUsername).WithPush(cfg.VAPIDPublicKey, cfg.WebhookAllowPrivate))

	lis, err := net.Listen("tcp", cfg.Addr())
	if err != nil {
		log.Fatal("listen", zap.Error(err), zap.String("addr", cfg.Addr()))
	}

	// the permissions of the admin screens (permissions.proto) go to identity before courier is ready
	if registrar != nil {
		if err := permsync.RegisterWithRetry(ctx, log, registrar, "courier", cfg.PermissionsSyncTimeout, courierv1.File_courier_v1_permissions_proto); err != nil && ctx.Err() == nil {
			log.Fatal("permission sync", zap.Error(err))
		}
	} else {
		log.Warn("no IDENTITY_CLIENT_ID: the permissions of courier are not registered with identity (administrators cannot grant them)")
	}

	defer gometrics.Serve(cfg.MetricsAddr, metrics.Registry(st, q), log)()

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

// jwtVerifier checks the user tokens of the inbox; nil (with a warning) when COURIER_ISSUER is not set.
func jwtVerifier(cfg appconfig.Courier, log *zap.Logger) *jwtx.Verifier {
	if cfg.Issuer == "" || cfg.JWKSURL == "" {
		log.Warn("COURIER_ISSUER or COURIER_JWKS_URL is empty: user tokens are refused, the inbox does not work")
		return nil
	}
	keys, err := jwtx.NewRemoteKeySet(jwtx.RemoteKeySetConfig{URL: cfg.JWKSURL})
	if err != nil {
		log.Fatal("jwks", zap.Error(err))
	}
	v, err := jwtx.NewVerifier(jwtx.VerifierConfig{Issuer: cfg.Issuer, Audience: cfg.AuthAudience, Keys: keys})
	if err != nil {
		log.Fatal("jwt verifier", zap.Error(err))
	}
	return v
}

// identityResolver connects to identity when IDENTITY_TARGET is set; nil otherwise (raw recipients only).
func identityResolver(ctx context.Context, cfg appconfig.Courier, log *zap.Logger) (directory.Resolver, permsync.Registrar) {
	if cfg.IdentityTarget == "" {
		log.Warn("IDENTITY_TARGET is empty: Notify accepts raw recipients only, user_ids are refused")
		return nil, nil
	}
	opts := []grpcclient.Option{grpcclient.WithLogger(log), grpcclient.WithInsecure(strings.EqualFold(cfg.Env, "local"))}
	if cfg.IdentityClientID == "" {
		log.Warn("no IDENTITY_CLIENT_ID: identity calls carry no token and will be refused (client credentials of the client `courier`)")
	} else {
		tc, err := tokenexchange.New(tokenexchange.Config{
			TokenURL: cfg.IdentityTokenURL, ClientID: cfg.IdentityClientID, ClientSecret: cfg.IdentityClientSecret,
		})
		if err != nil {
			log.Fatal("identity client credentials", zap.Error(err))
		}
		opts = append(opts, grpcclient.WithServiceToken(tc, cfg.IdentityAudience))
	}
	conn, err := grpcclient.Dial(ctx, cfg.IdentityTarget, opts...)
	if err != nil {
		log.Fatal("identity", zap.Error(err))
	}
	client := identityv1.NewIdentityServiceClient(conn)
	var registrar permsync.Registrar
	if cfg.IdentityClientID != "" { // permissions are registered as the service itself, which needs the client credentials
		registrar = client
	}
	return directory.NewIdentity(client, cfg.UserCacheTTL), registrar
}

func serviceName(name string) string {
	if name == "" || name == "app" {
		return "courier"
	}
	return name
}
