package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/huponx/courier/internal/appconfig"
	"github.com/huponx/gocommon/config"
	"github.com/huponx/gocommon/grpcclient"
	"github.com/huponx/gocommon/logging"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
	"go.uber.org/zap"
)

type kvMap map[string]string

func (m kvMap) String() string { return "" }

func (m kvMap) Set(v string) error {
	k, val, ok := strings.Cut(v, "=")
	if !ok || strings.TrimSpace(k) == "" {
		return fmt.Errorf("data must be key=value")
	}
	m[strings.TrimSpace(k)] = val
	return nil
}

func main() {
	data := kvMap{}
	locale := flag.String("locale", "en", "template locale (vi, en)")
	templateID := flag.String("template", "hello", "template_id")
	channel := flag.String("channel", "telegram", "email or telegram")
	address := flag.String("address", "", "email or telegram chat_id")
	idemp := flag.String("idempotency-key", "", "optional idempotency key")
	flag.Var(data, "data", "template data key=value (repeatable)")
	flag.Parse()

	ch, err := parseChannel(*channel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if strings.TrimSpace(*address) == "" {
		fmt.Fprintln(os.Stderr, "-address is required")
		os.Exit(2)
	}

	cfg := config.MustLoad[appconfig.Client]()
	log, err := logging.New(logging.Config{
		Level:    cfg.Level,
		Encoding: cfg.Encoding,
		Service:  "courierctl",
		Env:      cfg.Env,
	})
	if err != nil {
		panic(err)
	}
	defer logging.Sync(log)

	ctx := context.Background()
	conn, err := grpcclient.Dial(ctx, cfg.Target,
		grpcclient.WithLogger(log),
		grpcclient.WithTimeout(cfg.Timeout),
		grpcclient.WithInsecure(cfg.Insecure),
		grpcclient.WithBearerToken(cfg.S2SToken),
	)
	if err != nil {
		log.Fatal("dial", zap.Error(err))
	}
	defer conn.Close()

	client := courierv1.NewCourierServiceClient(conn)
	resp, err := client.SendNotification(ctx, &courierv1.SendNotificationRequest{
		IdempotencyKey: *idemp,
		Locale:         *locale,
		TemplateId:     *templateID,
		TemplateData:   data,
		Recipients: []*courierv1.Recipient{{
			Channel: ch,
			Address: *address,
		}},
	})
	if err != nil {
		log.Fatal("send", zap.Error(err))
	}
	fmt.Printf("notification_id=%s queued=%d\n", resp.GetNotificationId(), resp.GetQueued())
}

func parseChannel(s string) (courierv1.Channel, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "email":
		return courierv1.Channel_CHANNEL_EMAIL, nil
	case "telegram":
		return courierv1.Channel_CHANNEL_TELEGRAM, nil
	default:
		return courierv1.Channel_CHANNEL_UNSPECIFIED, fmt.Errorf("unknown channel %q", s)
	}
}
