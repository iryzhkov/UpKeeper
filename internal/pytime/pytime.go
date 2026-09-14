// Package pytime formats timestamps the way the Python implementation did.
//
// Every durable record and every report carries a timestamp produced by
// datetime.isoformat() with the "+00:00" suffix replaced by "Z". Python omits
// the fractional part entirely when the microsecond field is zero, so a naive
// Go layout string is not equivalent.
package pytime

import (
	"fmt"
	"time"
)

// ISO8601Z renders t in UTC exactly as Python's
// datetime.isoformat().replace("+00:00", "Z") would.
func ISO8601Z(t time.Time) string {
	u := t.UTC()
	micros := u.Nanosecond() / 1000
	base := u.Format("2006-01-02T15:04:05")
	if micros == 0 {
		return base + "Z"
	}
	return fmt.Sprintf("%s.%06dZ", base, micros)
}

// ParseISO8601Z reads a timestamp written by ISO8601Z or by Python's
// datetime.fromisoformat, accepting both the "Z" and "+00:00" spellings.
func ParseISO8601Z(value string) (time.Time, error) {
	for _, layout := range []string{
		"2006-01-02T15:04:05.999999Z07:00",
		"2006-01-02T15:04:05Z07:00",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("not an ISO-8601 UTC timestamp: %q", value)
}

// RunIDStamp renders the timestamp half of a run identifier.
func RunIDStamp(t time.Time) string {
	return t.UTC().Format("20060102T150405Z")
}
