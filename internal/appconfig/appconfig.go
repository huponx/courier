package appconfig

import (
	"fmt"
	"time"

	"github.com/huponx/gocommon/config"
)

type Courier struct {
	config.Log
	config.Redis
	Host            string        `env:"HOST" envDefault:"0.0.0.0"`
	Port            int           `env:"PORT" envDefault:"9090"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"15s"`
	DefaultLocale   string        `env:"DEFAULT_LOCALE" envDefault:"en"`
	// S2SToken is the optional static Bearer token, read only when ENV=local.
	S2SToken string `env:"COURIER_S2S_TOKEN"`
	// AuthAudience is the audience callers' ServiceAccount tokens must have (any ENV except local).
	AuthAudience string `env:"COURIER_AUTH_AUDIENCE" envDefault:"courier"`
	// AuthAllowed lists the accepted callers, for example system:serviceaccount:billing:billing-api.
	// Empty rejects every caller.
	AuthAllowed []string `env:"COURIER_AUTH_ALLOWED" envSeparator:","`
}

func (c Courier) Addr() string {
	return fmt.Sprintf("%s:%d", c.Host, c.Port)
}

type Worker struct {
	config.Log
	config.Redis
	HealthAddr      string        `env:"HEALTH_ADDR" envDefault:":8081"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"15s"`
	DefaultLocale   string        `env:"DEFAULT_LOCALE" envDefault:"en"`
	MaxRetry        int           `env:"QUEUE_MAX_RETRY" envDefault:"5"`
	ClaimMinIdle    time.Duration `env:"QUEUE_CLAIM_MIN_IDLE" envDefault:"30s"`
	TelegramToken   string        `env:"TELEGRAM_BOT_TOKEN"`
	SMTPHost        string        `env:"SMTP_HOST"`
	SMTPPort        int           `env:"SMTP_PORT" envDefault:"587"`
	SMTPUser        string        `env:"SMTP_USER"`
	SMTPPassword    string        `env:"SMTP_PASSWORD"`
	SMTPFrom        string        `env:"SMTP_FROM"`
}

type Client struct {
	config.Log
	config.GRPCClient
	// S2SToken is a static Bearer token (local courier started with COURIER_S2S_TOKEN).
	S2SToken string `env:"COURIER_S2S_TOKEN"`
	// TokenFile is a ServiceAccount token file (projected volume); it wins over S2SToken.
	TokenFile string `env:"COURIER_TOKEN_FILE"`
}
