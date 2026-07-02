package server

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/parithosh/piecesoflife/internal/store"
)

func TestAtLocalHour(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	require.NoError(t, err)

	berlin, err := time.LoadLocation("Europe/Berlin")
	require.NoError(t, err)

	tests := []struct {
		name string
		in   time.Time
		hour int
		loc  *time.Location
		want string // formatted in loc
	}{
		{
			name: "UTC instant pinned to IST morning stays on the IST day",
			// 2026-06-30 20:30 UTC == 2026-07-01 02:00 IST → July 1st 10:00 IST.
			in:   time.Date(2026, 6, 30, 20, 30, 0, 0, time.UTC),
			hour: 10,
			loc:  kolkata,
			want: "2026-07-01 10:00",
		},
		{
			name: "same-zone instant keeps its day",
			in:   time.Date(2026, 7, 15, 23, 45, 0, 0, berlin),
			hour: 21,
			loc:  berlin,
			want: "2026-07-15 21:00",
		},
		{
			name: "UTC loc behaves as plain truncation",
			in:   time.Date(2026, 7, 15, 3, 2, 1, 0, time.UTC),
			hour: 9,
			loc:  time.UTC,
			want: "2026-07-15 09:00",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := atLocalHour(tc.in, tc.hour, tc.loc)
			assert.Equal(t, tc.want, got.In(tc.loc).Format("2006-01-02 15:04"))
		})
	}
}

func TestSettingsLocationFallback(t *testing.T) {
	srv := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}

	tests := []struct {
		name     string
		settings *store.Settings
		want     string
	}{
		{name: "nil settings fall back to UTC", settings: nil, want: "UTC"},
		{name: "empty falls back to UTC", settings: &store.Settings{Timezone: ""}, want: "UTC"},
		{name: "invalid falls back to UTC", settings: &store.Settings{Timezone: "Mars/Olympus_Mons"}, want: "UTC"},
		{name: "valid IANA zone resolves", settings: &store.Settings{Timezone: "Asia/Kolkata"}, want: "Asia/Kolkata"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			loc := srv.settingsLocation(t.Context(), tc.settings)
			assert.Equal(t, tc.want, loc.String())
		})
	}
}
