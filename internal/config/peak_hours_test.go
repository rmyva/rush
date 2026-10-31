package config

import (
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/stretchr/testify/require"
)

func at(t *testing.T, h, m int) time.Time {
	t.Helper()
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), h, m, 0, 0, now.Location())
}

func TestPeakHoursWindow_Validate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		w       PeakHoursWindow
		wantErr bool
	}{
		{"empty (feature off)", PeakHoursWindow{}, false},
		{"normal window", PeakHoursWindow{Start: "09:00", End: "18:00"}, false},
		{"overnight window", PeakHoursWindow{Start: "22:00", End: "06:00"}, false},
		{"boundary start", PeakHoursWindow{Start: "00:00", End: "23:59"}, false},
		{"missing end", PeakHoursWindow{Start: "09:00"}, true},
		{"missing start", PeakHoursWindow{End: "18:00"}, true},
		{"bad start format", PeakHoursWindow{Start: "9:00", End: "18:00"}, true},
		{"bad end format", PeakHoursWindow{Start: "09:00", End: "18-00"}, true},
		{"out of range hour", PeakHoursWindow{Start: "24:00", End: "18:00"}, true},
		{"out of range minute", PeakHoursWindow{Start: "09:60", End: "18:00"}, true},
		{"garbage", PeakHoursWindow{Start: "nope", End: "18:00"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.w.Validate()
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestPeakHoursWindow_InPeakHours_NormalWindow(t *testing.T) {
	t.Parallel()
	w := PeakHoursWindow{Start: "09:00", End: "18:00"}
	// Before window.
	require.False(t, w.InPeakHours(at(t, 8, 59)))
	// Start inclusive.
	require.True(t, w.InPeakHours(at(t, 9, 0)))
	// Mid window.
	require.True(t, w.InPeakHours(at(t, 12, 30)))
	// End exclusive.
	require.False(t, w.InPeakHours(at(t, 18, 0)))
	// Well after.
	require.False(t, w.InPeakHours(at(t, 23, 0)))
}

func TestPeakHoursWindow_InPeakHours_OvernightWrap(t *testing.T) {
	t.Parallel()
	w := PeakHoursWindow{Start: "22:00", End: "06:00"}
	// Before start (daytime).
	require.False(t, w.InPeakHours(at(t, 12, 0)))
	// Start inclusive.
	require.True(t, w.InPeakHours(at(t, 22, 0)))
	// Late night, pre-midnight leg.
	require.True(t, w.InPeakHours(at(t, 23, 59)))
	// Post-midnight leg.
	require.True(t, w.InPeakHours(at(t, 0, 0)))
	require.True(t, w.InPeakHours(at(t, 3, 30)))
	// End exclusive.
	require.False(t, w.InPeakHours(at(t, 6, 0)))
	// After end, before start.
	require.False(t, w.InPeakHours(at(t, 10, 0)))
}

func TestPeakHoursWindow_InPeakHours_ZeroAndEqual(t *testing.T) {
	t.Parallel()
	require.False(t, PeakHoursWindow{}.InPeakHours(at(t, 12, 0)), "empty window = feature off")
	// start == end: zero-length window never matches.
	require.False(t, PeakHoursWindow{Start: "12:00", End: "12:00"}.InPeakHours(at(t, 12, 0)))
}

func TestParseHHMM_NonDigitBytesRejected(t *testing.T) {
	t.Parallel()
	// Regression: parseHHMM used to do raw `s[i]-'0'` arithmetic without
	// checking each byte was an ASCII digit. Bytes in the ':' (0x3A)–'C'
	// (0x43) range silently mapped to 10–19 and slipped through as a
	// plausible-but-wrong time (e.g. "09:0;" → 551, no error).
	bad := []string{
		"09:0;", // ';' (0x3B) - '0' = 11 → minutes 11, the original bug
		"0::00", // ':' (0x3A) in an hour slot
		"09:C0", // 'C' (0x43) - '0' = 19
		"09:0:", // ':' in a minute slot
		"1A:00", // 'A' (0x41) in an hour slot
	}
	for _, s := range bad {
		t.Run(s, func(t *testing.T) {
			t.Parallel()
			got, err := parseHHMM(s)
			require.Error(t, err, "%q must be rejected, got %d", s, got)
			require.Equal(t, 0, got)
		})
	}
	// Validate must surface the same rejection for the start/end fields.
	require.Error(t, PeakHoursWindow{Start: "09:0;", End: "18:00"}.Validate())
	require.Error(t, PeakHoursWindow{Start: "09:00", End: "18:C0"}.Validate())

	// Sanity: a well-formed window is still accepted.
	require.NoError(t, PeakHoursWindow{Start: "09:00", End: "18:00"}.Validate())
}

func TestPeakHoursWindow_EndTimeTodayPreservesLocalWallClockAcrossDST(t *testing.T) {
	t.Parallel()

	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)

	// On the spring-forward day, adding absolute minutes to local midnight
	// lands at 21:00 rather than the configured 20:00 local wall time.
	now := time.Date(2026, time.March, 8, 12, 0, 0, 0, loc)
	window := PeakHoursWindow{Start: "10:00", End: "20:00"}
	got := window.EndTimeToday(now)
	require.Equal(t, 20, got.Hour())
	require.Equal(t, 0, got.Minute())
	require.Equal(t, loc, got.Location())
}

func TestPeakHoursWindow_EndTimeToday(t *testing.T) {
	t.Parallel()
	// Normal window: end is today at End.
	w := PeakHoursWindow{Start: "09:00", End: "18:00"}
	end := w.EndTimeToday(at(t, 12, 0))
	require.Equal(t, 18, end.Hour())
	require.Equal(t, 0, end.Minute())
	require.True(t, end.After(at(t, 12, 0)))

	// Overnight, pre-midnight leg (22:00 now): end is tomorrow 06:00.
	on := PeakHoursWindow{Start: "22:00", End: "06:00"}
	end = on.EndTimeToday(at(t, 23, 0))
	require.Equal(t, 6, end.Hour())
	require.True(t, end.After(at(t, 23, 0)), "end must be after now")
	require.True(t, end.Sub(at(t, 23, 0)) > 6*time.Hour, "overnight end should be ~7h ahead, got %v", end.Sub(at(t, 23, 0)))

	// Overnight, post-midnight leg (03:00 now): end is today 06:00.
	end = on.EndTimeToday(at(t, 3, 0))
	require.Equal(t, 6, end.Hour())
	require.True(t, end.After(at(t, 3, 0)))
}
