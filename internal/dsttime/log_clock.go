package dsttime

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var logClockLine = regexp.MustCompile(`^\[(\d{2,}):([0-5]\d):([0-5]\d)\]:\s?(.*)$`)

// LogClock must see every line in a generation: DST wraps HH after 24 hours.
// A tail or an unanchored fragment cannot yield a trustworthy wall clock.
type LogClock struct {
	started  time.Time
	previous int64
	days     int64
}

func (c *LogClock) ReadLine(line string) (time.Time, string) {
	match := logClockLine.FindStringSubmatch(line)
	if match == nil {
		return time.Time{}, ""
	}
	payload := strings.TrimSpace(match[4])
	if payload == "Starting Up" {
		*c = LogClock{}
	}
	if strings.HasPrefix(payload, "Current time:") {
		if start, ok := FindStartTime(payload); ok {
			*c = LogClock{started: start}
		}
	}
	hours, err := strconv.ParseInt(match[1], 10, 32)
	if err != nil || hours > 1000000 {
		return time.Time{}, payload
	}
	minutes, _ := strconv.ParseInt(match[2], 10, 64)
	seconds, _ := strconv.ParseInt(match[3], 10, 64)
	elapsed := hours*3600 + minutes*60 + seconds
	if hours < 24 && c.previous-elapsed > 12*3600 {
		c.days++
	}
	c.previous = elapsed
	if c.started.IsZero() {
		return time.Time{}, payload
	}
	return c.started.Add(time.Duration(c.days*86400+elapsed) * time.Second).UTC(), payload
}
