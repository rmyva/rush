package config

import (
	"errors"
	"fmt"
	"time"
)

// PeakHoursWindow is a single local-time window during which a provider
// is refused. Start and End are "HH:MM" in the computer's local time.
// An overnight window (Start > End, e.g. "22:00"–"06:00") wraps past
// midnight. There is no weekday mask and no timezone field: the feature
// is intentionally local-clock only.
type PeakHoursWindow struct {
	Start string `json:"start" jsonschema:"description=Window start in local time HH:MM,example=09:00"`
	End   string `json:"end"   jsonschema:"description=Window end in local time HH:MM,example=18:00"`
	// Message is an optional operator-authored note appended (after a
	// blank-line separator) to the peak-hours refusal guidance shown when
	// rush exits because of this window. Empty by default; purely
	// cosmetic, not validated.
	Message string `json:"message,omitempty" jsonschema:"description=Optional custom text appended to the peak-hours exit message"`
}

// Validate parses the window's HH:MM strings. A malformed string is a
// hard error — callers must surface it at config-load time, not swallow
// it. An empty window (both fields unset) is valid and means "feature
// off".
func (w PeakHoursWindow) Validate() error {
	if w.Start == "" && w.End == "" {
		return nil
	}
	if w.Start == "" || w.End == "" {
		return errors.New("peak_hours: both start and end must be set")
	}
	if _, err := parseHHMM(w.Start); err != nil {
		return fmt.Errorf("peak_hours start: %w", err)
	}
	if _, err := parseHHMM(w.End); err != nil {
		return fmt.Errorf("peak_hours end: %w", err)
	}
	return nil
}

// InPeakHours reports whether now falls inside the window in local time.
// Start is inclusive, End is exclusive. An overnight window (Start > End)
// wraps past midnight. Returns false for a zero-valued window (feature off).
func (w PeakHoursWindow) InPeakHours(now time.Time) bool {
	if w.Start == "" || w.End == "" {
		return false
	}
	startM, err := parseHHMM(w.Start)
	if err != nil {
		return false
	}
	endM, err := parseHHMM(w.End)
	if err != nil {
		return false
	}
	cur := now.Hour()*60 + now.Minute()
	if startM == endM {
		// Zero-length window never matches.
		return false
	}
	if startM < endM {
		return cur >= startM && cur < endM
	}
	// Overnight wrap: [start, 24:00) ∪ [00:00, end).
	return cur >= startM || cur < endM
}

// EndTimeToday returns the local time.Time today at which the window
// ends — i.e. the moment the provider becomes available again. For an
// overnight window where now is before End (in the post-midnight leg),
// the end is still today at End; when now is in the pre-midnight leg
// (>= Start), the end is tomorrow at End. Used for error messaging.
func (w PeakHoursWindow) EndTimeToday(now time.Time) time.Time {
	endM, _ := parseHHMM(w.End)
	y, m, d := now.Date()
	end := time.Date(y, m, d, endM/60, endM%60, 0, 0, now.Location())
	startM, _ := parseHHMM(w.Start)
	if startM > endM && now.Hour()*60+now.Minute() >= startM {
		// Overnight, pre-midnight leg: end is tomorrow.
		end = end.AddDate(0, 0, 1)
	}
	if !end.After(now) {
		// Defensive: if the computed end is not after now (e.g. clock
		// skew, zero-length window), push it forward a day so the
		// message always names a future moment.
		end = end.AddDate(0, 0, 1)
	}
	return end
}

// parseHHMM parses a "HH:MM" string into minutes-since-midnight. The
// hours component must be 00–23 and minutes 00–59; leading zeros are
// required (matches the jsonschema examples and avoids ambiguity with
// 12-hour clocks).
func parseHHMM(s string) (int, error) {
	if len(s) != 5 || s[2] != ':' {
		return 0, fmt.Errorf("invalid time %q: expected HH:MM", s)
	}
	// Explicit digit check: bytes in the range ':' (0x3A)–'C' (0x43) would
	// otherwise slip through as 10–19 via the raw `c - '0'` arithmetic
	// below, producing a plausible-but-wrong time (e.g. "09:0;" → 551).
	for _, i := range [4]int{0, 1, 3, 4} {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("invalid time %q: expected HH:MM", s)
		}
	}
	h := int(s[0]-'0')*10 + int(s[1]-'0')
	mi := int(s[3]-'0')*10 + int(s[4]-'0')
	if h < 0 || h > 23 || mi < 0 || mi > 59 {
		return 0, fmt.Errorf("invalid time %q: out of range", s)
	}
	return h*60 + mi, nil
}
