package appconfig

import (
	"fmt"
	"time"

	"github.com/huponx/gocommon/config"
)

type Courier struct {
	config.Log
	config.Redis
	config.Postgres
	// GRPCTLS: the certificate courier serves gRPC with and the CA it trusts when it dials identity securely.
	config.GRPCTLS
	// MetricsAddr serves /metrics for Prometheus (the queue state, the outcome of the calls). Empty: no metrics.
	config.Metrics
	Host            string        `env:"HOST" envDefault:"0.0.0.0"`
	Port            int           `env:"PORT" envDefault:"9090"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"15s"`
	DefaultLocale   string        `env:"DEFAULT_LOCALE" envDefault:"en"`
	// AuthAudience is the audience callers' ServiceAccount tokens must have (any ENV except local).
	AuthAudience string `env:"COURIER_AUTH_AUDIENCE" envDefault:"courier"`
	// AuthAllowed lists the accepted callers, for example system:serviceaccount:billing:billing-api
	// or the short form billing:billing-api (expanded by gocommon grpcserver.KubeAuth).
	// Empty rejects every caller.
	AuthAllowed []string `env:"COURIER_AUTH_ALLOWED" envSeparator:","`

	// WebhookAllowPrivate lets a webhook point at an http address or a loopback or private one (a local run). Off, a
	// webhook is https and public only, so an administrator cannot aim courier at the cluster.
	WebhookAllowPrivate bool `env:"WEBHOOK_ALLOW_PRIVATE" envDefault:"false"`
	// The VAPID key pair of Web Push (`courierctl vapid` makes one) and the contact ("mailto:...") the push services may
	// write to. Without the keys there is no Web Push: the portal hides the switch. The courier API needs only the
	// public key; the worker, which sends, needs both. WEBHOOK_ALLOW_PRIVATE also lets a push endpoint be a local one.
	VAPIDPublicKey string `env:"VAPID_PUBLIC_KEY"`
	// Issuer is the iss of the user tokens identity issues and JWKSURL is where identity publishes its keys: the
	// inbox (InboxService) is called with a token of the user, exchanged by the gateway for the audience
	// COURIER_AUTH_AUDIENCE. Empty: no user token is accepted, the inbox does not work.
	Issuer  string `env:"COURIER_ISSUER"`
	JWKSURL string `env:"COURIER_JWKS_URL"`
	// IdentityTarget is the identity gRPC address courier resolves user_ids with (GetUser, permission
	// identity:user:read). Empty: Notify accepts raw recipients only.
	IdentityTarget string `env:"IDENTITY_TARGET"`
	// IdentityInsecure dials identity without TLS, the default. Set false once identity serves TLS (GRPC_TLS_CERT_FILE
	// there); the CA to trust is GRPC_TLS_CA_FILE.
	IdentityInsecure bool `env:"IDENTITY_INSECURE" envDefault:"true"`
	// Client credentials of courier in identity (confidential client with a role holding identity:user:read and
	// an exchange policy courier -> identity). They win over IdentityToken outside ENV=local.
	IdentityTokenURL     string `env:"IDENTITY_TOKEN_URL"`
	IdentityClientID     string `env:"IDENTITY_CLIENT_ID"`
	IdentityClientSecret string `env:"IDENTITY_CLIENT_SECRET"`
	IdentityAudience     string `env:"IDENTITY_AUDIENCE" envDefault:"identity"`
	// TelegramBotUsername is the bot the "link Telegram" button opens (https://t.me/<username>?start=<code>), with or
	// without the @. Empty: the user cannot link Telegram.
	TelegramBotUsername string `env:"TELEGRAM_BOT_USERNAME"`
	// PermissionsSyncTimeout is how long the start retries to register the permissions with identity (needs
	// IDENTITY_CLIENT_ID, whose client holds the role service_registrar). When it runs out courier exits non-zero.
	PermissionsSyncTimeout time.Duration `env:"PERMISSIONS_SYNC_TIMEOUT" envDefault:"2m"`
	// TemplateReload is how often templates edited through another process are loaded.
	TemplateReload time.Duration `env:"TEMPLATE_RELOAD" envDefault:"30s"`
	// UserCacheTTL is how long a resolved user (e-mail, language) is kept.
	UserCacheTTL time.Duration `env:"USER_CACHE_TTL" envDefault:"60s"`
}

