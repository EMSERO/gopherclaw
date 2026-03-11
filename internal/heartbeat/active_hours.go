package heartbeat

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// IsWithinActiveHours checks if the given time falls within the [start, end)
// window. Times are in HH:MM format (24h). "24:00" is allowed for end.
// If start == end, returns false (zero-width window).
// If end < start, the window wraps across midnight.
func IsWithinActiveHours(start, end, timezone string, now time.Time) bool {
	startMin, err := parseHHMM(start, false)
	if err != nil {
		return true // invalid config — allow
	}
	endMin, err := parseHHMM(end, true)
	if err != nil {
		return true
	}

	if startMin == endMin {
		return false // zero-width window
	}

	// Resolve timezone.
	loc, err := resolveTimezone(timezone)
	if err != nil {
		loc = time.UTC
	}

	nowInTZ := now.In(loc)
	currentMin := nowInTZ.Hour()*60 + nowInTZ.Minute()

	if endMin > startMin {
		// Normal window: 09:00–22:00
		return currentMin >= startMin && currentMin < endMin
	}
	// Wrapped window: 22:00–09:00 (spans midnight)
	return currentMin >= startMin || currentMin < endMin
}

// parseHHMM parses "HH:MM" to minutes since midnight.
// allow24 permits "24:00" (returns 1440).
func parseHHMM(s string, allow24 bool) (int, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid time format: %q", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, err
	}

	maxH := 23
	if allow24 && m == 0 {
		maxH = 24
	}
	if h < 0 || h > maxH || m < 0 || m > 59 {
		return 0, fmt.Errorf("time out of range: %q", s)
	}
	return h*60 + m, nil
}

// resolveTimezone resolves a timezone string to a *time.Location.
// Accepts "local", "", or an IANA identifier.
func resolveTimezone(tz string) (*time.Location, error) {
	switch tz {
	case "", "local":
		return time.Local, nil
	default:
		return time.LoadLocation(tz)
	}
}
