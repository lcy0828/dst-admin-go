package steamupdate

import (
	"context"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"dont/internal/operationprogress"
)

var steamProgress = regexp.MustCompile(`(?i)Update state \([^)]*\)\s*([^,]+),\s*progress:\s*([0-9.]+)\s*\(([0-9]+)\s*/\s*([0-9]+)\)`)
var steamANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
var steamAttempt = regexp.MustCompile(`^\[DST Admin\] SteamCMD app=[0-9]+ validate=true attempt=([12])/2$`)

// ProgressWriter forwards the original log to its bounded caller-owned sink.
// Parsing retains at most one 8 KiB line and reports once per second, except
// for phase/attempt changes and a phase's final byte count. It never reports
// installation success: callers must still verify the installed files/version.
type ProgressWriter struct {
	mu                       sync.Mutex
	ctx                      context.Context
	out                      io.Writer
	now                      func() time.Time
	pending                  string
	stage                    string
	attempt                  int
	percent                  int
	lastAt                   time.Time
	lastBytes, lastTotal     int64
	sampleAt                 time.Time
	sampleBytes, sampleTotal int64
	sampleStage              string
	sampleAttempt            int
}

func NewProgressWriter(ctx context.Context, out io.Writer) *ProgressWriter {
	return &ProgressWriter{ctx: ctx, out: out, now: time.Now, attempt: 1}
}

func (w *ProgressWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.out.Write(p)
	// Bound temporary allocations too, even if a custom runner writes a huge
	// unterminated line in one call.
	for data := p[:n]; len(data) > 0; {
		end := min(len(data), 8192)
		w.pending += string(data[:end])
		data = data[end:]
		for {
			i := strings.IndexAny(w.pending, "\r\n")
			if i < 0 {
				break
			}
			w.line(w.pending[:i])
			w.pending = w.pending[i+1:]
		}
		if len(w.pending) > 8192 {
			w.pending = w.pending[len(w.pending)-8192:]
		}
	}
	return n, err
}

func (w *ProgressWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.line(w.pending)
	w.pending = ""
}

func (w *ProgressWriter) line(line string) {
	line = strings.TrimSpace(steamANSI.ReplaceAllString(line, ""))
	if line == "" {
		return
	}
	now := w.now()
	stage, attempt := w.stage, w.attempt
	if stage == "" {
		stage = "game.prepare"
	}
	current, total, rate := w.lastBytes, w.lastTotal, int64(0)
	measured := false
	if m := steamAttempt.FindStringSubmatch(line); m != nil {
		attempt, _ = strconv.Atoi(m[1])
		stage, current, total = "game.prepare", 0, 0
	} else if m := steamProgress.FindStringSubmatch(line); m != nil {
		measured = true
		var e1, e2 error
		current, e1 = strconv.ParseInt(m[3], 10, 64)
		total, e2 = strconv.ParseInt(m[4], 10, 64)
		if e1 != nil || e2 != nil || total <= 0 || current < 0 || current > total {
			return
		}
		switch label := strings.ToLower(m[1]); {
		case strings.Contains(label, "download"):
			stage = "game.download"
		case strings.Contains(label, "validat"), strings.Contains(label, "verif"):
			stage = "game.validate"
		default:
			stage = "game.prepare"
		}
		w.percent = max(w.percent, min(95, int(float64(current)/float64(total)*90)))
		if stage == "game.download" && stage == w.sampleStage && attempt == w.sampleAttempt && total == w.sampleTotal && current >= w.sampleBytes && !w.sampleAt.IsZero() {
			if seconds := now.Sub(w.sampleAt).Seconds(); seconds > 0 {
				rate = int64(float64(current-w.sampleBytes) / seconds)
			}
		}
	} else if strings.HasPrefix(line, "Success! App '") && strings.Contains(line, "fully installed") {
		stage, current, total = "game.finalize", 0, 0
	}
	complete := total > 0 && current == total && (w.lastBytes != current || w.lastTotal != total)
	if stage == w.stage && attempt == w.attempt && now.Sub(w.lastAt) < time.Second && !complete {
		return
	}
	// Ordinary Steam log lines are output activity, not new byte samples.
	// Using their timestamps would overstate throughput on the next sample.
	if measured {
		w.sampleAt, w.sampleBytes, w.sampleTotal, w.sampleStage, w.sampleAttempt = now, current, total, stage, attempt
	} else if stage != w.stage || attempt != w.attempt {
		w.sampleAt = time.Time{}
	}
	w.stage, w.attempt, w.lastAt, w.lastBytes, w.lastTotal = stage, attempt, now, current, total
	if len(line) > 900 {
		line = line[:900]
	}
	operationprogress.Report(w.ctx, operationprogress.Update{Stage: stage, Attempt: attempt, Percent: w.percent, Message: line, CurrentBytes: current, TotalBytes: total, BytesPerSecond: rate})
}
