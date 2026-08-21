package logparser

import (
	"time"

	"dont/internal/dsttime"
)

// ParseDSTStartTime parses the wall-clock value printed by DST's
// "Current time" line. DST does not include a timezone in this value, so it
// must be interpreted in the local timezone of the DST process rather than
// UTC.
func ParseDSTStartTime(value string) (time.Time, error) {
	return dsttime.ParseStartTime(value)
}

// ResolveDSTTimestamp converts DST's process-relative [HH:MM:SS] clock into
// a real timestamp. The returned value keeps the startup timestamp's location
// so JSON/database consumers can render the same wall-clock time reliably.
func ResolveDSTTimestamp(start time.Time, relative string) (time.Time, error) {
	return dsttime.ResolveTimestamp(start, relative)
}
