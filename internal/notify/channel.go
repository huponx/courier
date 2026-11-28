package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	courierv1 "github.com/huponx/protogen/go/courier/v1"
)

type Message struct {
	Subject string
	Text    string
	HTML    string
	// Link is the page a push notification opens when it is clicked.
	Link string
	// Event is the event the message is about; a webhook sends it as data, the other channels ignore it.
	Event *EventInfo
}

// EventInfo is the event behind a message.
type EventInfo struct {
	ID        string
	Type      string
	CreatedAt time.Time
	Data      map[string]string
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

// templateSlug is the file prefix of the templates of a channel. It includes the in-app channel, which is not a
// delivery channel (Slug refuses it, so a raw recipient cannot use it) but is rendered from the same templates.
func TemplateSlug(ch courierv1.Channel) (string, bool) {
	if ch == courierv1.Channel_CHANNEL_IN_APP || ch == courierv1.Channel_CHANNEL_PUSH {
		return "in_app", true // a push notification shows the title and the body of the inbox item
	}
	return Slug(ch)
}

// ErrPermanent marks a send error that a retry cannot fix (a bounced address, a blocked chat):
// the delivery is failed at once instead of going through the retries.
var ErrPermanent = errors.New("permanent delivery error")

// Permanent wraps err as a permanent error; nil stays nil.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrPermanent, err)
}

// IsPermanent reports whether err (or an error it wraps) is permanent.
func IsPermanent(err error) bool { return errors.Is(err, ErrPermanent) }

// ErrRecipientGone marks a permanent error that also says the address is dead (the user blocked the bot, the chat
// does not exist): the worker drops the binding of that address, so nothing more is sent to it.
var ErrRecipientGone = errors.New("recipient is gone")
