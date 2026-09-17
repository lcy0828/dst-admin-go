package gameinstall

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"dont/internal/operationprogress"
)

var steamProgress = regexp.MustCompile(`(?i)Update state \([^)]*\)\s*([^,]+),\s*progress:\s*([0-9.]+)\s*\(([0-9]+)\s*/\s*([0-9]+)\)`)
var steamANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// SteamCMD writes both CR-delimited updates and ordinary lines, sometimes split
// across writes. Keep only a bounded tail and emit at most one update per second.
type installOutput struct {
	mu        sync.Mutex
	ctx       context.Context
	now       func() time.Time
	tail      limitedOutput
	pending   string
	stage     string
	percent   int
	lastAt    time.Time
	lastBytes int64
}

func (w *installOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.tail.Write(p)
	w.pending += string(p)
	for {
		index := strings.IndexAny(w.pending, "\r\n")
		if index < 0 {
			break
		}
		line := w.pending[:index]
		w.pending = w.pending[index+1:]
		w.line(line)
	}
	if len(w.pending) > 8192 {
		w.pending = w.pending[len(w.pending)-8192:]
	}
	return len(p), nil
}

func (w *installOutput) line(line string) {
	line = strings.TrimSpace(steamANSI.ReplaceAllString(line, ""))
	if line == "" {
		return
	}
	now := w.now()
	match := steamProgress.FindStringSubmatch(line)
	stage := "game.install"
	var current, total, rate int64
	percent := w.percent
	if len(match) != 0 {
		current, _ = strconv.ParseInt(match[3], 10, 64)
		total, _ = strconv.ParseInt(match[4], 10, 64)
		if current < 0 || total <= 0 || current > total {
			return
		}
		switch {
		case strings.Contains(strings.ToLower(match[1]), "download"):
			stage = "game.download"
		case strings.Contains(strings.ToLower(match[1]), "validat"), strings.Contains(strings.ToLower(match[1]), "verif"):
			stage = "game.validate"
		default:
			stage = "game.prepare"
		}
		// Overall job progress stays monotonic; the UI uses byte counts for
		// the exact per-stage percentage, including validation before download.
		percent = max(percent, min(95, int(float64(current)/float64(total)*90)))
		if stage == "game.download" && stage == w.stage && current >= w.lastBytes && !w.lastAt.IsZero() {
			if seconds := now.Sub(w.lastAt).Seconds(); seconds > 0 {
				rate = int64(float64(current-w.lastBytes) / seconds)
			}
		}
	}
	if stage == w.stage && now.Sub(w.lastAt) < time.Second {
		return
	}
	w.stage, w.lastAt, w.lastBytes, w.percent = stage, now, current, percent
	if len(line) > 900 {
		line = line[:900]
	}
	operationprogress.Report(w.ctx, operationprogress.Update{Stage: stage, Percent: percent, Message: line, CurrentBytes: current, TotalBytes: total, BytesPerSecond: rate})
}

func (w *installOutput) finish() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.line(w.pending)
	w.pending = ""
	return w.tail.text
}
