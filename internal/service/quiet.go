package service

import (
	"time"

	"github.com/huponx/courier/internal/store"
)

// quietUntil says whether t falls in the quiet hours and, if so, when the window ends. A time zone that cannot be
// loaded means no quiet hours (a message is never held back for a window we cannot place).
func quietUntil(t time.Time, q store.QuietHours) (time.Time, bool) {
	loc, err := time.LoadLocation(q.Timezone)
	if err != nil {
		return time.Time{}, false
	}
	local := t.In(loc)
	min := local.Hour()*60 + local.Minute()
	var inside, endsToday bool
	if q.FromMin < q.ToMin { // 13:00 to 17:00
		inside = min >= q.FromMin && min < q.ToMin
		endsToday = true
	} else { // 22:00 to 07:00: the end is today before the end, tomorrow after the start
		inside = min >= q.FromMin || min < q.ToMin
		endsToday = min < q.ToMin
	}
	if !inside {
		return time.Time{}, false
	}
	day := local
	if !endsToday {
		day = local.AddDate(0, 0, 1)
	}
	return time.Date(day.Year(), day.Month(), day.Day(), q.ToMin/60, q.ToMin%60, 0, 0, loc), true
}
