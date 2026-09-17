package dsttime

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const wallClockLayout = "Mon Jan 2 15:04:05 2006"

var relativeClockPattern = regexp.MustCompile(`^(\d{2,}):([0-5]\d):([0-5]\d)$`)
var startTimeLinePattern = regexp.MustCompile(`Current time:\s+([A-Za-z]+\s+[A-Za-z]+\s+\d{1,2}\s+\d{2}:\d{2}:\d{2}\s+\d{4})`)

// ParseStartTime parses DST's timezone-less Current time wall clock in the
// local timezone used by the DST process.
func ParseStartTime(value string) (time.Time, error) {
	return time.ParseInLocation(wallClockLayout, strings.TrimSpace(value), time.Local)
}

// FindStartTime extracts the latest startup wall clock from a log fragment.
// It returns false when the fragment is a tail that does not contain startup.
func FindStartTime(content string) (time.Time, bool) {
	matches := startTimeLinePattern.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return time.Time{}, false
	}
	value, err := ParseStartTime(matches[len(matches)-1][1])
	return value, err == nil
}

// ResolveTimestamp turns DST's process-relative [HH:MM:SS] value into a real
// timestamp anchored at the process startup time.
func ResolveTimestamp(start time.Time, relative string) (time.Time, error) {
	if start.IsZero() {
		return time.Time{}, fmt.Errorf("DST startup time is not available")
	}
	relative = strings.TrimSpace(relative)
	matches := relativeClockPattern.FindStringSubmatch(relative)
	if len(matches) != 4 {
		return time.Time{}, fmt.Errorf("invalid DST relative time %q", relative)
	}
	hours, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid DST relative time %q: %w", relative, err)
	}
	minutes, _ := strconv.Atoi(matches[2])
	seconds, _ := strconv.Atoi(matches[3])
	const maxDurationSeconds = int64((1<<63 - 1) / int64(time.Second))
	if hours > maxDurationSeconds/3600 {
		return time.Time{}, fmt.Errorf("invalid DST relative time %q", relative)
	}
	totalSeconds := hours*3600 + int64(minutes*60+seconds)
	if totalSeconds > maxDurationSeconds {
		return time.Time{}, fmt.Errorf("invalid DST relative time %q", relative)
	}
	elapsed := time.Duration(totalSeconds) * time.Second
	return start.Add(elapsed), nil
}
