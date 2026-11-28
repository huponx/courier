package queue

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// needs a Redis (COURIER_TEST_REDIS_URL); it uses database 10 and empties it
func newTestQueue(t *testing.T) (*Client, *redis.Client) {
	t.Helper()
	url := os.Getenv("COURIER_TEST_REDIS_URL")
	if url == "" {
		t.Skip("COURIER_TEST_REDIS_URL is not set")
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	opt.DB = 10
	raw := redis.NewClient(opt)
	t.Cleanup(func() { _ = raw.Close() })
	ctx := context.Background()
	if err := raw.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	q, err := New(ctx, "redis://"+opt.Addr+"/10")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatal(err)
	}
	return q, raw
}

func TestAckRemovesTheMessage(t *testing.T) {
	q, raw := newTestQueue(t)
	ctx := context.Background()
	for _, id := range []string{"d1", "d2"} {
		if err := q.Enqueue(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := q.Read(ctx, "w1", 10, time.Second)
	if err != nil || len(msgs) != 2 || msgs[0].Job.DeliveryID != "d1" {
		t.Fatalf("%v %v", msgs, err)
	}
	if err := q.Ack(ctx, msgs[0].ID); err != nil {
		t.Fatal(err)
	}
	if n, _ := raw.XLen(ctx, Stream).Result(); n != 1 {
		t.Fatalf("the acknowledged message must be deleted, the stream holds %d", n)
	}
	if l, p, _ := q.StreamStats(ctx); l != 1 || p != 1 {
		t.Fatalf("stats %d %d", l, p)
	}
}

// a message nobody can decode is dropped, and the readable ones of its batch are still handled
func TestUndecodableMessageDoesNotPoisonTheBatch(t *testing.T) {
	q, raw := newTestQueue(t)
	ctx := context.Background()
	_ = q.Enqueue(ctx, "good-1")
	if err := raw.XAdd(ctx, &redis.XAddArgs{Stream: Stream, Values: map[string]any{"payload": "{not json"}}).Err(); err != nil {
		t.Fatal(err)
	}
	_ = q.Enqueue(ctx, "good-2")
	msgs, err := q.Read(ctx, "w1", 10, time.Second)
	if err != nil || len(msgs) != 2 || msgs[0].Job.DeliveryID != "good-1" || msgs[1].Job.DeliveryID != "good-2" {
		t.Fatalf("%v %v", msgs, err)
	}
	if n, _ := raw.XLen(ctx, Stream).Result(); n != 2 {
		t.Fatalf("the broken entry must be deleted: %d left", n)
	}
	// and it cannot come back through the claim of idle messages
	time.Sleep(20 * time.Millisecond)
	again, err := q.AutoClaim(ctx, "w2", 10*time.Millisecond, 16)
	if err != nil || len(again) != 2 {
		t.Fatalf("claim: %v %v", again, err)
	}
}

// Redis was flushed: the group is gone, the worker notices and creates it again
func TestMissingGroupIsDetectedAndRecreated(t *testing.T) {
	q, raw := newTestQueue(t)
	ctx := context.Background()
	if err := raw.FlushDB(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	_ = q.Enqueue(ctx, "d1") // XADD makes the stream, not the group
	_, err := q.Read(ctx, "w1", 10, 100*time.Millisecond)
	if !IsNoGroup(err) {
		t.Fatalf("a missing group must be recognised: %v", err)
	}
	if err := q.EnsureGroup(ctx); err != nil {
		t.Fatal(err)
	}
	msgs, err := q.Read(ctx, "w1", 10, time.Second)
	if err != nil || len(msgs) != 1 || msgs[0].Job.DeliveryID != "d1" {
		t.Fatalf("after the group is back: %v %v", msgs, err)
	}
	if IsNoGroup(nil) || IsNoGroup(context.Canceled) {
		t.Fatal("other errors are not a missing group")
	}
}
