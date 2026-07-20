package server

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestNextIssueOpenTime(t *testing.T) {
	now := time.Date(2026, 4, 22, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		currentOpen time.Time
		frequency   string
		now         time.Time
		want        time.Time
	}{
		{
			name:        "biweekly lands in the future",
			currentOpen: time.Date(2026, 4, 15, 9, 0, 0, 0, time.UTC),
			frequency:   "biweekly",
			now:         now,
			want:        time.Date(2026, 4, 29, 9, 0, 0, 0, time.UTC),
		},
		{
			name:        "monthly lands in the future",
			currentOpen: time.Date(2026, 4, 15, 9, 0, 0, 0, time.UTC),
			frequency:   "monthly",
			now:         now,
			want:        time.Date(2026, 5, 15, 9, 0, 0, 0, time.UTC),
		},
		{
			name:        "quarterly lands in the future",
			currentOpen: time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC),
			frequency:   "quarterly",
			now:         now,
			want:        time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
		},
		{
			name:        "unknown frequency falls through to monthly",
			currentOpen: time.Date(2026, 4, 15, 9, 0, 0, 0, time.UTC),
			frequency:   "gibberish",
			now:         now,
			want:        time.Date(2026, 5, 15, 9, 0, 0, 0, time.UTC),
		},
		{
			name:        "monthly clamps January 31 to February instead of skipping it",
			currentOpen: time.Date(2027, 1, 31, 9, 0, 0, 0, time.UTC),
			frequency:   "monthly",
			now:         now,
			want:        time.Date(2027, 2, 28, 9, 0, 0, 0, time.UTC),
		},
		{
			name:        "monthly honors leap day when clamping",
			currentOpen: time.Date(2028, 1, 31, 9, 0, 0, 0, time.UTC),
			frequency:   "monthly",
			now:         now,
			want:        time.Date(2028, 2, 29, 9, 0, 0, 0, time.UTC),
		},
		{
			name:        "quarterly clamps into a short target month",
			currentOpen: time.Date(2026, 11, 30, 9, 0, 0, 0, time.UTC),
			frequency:   "quarterly",
			now:         now,
			want:        time.Date(2027, 2, 28, 9, 0, 0, 0, time.UTC),
		},
		{
			name:        "clamps to now+48h when slot already elapsed",
			currentOpen: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			frequency:   "biweekly",
			now:         now,
			// current + 14d = Jan 15 — well in the past vs now (Apr 22) — clamp
			want: now.Add(48 * time.Hour),
		},
		{
			name:        "clamps when computed next is within 24h of now",
			currentOpen: now.Add(-14 * 24 * time.Hour).Add(23 * time.Hour),
			frequency:   "biweekly",
			now:         now,
			// next = now + 23h — within 24h clamp → now+48h
			want: now.Add(48 * time.Hour),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nextIssueOpenTime(tt.currentOpen, tt.frequency, tt.now)
			assert.True(t, got.Equal(tt.want),
				"nextIssueOpenTime(%v, %q, %v) = %v; want %v",
				tt.currentOpen, tt.frequency, tt.now, got, tt.want)
		})
	}
}
