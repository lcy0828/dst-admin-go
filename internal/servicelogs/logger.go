package servicelogs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"dont/shared"
	"github.com/google/uuid"
)

var defaultMu sync.RWMutex
var defaultStore *Store
var defaultSink func(shared.ServiceLogEntry)
var defaultConfig Config
var debugMu sync.Mutex
var debugRoot string
var debugCheckAt, debugUntil time.Time
var legacyTime = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2}(?:\.\d+)? `)
var legacyIDs = regexp.MustCompile(`(?:^|\s)(request_id|job_id|operation_id|room_id|world_id)=([^\s]+)`)
var legacyLevelPrefix = regexp.MustCompile(`(?i)^(DEBUG|INFO|WARN(?:ING)?|ERROR|FATAL|PANIC|调试|信息|警告|错误)(?:\s|:|：|$)`)

// Old launchers supervise updates but do not persist child output. A child
// must retain its own writer unless the parent explicitly advertises capture.
func CapturedByLauncher() bool {
	return os.Getenv("DST_ADMIN_SUPERVISED") == "1" && os.Getenv("DST_ADMIN_SERVICE_LOG_CAPTURE") == "1"
}

// Start runs before configuration validation. Metadata-only CLI commands do
// not call it. Supervised children emit structured lines; only the parent owns
// persistence and rotation.
func Start(config Config, captured bool) func() {
	if config.BootID == "" {
		config.BootID = os.Getenv("DST_ADMIN_BOOT_ID")
	}
	if config.BootID == "" {
		config.BootID = uuid.NewString()
	}
	if config.Console == nil {
		config.Console = os.Stderr
	}
	previous, flags := log.Writer(), log.Flags()
	var store *Store
	var sink func(shared.ServiceLogEntry)
	if captured {
		var mu sync.Mutex
		sink = func(e shared.ServiceLogEntry) {
			e = normalize(e, config)
			data, _ := json.Marshal(e)
			mu.Lock()
			defer mu.Unlock()
			_, _ = os.Stdout.Write(append(data, '\n'))
		}
	} else {
		var err error
		store, err = Open(config)
		if err != nil {
			fmt.Fprintf(config.Console, "[ServiceLogs] persistence unavailable: %s\n", Redact(err.Error()))
			sink = func(e shared.ServiceLogEntry) {
				e = normalize(e, config)
				fmt.Fprintf(config.Console, "%s [%s] %s%s\n", e.Time.Format(time.RFC3339), e.Level, e.Message, correlationText(e))
			}
		} else {
			sink = store.Append
		}
	}
	defaultMu.Lock()
	defaultConfig = config
	defaultStore = store
	defaultSink = sink
	defaultMu.Unlock()
	log.SetOutput(standardWriter{})
	log.SetFlags(log.LstdFlags)
	Emit(shared.ServiceLogEntry{Level: "info", Code: "SERVICE_STARTING", Message: "Service process starting"})
	return func() {
		Emit(shared.ServiceLogEntry{Level: "info", Code: "SERVICE_STOPPED", Message: "Service process stopped"})
		closeDefault()
		log.SetOutput(previous)
		log.SetFlags(flags)
		defaultMu.Lock()
		defaultStore = nil
		defaultSink = nil
		defaultConfig = Config{}
		defaultMu.Unlock()
	}
}
func CurrentRoot() string { defaultMu.RLock(); defer defaultMu.RUnlock(); return defaultConfig.Root }

// Debug checks are demand-driven and cached; normal requests never read the
// settings file more than once a second, and idle services add no polling.
func DebugEnabled() bool {
	root := CurrentRoot()
	if root == "" {
		return false
	}
	debugMu.Lock()
	defer debugMu.Unlock()
	now := time.Now()
	if root != debugRoot || !now.Before(debugCheckAt) {
		settings := ReadSettings(root)
		debugRoot, debugCheckAt, debugUntil = root, now.Add(time.Second), time.Time{}
		if settings.Enabled && settings.DebugUntil != nil {
			debugUntil = *settings.DebugUntil
		}
	}
	return now.Before(debugUntil)
}
func Emit(e shared.ServiceLogEntry) {
	defaultMu.RLock()
	sink, config := defaultSink, defaultConfig
	defaultMu.RUnlock()
	if sink != nil {
		if e.Time.IsZero() {
			e.Time = time.Now().UTC()
		}
		sink(e)
	} else {
		e = normalize(e, config)
		log.Print(e.Message + correlationText(e))
	}
}
func closeDefault() {
	defaultMu.RLock()
	store := defaultStore
	defaultMu.RUnlock()
	if store != nil {
		done := make(chan struct{})
		go func() { store.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	}
}
func Fatalf(format string, values ...interface{}) {
	Emit(shared.ServiceLogEntry{Level: "fatal", Message: fmt.Sprintf(format, values...)})
	closeDefault()
	os.Exit(1)
}
func Fatal(err error) { Fatalf("%v", err) }

type standardWriter struct{}

func (standardWriter) Write(p []byte) (int, error) {
	e := legacyEntry(string(p))
	Emit(e)
	return len(p), nil
}
func legacyEntry(raw string) shared.ServiceLogEntry {
	raw = strings.TrimSpace(legacyTime.ReplaceAllString(raw, ""))
	e := shared.ServiceLogEntry{Level: legacyLevel(raw), Message: raw}
	for _, m := range legacyIDs.FindAllStringSubmatch(raw, -1) {
		switch m[1] {
		case "request_id":
			e.RequestID = m[2]
		case "job_id":
			e.JobID = m[2]
		case "operation_id":
			e.OperationID = m[2]
		case "room_id":
			e.RoomID = m[2]
		case "world_id":
			e.WorldID = m[2]
		}
	}
	return e
}

func legacyLevel(message string) string {
	// Only leading labels describe severity. A command, path or quoted output
	// may contain ERROR/WARN/DEBUG without reporting a failed operation.
	head := message
	level := ""
	for strings.HasPrefix(head, "[") {
		end := strings.IndexByte(head, ']')
		if end < 0 {
			break
		}
		label := strings.Fields(head[1:end])
		if len(label) > 0 {
			if explicit := legacyLevelLabel(label[len(label)-1]); explicit != "" {
				level = explicit
			}
		}
		head = strings.TrimSpace(head[end+1:])
	}
	if level != "" {
		return level
	}
	if label := legacyLevelPrefix.FindStringSubmatch(head); label != nil {
		return legacyLevelLabel(label[1])
	}
	// Older callers report an unlabelled failure before the first field
	// separator. Values after it must never promote a successful log entry.
	if end := strings.IndexAny(head, ":：,，\r\n"); end >= 0 {
		head = head[:end]
	}
	head = strings.TrimSpace(head)
	if strings.HasSuffix(head, "失败") || strings.HasSuffix(head, "错误") {
		return "error"
	}
	return "info"
}

func legacyLevelLabel(label string) string {
	switch strings.ToLower(label) {
	case "debug", "调试":
		return "debug"
	case "info", "信息":
		return "info"
	case "warn", "warning", "警告":
		return "warn"
	case "error", "panic", "错误":
		return "error"
	case "fatal":
		return "fatal"
	default:
		return ""
	}
}

// ChildWriter accepts raw stdout/stderr and structured application events. Its
// partial-line buffer is bounded even when a child writes an unbroken stream.
type ChildWriter struct {
	mu                       sync.Mutex
	pending                  []byte
	component, version, boot string
	store                    *Store
	fallback                 io.Writer
}

func NewChildWriter(component, version, boot string) *ChildWriter {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	return &ChildWriter{component: component, version: version, boot: boot, store: defaultStore, fallback: os.Stderr}
}
func (w *ChildWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		cut := bytes.IndexByte(p, '\n')
		if cut < 0 {
			cut = len(p)
		}
		part := p[:cut]
		space := maxEntryBytes - len(w.pending)
		if len(part) > space {
			w.pending = append(w.pending, part[:space]...)
			w.publish(true)
			p = p[space:]
			continue
		}
		w.pending = append(w.pending, part...)
		p = p[cut:]
		if len(p) > 0 {
			p = p[1:]
			w.publish(false)
		}
	}
	return n, nil
}
func (w *ChildWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) > 0 {
		w.publish(false)
	}
}
func (w *ChildWriter) publish(truncated bool) {
	if len(w.pending) == 0 {
		return
	}
	var e shared.ServiceLogEntry
	if json.Unmarshal(w.pending, &e) != nil || e.Message == "" {
		e = legacyEntry(string(w.pending))
	}
	e.Component = w.component
	e.Version = w.version
	e.BootID = w.boot
	e.ID = ""
	e.Truncated = e.Truncated || truncated
	w.pending = nil
	if w.store != nil {
		w.store.Append(e)
	} else {
		e = normalize(e, Config{})
		fmt.Fprintln(w.fallback, Redact(e.Message))
	}
}
