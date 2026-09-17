package requesttiming

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type contextKey struct{}

type scope struct {
	recorder *Recorder
	prefix   string
}

// Recorder is owned by one request. Nested and parallel measurements overlap;
// their durations must not be added to calculate the request's elapsed time.
type Recorder struct {
	mu      sync.Mutex
	started time.Time
	metrics map[string]Metric
}

type Metric struct {
	Name       string  `json:"name"`
	DurationMS float64 `json:"duration_ms"`
	Calls      int     `json:"calls"`
}

func New(ctx context.Context) (context.Context, *Recorder) {
	recorder := &Recorder{started: time.Now(), metrics: make(map[string]Metric)}
	return context.WithValue(ctx, contextKey{}, scope{recorder: recorder}), recorder
}

func Scope(ctx context.Context, name string) context.Context {
	value, ok := ctx.Value(contextKey{}).(scope)
	if !ok {
		return ctx
	}
	value.prefix += name + "."
	return context.WithValue(ctx, contextKey{}, value)
}

// Start is inactive unless the HTTP handler explicitly enabled request timing.
func Start(ctx context.Context, name string) func() {
	value, ok := ctx.Value(contextKey{}).(scope)
	if !ok {
		return func() {}
	}
	name = value.prefix + name
	if !validName(name) {
		return func() {}
	}
	started := time.Now()
	return func() {
		elapsed := time.Since(started)
		value.recorder.mu.Lock()
		defer value.recorder.mu.Unlock()
		metric, exists := value.recorder.metrics[name]
		if !exists && len(value.recorder.metrics) >= 128 {
			return
		}
		metric.Name = name
		metric.DurationMS += float64(elapsed) / float64(time.Millisecond)
		metric.Calls++
		value.recorder.metrics[name] = metric
	}
}

func (r *Recorder) Snapshot() (time.Duration, []Metric) {
	elapsed := time.Since(r.started)
	r.mu.Lock()
	defer r.mu.Unlock()
	metrics := make([]Metric, 0, len(r.metrics))
	for _, metric := range r.metrics {
		metrics = append(metrics, metric)
	}
	sort.Slice(metrics, func(i, j int) bool { return metrics[i].Name < metrics[j].Name })
	return elapsed, metrics
}

func Header(elapsed time.Duration, metrics []Metric) string {
	var value strings.Builder
	fmt.Fprintf(&value, "handler;dur=%.3f", float64(elapsed)/float64(time.Millisecond))
	for _, metric := range metrics {
		if !validName(metric.Name) {
			continue
		}
		part := fmt.Sprintf(", %s;dur=%.3f;desc=\"%d calls\"", metric.Name, metric.DurationMS, metric.Calls)
		if value.Len()+len(part) > 6000 {
			break
		}
		value.WriteString(part)
	}
	return value.String()
}

func validName(value string) bool {
	if value == "" || len(value) > 160 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.') {
			return false
		}
	}
	return true
}