func (c Courier) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

type Worker struct {
	config.Log
	config.Redis
	config.Postgres
	// MetricsAddr serves /metrics for Prometheus (the queue state, the outcome of every send). Empty: no metrics.
	config.Metrics
	HealthAddr      string        `env:"HEALTH_ADDR" envDefault:":8081"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"15s"`
	DefaultLocale   string        `env:"DEFAULT_LOCALE" envDefault:"en"`
	MaxRetry        int           `env:"QUEUE_MAX_RETRY" envDefault:"5"`
	ClaimMinIdle    time.Duration `env:"QUEUE_CLAIM_MIN_IDLE" envDefault:"30s"`
	// RelayInterval: how often the relay pushes the due deliveries (retries, rows Redis never got) to the stream.
	RelayInterval time.Duration `env:"QUEUE_RELAY_INTERVAL" envDefault:"5s"`
	// RelayStale: a delivery pushed to the stream and still pending after this long is pushed again (Redis lost it).
	RelayStale time.Duration `env:"QUEUE_RELAY_STALE" envDefault:"10m"`
	// RetentionDays: events and deliveries older than this are deleted (0 keeps everything).
	RetentionDays int `env:"RETENTION_DAYS" envDefault:"90"`
	// InboxRetentionDays: inbox items older than this are deleted (0 keeps everything).
	InboxRetentionDays int `env:"INBOX_RETENTION_DAYS" envDefault:"365"`
	// TemplateReload is how often the templates edited in the admin app are loaded.
	TemplateReload time.Duration `env:"TEMPLATE_RELOAD" envDefault:"30s"`
	// WebhookAllowPrivate lets a webhook reach a loopback or private address (a local run); see the courier API.
	WebhookAllowPrivate bool `env:"WEBHOOK_ALLOW_PRIVATE" envDefault:"false"`
	// The VAPID key pair of Web Push (`courierctl vapid` makes one) and the contact ("mailto:...") the push services may
	// write to. Without the keys there is no Web Push: the portal hides the switch. The courier API needs only the
	// public key; the worker, which sends, needs both. WEBHOOK_ALLOW_PRIVATE also lets a push endpoint be a local one.
	VAPIDPublicKey  string `env:"VAPID_PUBLIC_KEY"`
	VAPIDPrivateKey string `env:"VAPID_PRIVATE_KEY"`
	VAPIDSubject    string `env:"VAPID_SUBJECT"`
	// TelegramPoll: the worker listens to the bot (getUpdates) so that `/start <code>` links a chat. Needs
	// TELEGRAM_BOT_TOKEN; one worker polls at a time (an advisory lock in PostgreSQL).
	TelegramPoll  bool   `env:"TELEGRAM_POLL" envDefault:"true"`
	TelegramToken string `env:"TELEGRAM_BOT_TOKEN"`
	SMTPHost      string `env:"SMTP_HOST"`
	SMTPPort      int    `env:"SMTP_PORT" envDefault:"587"`
	SMTPUser      string `env:"SMTP_USER"`
	SMTPPassword  string `env:"SMTP_PASSWORD"`
	SMTPFrom      string `env:"SMTP_FROM"`
	// MailDir: write every e-mail as HTML and text files into this directory instead of sending it (local runs).
	MailDir string `env:"MAIL_DIR"`
	// MailBrand and MailLogoURL are the name and the logo (a PNG on a public https address) at the top of every HTML email.
	// A caller that sends `brand` or `logo_url` in the template data wins over them.
	MailBrand   string `env:"MAIL_BRAND"`
	MailLogoURL string `env:"MAIL_LOGO_URL"`
}

type Client struct {
	config.Log
	config.GRPCClient
	config.GRPCTLS
	// TokenFile is a ServiceAccount token file (projected volume). A local courier does not authenticate services.
	TokenFile string `env:"COURIER_TOKEN_FILE"`
}
