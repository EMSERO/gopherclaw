package heartbeat

import (
	"testing"
	"time"
)

func FuzzParseHHMM(f *testing.F) {
	// Valid times.
	f.Add("00:00", false)
	f.Add("09:30", false)
	f.Add("23:59", false)
	f.Add("12:00", true)
	f.Add("24:00", true)  // allowed when allow24=true
	f.Add("24:00", false) // rejected when allow24=false

	// Edge cases.
	f.Add("0:0", false)
	f.Add("9:5", false)
	f.Add("00:60", false) // invalid minute
	f.Add("25:00", false) // invalid hour

	// Missing colon.
	f.Add("1234", false)
	f.Add("", false)

	// Non-numeric.
	f.Add("ab:cd", false)
	f.Add("12:xx", false)
	f.Add("xx:30", false)

	// Negative values.
	f.Add("-1:30", false)
	f.Add("12:-5", false)

	// Extra colons.
	f.Add("12:30:00", false)

	// Whitespace.
	f.Add(" 12:30", false)
	f.Add("12:30 ", false)

	// Large numbers.
	f.Add("999:999", false)
	f.Add("2147483647:0", false)

	f.Fuzz(func(t *testing.T, s string, allow24 bool) {
		mins, err := parseHHMM(s, allow24)
		if err != nil {
			// Error is expected for most random inputs.
			return
		}
		// If parsing succeeded, validate the result.
		maxMins := 23*60 + 59
		if allow24 {
			maxMins = 24 * 60
		}
		if mins < 0 {
			t.Fatalf("parseHHMM(%q, %v) = %d, must be >= 0", s, allow24, mins)
		}
		if mins > maxMins {
			t.Fatalf("parseHHMM(%q, %v) = %d, must be <= %d", s, allow24, mins, maxMins)
		}
	})
}

func FuzzIsWithinActiveHours(f *testing.F) {
	now := time.Date(2026, 3, 9, 14, 30, 0, 0, time.UTC)
	refTime := now.Unix()

	// Normal window, inside.
	f.Add("09:00", "22:00", "UTC", refTime)
	// Normal window, outside.
	f.Add("15:00", "22:00", "UTC", refTime)
	// Wrapped window (spans midnight).
	f.Add("22:00", "06:00", "UTC", refTime)
	// Zero-width window.
	f.Add("14:00", "14:00", "UTC", refTime)
	// End at 24:00.
	f.Add("09:00", "24:00", "UTC", refTime)
	// Invalid start.
	f.Add("bad", "22:00", "UTC", refTime)
	// Invalid end.
	f.Add("09:00", "bad", "UTC", refTime)
	// Invalid timezone.
	f.Add("09:00", "22:00", "Invalid/TZ", refTime)
	// Empty timezone.
	f.Add("09:00", "22:00", "", refTime)
	// Local timezone.
	f.Add("00:00", "24:00", "local", refTime)
	// US timezone.
	f.Add("09:00", "17:00", "America/New_York", refTime)

	f.Fuzz(func(t *testing.T, start, end, timezone string, unixSec int64) {
		// Clamp to reasonable range to avoid time.Unix panics on extreme values.
		if unixSec < -62135596800 || unixSec > 253402300799 {
			return
		}
		now := time.Unix(unixSec, 0)
		// Must not panic. Return value is either true or false.
		_ = IsWithinActiveHours(start, end, timezone, now)
	})
}

func FuzzResolveTimezone(f *testing.F) {
	f.Add("")
	f.Add("local")
	f.Add("UTC")
	f.Add("America/New_York")
	f.Add("Europe/London")
	f.Add("Asia/Tokyo")
	f.Add("Invalid/Timezone")
	f.Add("US/Eastern")
	f.Add("PST")
	f.Add("EST")
	f.Add("GMT+5")
	f.Add("Etc/GMT-12")

	f.Fuzz(func(t *testing.T, tz string) {
		loc, err := resolveTimezone(tz)
		if err != nil {
			// Error is expected for invalid timezone strings.
			return
		}
		if loc == nil {
			t.Fatal("resolveTimezone returned nil location without error")
		}
	})
}
