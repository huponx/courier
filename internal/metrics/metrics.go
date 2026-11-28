// Package metrics exposes the state of courier to Prometheus. The queue and the delivery counts are read from
// PostgreSQL and Redis when Prometheus scrapes (so every replica answers the same, whichever one is asked); the
// outcome of each send is counted in the worker that made it.
package metrics

import (
	"context"
	"sync"
	"time"

	"github.com/huponx/courier/internal/queue"
	"github.com/huponx/courier/internal/store"
	gometrics "github.com/huponx/gocommon/metrics"
	"github.com/prometheus/client_golang/prometheus"
)

// Outcomes of an attempt to send a delivery.
const (
	OutcomeSent   = "sent"
	OutcomeRetry  = "retry"  // transient error, scheduled again
	OutcomeFailed = "failed" // permanent error
	OutcomeDead   = "dead"   // transient error and out of attempts
)

var (
	attempts = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "courier_delivery_attempts_total", Help: "Attempts to send a delivery, by channel and outcome (sent, retry, failed, dead).",
	}, []string{"channel", "outcome"})
	sendSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "courier_send_duration_seconds", Help: "Time the channel took to send one message.", Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30},
	}, []string{"channel"})
	accepted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "courier_notify_total", Help: "Notify and SendNotification calls, by result (ok, invalid, error).",
	}, []string{"result"})
)

// Attempt records one attempt of the worker.
func Attempt(channel, outcome string, took time.Duration) {
	attempts.WithLabelValues(channel, outcome).Inc()
	sendSeconds.WithLabelValues(channel).Observe(took.Seconds())
}

// Accepted records the result of a call that creates notifications.
func Accepted(result string) { accepted.WithLabelValues(result).Inc() }

// collector reads the queue state at a scrape, at most once per `ttl`: a scrape every few seconds must not become
// a query every few seconds.
type collector struct {
	st  *store.Store
	q   *queue.Client
	ttl time.Duration

	mu      sync.Mutex
	at      time.Time
	stats   store.Stats
	length  int64
	pending int64
	okDB    bool
	okRedis bool

	pendingDesc, recentDesc, oldestDesc, lengthDesc, pendingStreamDesc, upDesc *prometheus.Desc
}

func newCollector(st *store.Store, q *queue.Client) *collector {
	d := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc(name, help, labels, nil)
	}
	return &collector{
		st: st, q: q, ttl: 10 * time.Second,
		pendingDesc:       d("courier_deliveries_pending", "Deliveries waiting for a send or a retry.", "channel"),
		recentDesc:        d("courier_deliveries_recent", "Deliveries created in the last 24 hours, by status and channel.", "status", "channel"),
		oldestDesc:        d("courier_oldest_due_delivery_age_seconds", "Age of the oldest pending delivery that is already due (0: none). Grows when the workers are stuck or down."),
		lengthDesc:        d("courier_stream_length", "Messages in the Redis stream of the work queue."),
		pendingStreamDesc: d("courier_stream_pending", "Messages delivered to a worker and not acknowledged yet."),
		upDesc:            d("courier_dependency_up", "1 when the dependency answered the last scrape.", "dependency"),
	}
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.pendingDesc, c.recentDesc, c.oldestDesc, c.lengthDesc, c.pendingStreamDesc, c.upDesc} {
		ch <- d
	}
}

func (c *collector) refresh() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if time.Since(c.at) < c.ttl {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if c.st != nil {
		s, err := c.st.Stats(ctx)
		c.okDB = err == nil
		if err == nil {
			c.stats = s
		}
	}
	if c.q != nil {
		l, p, err := c.q.StreamStats(ctx)
		c.okRedis = err == nil
		if err == nil {
			c.length, c.pending = l, p
		}
	}
	c.at = time.Now()
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	c.refresh()
	c.mu.Lock()
	defer c.mu.Unlock()
	up := func(b bool) float64 {
		if b {
			return 1
		}
		return 0
	}
	if c.st != nil {
		ch <- prometheus.MustNewConstMetric(c.upDesc, prometheus.GaugeValue, up(c.okDB), "postgres")
		for channel, n := range c.stats.Pending {
			ch <- prometheus.MustNewConstMetric(c.pendingDesc, prometheus.GaugeValue, float64(n), channel)
		}
		for k, n := range c.stats.Recent {
			ch <- prometheus.MustNewConstMetric(c.recentDesc, prometheus.GaugeValue, float64(n), k[0], k[1])
		}
		ch <- prometheus.MustNewConstMetric(c.oldestDesc, prometheus.GaugeValue, c.stats.OldestDue.Seconds())
	}
	if c.q != nil {
		ch <- prometheus.MustNewConstMetric(c.upDesc, prometheus.GaugeValue, up(c.okRedis), "redis")
		ch <- prometheus.MustNewConstMetric(c.lengthDesc, prometheus.GaugeValue, float64(c.length))
		ch <- prometheus.MustNewConstMetric(c.pendingStreamDesc, prometheus.GaugeValue, float64(c.pending))
	}
}

// Registry builds the registry of a process: its counters and the queue state (either source may be nil), with the Go
// runtime and the process (gocommon/metrics). Serve it with gocommon's metrics.Serve.
func Registry(st *store.Store, q *queue.Client) *prometheus.Registry {
	return gometrics.NewRegistry(attempts, sendSeconds, accepted, newCollector(st, q))
}
