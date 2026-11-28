package service

import (
	"testing"
	"time"

	"github.com/huponx/courier/internal/store"
)

func TestQuietUntil(t *testing.T) {
	hcm := store.QuietHours{Timezone: "Asia/Ho_Chi_Minh", FromMin: 22 * 60, ToMin: 7 * 60} // UTC+7
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		name string
		now  string
		q    store.QuietHours
		want string // "" = not quiet
	}{
		{"evening, window ends tomorrow morning", "2026-11-28T16:00:00Z", hcm, "2026-11-29T00:00:00Z"},    // 23:00 local on the 28th -> 07:00 local on the 29th
		{"after midnight, window ends this morning", "2026-11-28T20:00:00Z", hcm, "2026-11-29T00:00:00Z"}, // 03:00 local on the 29th
		{"daytime", "2026-11-28T05:00:00Z", hcm, ""},                                                      // 12:00 local
		{"the end is outside", "2026-11-28T00:00:00Z", hcm, ""},                                           // 07:00 local
		{"the start is inside", "2026-11-28T15:00:00Z", hcm, "2026-11-29T00:00:00Z"},                      // 22:00 local on the 28th
		{"same day window", "2026-11-28T07:00:00Z", store.QuietHours{Timezone: "UTC", FromMin: 6 * 60, ToMin: 9 * 60}, "2026-11-28T09:00:00Z"},
		{"same day window, after", "2026-11-28T09:00:00Z", store.QuietHours{Timezone: "UTC", FromMin: 6 * 60, ToMin: 9 * 60}, ""},
		{"unknown zone", "2026-11-28T16:00:00Z", store.QuietHours{Timezone: "Mars/Base", FromMin: 0, ToMin: 60}, ""},
	}
	for _, c := range cases {
		got, quiet := quietUntil(at(c.now), c.q)
		if c.want == "" {
			if quiet {
				t.Errorf("%s: quiet until %s", c.name, got)
			}
			continue
		}
		if !quiet || !got.Equal(at(c.want)) {
			t.Errorf("%s: got %v %s, want %s", c.name, quiet, got.UTC().Format(time.RFC3339), c.want)
		}
	}
}
