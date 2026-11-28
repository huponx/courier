package main

import (
	"context"
	"flag"
	"fmt"
	webpush "github.com/SherClockHolmes/webpush-go"
	"os"
	"strings"

	"github.com/huponx/courier/internal/appconfig"
	"github.com/huponx/gocommon/config"
	"github.com/huponx/gocommon/grpcclient"
	"github.com/huponx/gocommon/logging"
	courierv1 "github.com/huponx/protogen/go/courier/v1"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/structpb"
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
	// `courierctl vapid`: a key pair for Web Push (VAPID_PUBLIC_KEY, VAPID_PRIVATE_KEY of courier and its worker)
	if len(os.Args) == 2 && os.Args[1] == "vapid" {
		priv, pub, err := webpush.GenerateVAPIDKeys()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("VAPID_PUBLIC_KEY=%s\nVAPID_PRIVATE_KEY=%s\n", pub, priv)
		return
	}
	data := kvMap{}
	locale := flag.String("locale", "en", "template locale (vi, en)")
	templateID := flag.String("template", "hello", "template_id")
	channel := flag.String("channel", "telegram", "email or telegram")
	address := flag.String("address", "", "email or telegram chat_id")
	idemp := flag.String("idempotency-key", "", "optional idempotency key")
	get := flag.String("get", "", "print the deliveries of this notification_id instead of sending")
	retry := flag.String("retry", "", "retry this failed or dead delivery_id instead of sending")
	event := flag.String("event", "", "send Notify with this catalog event type (security.password_changed, ...) instead of SendNotification")
	users := flag.String("users", "", "with -event: comma separated identity user ids")
	flag.Var(data, "data", "template data key=value (repeatable)")
	flag.Parse()

	var ch courierv1.Channel
	// Notify takes users or an address; the other calls need neither
	needAddress := *get == "" && *retry == "" && (*event == "" || *users == "")
	if needAddress {
		var err error
		if ch, err = parseChannel(*channel); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if strings.TrimSpace(*address) == "" {
			fmt.Fprintln(os.Stderr, "-address is required")
			os.Exit(2)
		}
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
	dialOpts := []grpcclient.Option{
		grpcclient.WithLogger(log),
		grpcclient.WithTimeout(cfg.Timeout),
		grpcclient.WithInsecure(cfg.Insecure),
		grpcclient.WithBearerTokenFile(cfg.TokenFile),
	}
	if !cfg.Insecure {
		dialOpts = append(dialOpts, grpcclient.WithTLSFiles(cfg.ClientTLS()))
	}
	conn, err := grpcclient.Dial(ctx, cfg.Target, dialOpts...)
	if err != nil {
		log.Fatal("dial", zap.Error(err))
	}
	defer conn.Close()

	client := courierv1.NewCourierServiceClient(conn)
	switch {
	case *get != "":
		resp, err := client.GetNotification(ctx, &courierv1.GetNotificationRequest{NotificationId: *get})
		if err != nil {
			log.Fatal("get", zap.Error(err))
		}
		fmt.Printf("notification_id=%s template=%s created_at=%s\n", resp.GetNotificationId(), resp.GetTemplateId(), resp.GetCreatedAt())
		for _, d := range resp.GetDeliveries() {
			printDelivery(d)
		}
		return
	case *retry != "":
		resp, err := client.RetryDelivery(ctx, &courierv1.RetryDeliveryRequest{DeliveryId: *retry})
		if err != nil {
			log.Fatal("retry", zap.Error(err))
		}
		printDelivery(resp.GetDelivery())
		return
	}
	if *event != "" {
		fields := make(map[string]any, len(data))
		for k, v := range data {
			fields[k] = v
		}
		st, err := structpb.NewStruct(fields)
		if err != nil {
			log.Fatal("data", zap.Error(err))
		}
		req := &courierv1.NotifyRequest{IdempotencyKey: *idemp, EventType: *event, LocaleOverride: *locale, Data: st}
		for _, id := range strings.Split(*users, ",") {
			if id = strings.TrimSpace(id); id != "" {
				req.UserIds = append(req.UserIds, id)
			}
		}
		if strings.TrimSpace(*address) != "" {
			req.Recipients = []*courierv1.Recipient{{Channel: ch, Address: *address}}
		}
		resp, err := client.Notify(ctx, req)
		if err != nil {
			log.Fatal("notify", zap.Error(err))
		}
		fmt.Printf("event_id=%s queued=%d skipped=%v\n", resp.GetEventId(), resp.GetQueued(), resp.GetSkippedUserIds())
		return
	}
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

func printDelivery(d *courierv1.Delivery) {
	fmt.Printf("delivery_id=%s channel=%s address=%s status=%s attempts=%d next_attempt_at=%s sent_at=%s last_error=%q\n",
		d.GetId(), d.GetChannel(), d.GetAddress(), d.GetStatus(), d.GetAttempts(), d.GetNextAttemptAt(), d.GetSentAt(), d.GetLastError())
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
