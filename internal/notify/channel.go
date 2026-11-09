package notify

import (
	"context"

	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

type Message struct {
	Subject string
	Text    string
	HTML    string
}

type Destination struct {
	Address string
}

type Channel interface {
	ID() courierv1.Channel
	Send(ctx context.Context, dest Destination, msg Message) error
}

func Slug(ch courierv1.Channel) (string, bool) {
	switch ch {
	case courierv1.Channel_CHANNEL_EMAIL:
		return "email", true
	case courierv1.Channel_CHANNEL_TELEGRAM:
		return "telegram", true
	default:
		return "", false
	}
}
