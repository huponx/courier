package metrics

import (
	gometrics "github.com/huponx/gocommon/metrics"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	return rec.Body.String()
}

func TestAttemptsAreCountedByChannelAndOutcome(t *testing.T) {
	h := gometrics.Handler(Registry(nil, nil))
	Attempt("email", OutcomeSent, 120*time.Millisecond)
	Attempt("email", OutcomeSent, 80*time.Millisecond)
	Attempt("telegram", OutcomeDead, time.Second)
	Accepted("ok")
	Accepted("invalid")
	body := scrape(t, h)
	for _, want := range []string{
		`courier_delivery_attempts_total{channel="email",outcome="sent"} 2`,
		`courier_delivery_attempts_total{channel="telegram",outcome="dead"} 1`,
		`courier_send_duration_seconds_count{channel="email"} 2`,
		`courier_notify_total{result="ok"} 1`,
		`courier_notify_total{result="invalid"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in\n%s", want, body)
		}
	}
	// without a database or Redis the queue gauges are not invented
	if strings.Contains(body, "courier_deliveries_pending") || strings.Contains(body, "courier_stream_length") {
		t.Error("queue gauges without a source")
	}
}
