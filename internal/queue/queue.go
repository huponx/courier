package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	courierv1 "github.com/huponx/protogen/go/courier/v1"
	"github.com/redis/go-redis/v9"
)

const (
	Stream = "courier:notifications"
	Group  = "courier-workers"
)

// Job is one stream message. The state lives in PostgreSQL: a message only names the delivery.
// The other fields exist for messages written before the database (a rolling deploy); the worker
// imports such a message into the database and carries on with the delivery.
type Job struct {
	DeliveryID string `json:"delivery_id,omitempty"`

	NotificationID string            `json:"notification_id,omitempty"`
	Channel        courierv1.Channel `json:"channel,omitempty"`
	Address        string            `json:"address,omitempty"`
	Locale         string            `json:"locale,omitempty"`
	TemplateID     string            `json:"template_id,omitempty"`
	TemplateData   map[string]string `json:"template_data,omitempty"`
	CorrelationID  string            `json:"correlation_id,omitempty"`
	UserID         string            `json:"user_id,omitempty"`
	TenantID       string            `json:"tenant_id,omitempty"`
	AgencyID       string            `json:"agency_id,omitempty"`
	RequestID      string            `json:"request_id,omitempty"`
}

type Message struct {
	ID  string
	Job Job
}

type Client struct {
	rdb *redis.Client
}

func New(ctx context.Context, redisURL string) (*Client, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return &Client{rdb: rdb}, nil
}

func (c *Client) Close() error {
	return c.rdb.Close()
}

func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

func (c *Client) EnsureGroup(ctx context.Context) error {
	err := c.rdb.XGroupCreateMkStream(ctx, Stream, Group, "0").Err()
	if err != nil && !isBusyGroup(err) {
		return fmt.Errorf("xgroup create: %w", err)
	}
	return nil
}

func isBusyGroup(err error) bool {
	return err != nil && (err.Error() == "BUSYGROUP Consumer Group name already exists")
}

// Enqueue pushes a delivery to the stream.
func (c *Client) Enqueue(ctx context.Context, deliveryID string) error {
	raw, err := json.Marshal(Job{DeliveryID: deliveryID})
	if err != nil {
		return err
	}
	return c.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: Stream,
		Values: map[string]any{"payload": string(raw)},
	}).Err()
}

func (c *Client) Read(ctx context.Context, consumer string, count int64, block time.Duration) ([]Message, error) {
	streams, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    Group,
		Consumer: consumer,
		Streams:  []string{Stream, ">"},
		Count:    count,
		Block:    block,
	}).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	good, bad := parseStreams(streams)
	c.discard(ctx, bad)
	return good, nil
}

func (c *Client) AutoClaim(ctx context.Context, consumer string, minIdle time.Duration, count int64) ([]Message, error) {
	msgs, _, err := c.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   Stream,
		Group:    Group,
		Consumer: consumer,
		MinIdle:  minIdle,
		Start:    "0-0",
		Count:    count,
	}).Result()
	if err != nil {
		return nil, err
	}
	good, bad := parseXMessages(msgs)
	c.discard(ctx, bad)
	return good, nil
}

// Ack acknowledges the message and deletes it: the state is in PostgreSQL, so the stream only holds work not done yet
// (a retry is a new message, pushed by the relay).
func (c *Client) Ack(ctx context.Context, id string) error {
	pipe := c.rdb.TxPipeline()
	ack := pipe.XAck(ctx, Stream, Group, id)
	pipe.XDel(ctx, Stream, id)
	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}
	return ack.Err()
}

func parseStreams(streams []redis.XStream) (good []Message, bad []string) {
	for _, s := range streams {
		g, b := parseXMessages(s.Messages)
		good, bad = append(good, g...), append(bad, b...)
	}
	return good, bad
}

// parseXMessages decodes the messages; the ids of those that cannot be decoded are returned apart, so that one broken
// entry does not take the readable ones of its batch with it.
func parseXMessages(msgs []redis.XMessage) (good []Message, bad []string) {
	for _, m := range msgs {
		raw, _ := m.Values["payload"].(string)
		var job Job
		if err := json.Unmarshal([]byte(raw), &job); err != nil {
			bad = append(bad, m.ID)
			continue
		}
		good = append(good, Message{ID: m.ID, Job: job})
	}
	return good, bad
}

// discard drops entries nobody can decode: acknowledged and deleted, or they would be claimed again for ever.
func (c *Client) discard(ctx context.Context, ids []string) {
	if len(ids) == 0 {
		return
	}
	pipe := c.rdb.TxPipeline()
	pipe.XAck(ctx, Stream, Group, ids...)
	pipe.XDel(ctx, Stream, ids...)
	_, _ = pipe.Exec(ctx)
}

// IsNoGroup reports whether err says the consumer group does not exist (Redis was flushed or replaced): the caller
// creates it again with EnsureGroup.
func IsNoGroup(err error) bool { return err != nil && strings.Contains(err.Error(), "NOGROUP") }

// InboxChannel is the Redis pub/sub channel of the inbox events of a user. The gateway subscribes to it for its
// SSE stream (InboxService.StreamInbox subscribes to it).
func InboxChannel(userID string) string { return "courier:inbox:" + userID }

// InboxEvent is the message of that channel: the new unread count and, for a new notification, the item (a read or
// archive change has no item; other tabs of the user only refresh the counter).
type InboxEvent struct {
	Unread int            `json:"unread"`
	Item   *InboxItemJSON `json:"item,omitempty"`
}

type InboxItemJSON struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Link      string `json:"link"`
	Severity  string `json:"severity"`
	CreatedAt int64  `json:"created_at"`
}

// PublishInbox is fire and forget: a subscriber that is not connected loses the message and reads the inbox later.
func (c *Client) PublishInbox(ctx context.Context, userID string, ev InboxEvent) error {
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return c.rdb.Publish(ctx, InboxChannel(userID), raw).Err()
}

// SubscribeInbox delivers the inbox events of a user until ctx ends. The returned channel closes then; a message that
// cannot be decoded is skipped.
func (c *Client) SubscribeInbox(ctx context.Context, userID string) (<-chan InboxEvent, error) {
	sub := c.rdb.Subscribe(ctx, InboxChannel(userID))
	// wait for the subscription to be confirmed: an event published right after this returns is not lost
	if _, err := sub.Receive(ctx); err != nil {
		_ = sub.Close()
		return nil, err
	}
	out := make(chan InboxEvent, 16)
	go func() {
		defer close(out)
		defer sub.Close()
		in := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-in:
				if !ok {
					return
				}
				var ev InboxEvent
				if json.Unmarshal([]byte(m.Payload), &ev) != nil {
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// StreamStats is the depth of the work queue: the messages in the stream and those delivered to a worker and not
// acknowledged yet.
func (c *Client) StreamStats(ctx context.Context) (length, pending int64, err error) {
	length, err = c.rdb.XLen(ctx, Stream).Result()
	if err != nil {
		return 0, 0, err
	}
	p, err := c.rdb.XPending(ctx, Stream, Group).Result()
	if err != nil {
		// no group yet: nothing is pending
		return length, 0, nil
	}
	return length, p.Count, nil
}
