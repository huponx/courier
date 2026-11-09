package queue

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	courierv1 "github.com/huponx/protogen/go/courier/v1"
	"github.com/redis/go-redis/v9"
)

const (
	Stream      = "courier:notifications"
	Group       = "courier-workers"
	idempTTL    = 24 * time.Hour
	idempPref   = "courier:idemp:"
	attemptPref = "courier:attempts:"
)

type Job struct {
	NotificationID string            `json:"notification_id"`
	Channel        courierv1.Channel `json:"channel"`
	Address        string            `json:"address"`
	Locale         string            `json:"locale"`
	TemplateID     string            `json:"template_id"`
	TemplateData   map[string]string `json:"template_data"`
	CorrelationID  string            `json:"correlation_id"`
	UserID         string            `json:"user_id"`
	TenantID       string            `json:"tenant_id"`
	AgencyID       string            `json:"agency_id"`
	RequestID      string            `json:"request_id"`
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

func (c *Client) Enqueue(ctx context.Context, job Job) error {
	raw, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return c.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: Stream,
		Values: map[string]any{"payload": string(raw)},
	}).Err()
}

func (c *Client) ClaimIdempotency(ctx context.Context, key, notificationID string) (existing string, claimed bool, err error) {
	ok, err := c.rdb.SetNX(ctx, idempPref+key, notificationID, idempTTL).Result()
	if err != nil {
		return "", false, err
	}
	if ok {
		return notificationID, true, nil
	}
	existing, err = c.rdb.Get(ctx, idempPref+key).Result()
	if err != nil {
		return "", false, err
	}
	return existing, false, nil
}

func (c *Client) DeleteIdempotency(ctx context.Context, key string) error {
	return c.rdb.Del(ctx, idempPref+key).Err()
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
	return parseStreams(streams)
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
	return parseXMessages(msgs)
}

func (c *Client) Ack(ctx context.Context, id string) error {
	return c.rdb.XAck(ctx, Stream, Group, id).Err()
}

func (c *Client) IncrAttempts(ctx context.Context, id string) (int64, error) {
	n, err := c.rdb.Incr(ctx, attemptPref+id).Result()
	if err != nil {
		return 0, err
	}
	_ = c.rdb.Expire(ctx, attemptPref+id, 24*time.Hour).Err()
	return n, nil
}

func (c *Client) ClearAttempts(ctx context.Context, id string) {
	_ = c.rdb.Del(ctx, attemptPref+id).Err()
}

func parseStreams(streams []redis.XStream) ([]Message, error) {
	var out []Message
	for _, s := range streams {
		msgs, err := parseXMessages(s.Messages)
		if err != nil {
			return nil, err
		}
		out = append(out, msgs...)
	}
	return out, nil
}

func parseXMessages(msgs []redis.XMessage) ([]Message, error) {
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		raw, _ := m.Values["payload"].(string)
		var job Job
		if err := json.Unmarshal([]byte(raw), &job); err != nil {
			return nil, fmt.Errorf("decode job %s: %w", m.ID, err)
		}
		out = append(out, Message{ID: m.ID, Job: job})
	}
	return out, nil
}
