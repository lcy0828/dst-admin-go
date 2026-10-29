package servicelogs

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"dont/shared"
	"github.com/google/uuid"
)

const maxEntryBytes = 32 * 1024
const fileBytes = 10 * 1024 * 1024

type Config struct {
	Root, Component, Version, BootID string
	Console                          io.Writer
	FileBytes                        int64
}
type Store struct {
	config      Config
	queue       chan shared.ServiceLogEntry
	done        chan struct{}
	mu          sync.RWMutex
	health      shared.ServiceLogHealth
	savedHealth shared.ServiceLogHealth
	dropped     atomic.Uint64
	closed      bool
	closeOnce   sync.Once
	file        *os.File
	lock        *os.File
	size        int64
	fileDay     string
	settings    shared.ServiceLogSettings
	settingsAt  time.Time
	retryAt     time.Time
	noticeAt    time.Time
	repeats     map[string]*repeatEntry
}
type repeatEntry struct {
	entry shared.ServiceLogEntry
	first time.Time
	count int
}

func Open(config Config) (*Store, error) {
	if config.BootID == "" {
		config.BootID = uuid.NewString()
	}
	if config.FileBytes <= 0 {
		config.FileBytes = fileBytes
	}
	s := &Store{config: config, queue: make(chan shared.ServiceLogEntry, 512), done: make(chan struct{}), repeats: make(map[string]*repeatEntry), settings: ReadSettings(config.Root)}
	s.health.Persistence = "ready"
	if err := os.MkdirAll(config.Root, 0700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(config.Root, "writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = lockWriter(lock); err != nil {
		lock.Close()
		return nil, fmt.Errorf("service log writer is already active: %w", err)
	}
	s.lock = lock
	s.loadHealth()
	s.prune()
	go s.run()
	return s, nil
}
func (s *Store) Append(e shared.ServiceLogEntry) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return
	}
	// Bound records before queuing, so oversized legacy messages cannot make
	// the fixed-capacity queue retain an unbounded amount of memory.
	e = normalize(e, s.config)
	select {
	case s.queue <- e:
	default:
		s.dropped.Add(1)
	}
}
func (s *Store) Health() shared.ServiceLogHealth {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v := s.health
	v.Dropped += s.dropped.Load()
	return v
}
func (s *Store) Close() {
	s.closeOnce.Do(func() { s.mu.Lock(); s.closed = true; close(s.queue); s.mu.Unlock() })
	<-s.done
}
func (s *Store) run() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer close(s.done)
	defer s.lock.Close()
	healthAt := time.Now()
	pruneAt := time.Now()
	for {
		select {
		case entry, ok := <-s.queue:
			if !ok {
				s.flushRepeats(true)
				if s.file != nil {
					s.file.Sync()
					s.file.Close()
				}
				s.saveHealth()
				return
			}
			s.reloadSettings()
			if !s.settings.Enabled {
				s.setPersistence("disabled")
				s.console(entry)
				continue
			}
			if entry.Level == "debug" && (s.settings.DebugUntil == nil || s.settings.DebugUntil.Before(time.Now())) {
				continue
			}
			key := entry.Level + "\x00" + entry.Component + "\x00" + entry.RequestID + "\x00" + entry.JobID + "\x00" + entry.OperationID + "\x00" + entry.RoomID + "\x00" + entry.WorldID + "\x00" + entry.Code + "\x00" + entry.Message + "\x00" + entry.Stack
			if previous := s.repeats[key]; previous != nil && entry.Time.Sub(previous.first) < 10*time.Second && entry.Time.Sub(previous.first) >= 0 {
				previous.count++
				previous.entry.Time = entry.Time
				continue
			}
			if len(s.repeats) >= 128 {
				s.flushRepeats(true)
			}
			s.repeats[key] = &repeatEntry{entry: entry, first: entry.Time}
			s.write(entry)
		case <-ticker.C:
			s.flushRepeats(false)
			if time.Since(healthAt) >= 5*time.Second {
				s.saveHealth()
				healthAt = time.Now()
			}
			if time.Since(pruneAt) >= time.Minute {
				s.reloadSettings()
				s.prune()
				pruneAt = time.Now()
			}
		}
	}
}
func (s *Store) flushRepeats(all bool) {
	for key, value := range s.repeats {
		if all || time.Since(value.first) >= 10*time.Second {
			if value.count > 0 {
				value.entry.FirstAt = &value.first
				value.entry.Repeated = value.count
				s.write(value.entry)
			}
			delete(s.repeats, key)
		}
	}
}
func (s *Store) reloadSettings() {
	if time.Since(s.settingsAt) >= time.Second {
		s.settings = ReadSettings(s.config.Root)
		s.settingsAt = time.Now()
	}
}
func (s *Store) setPersistence(value string) {
	s.mu.Lock()
	s.health.Persistence = value
	s.mu.Unlock()
}
func (s *Store) write(e shared.ServiceLogEntry) {
	s.console(e)
	if time.Now().Before(s.retryAt) {
		s.dropped.Add(1)
		return
	}
	day := time.Now().UTC().Format("2006-01-02")
	if s.file == nil || s.size >= s.config.FileBytes || s.fileDay != day {
		if s.file != nil {
			s.file.Sync()
			s.file.Close()
			s.file = nil
		}
		name := fmt.Sprintf("service-%020d-%s.jsonl", time.Now().UnixNano(), strings.ReplaceAll(uuid.NewString(), "-", ""))
		file, err := os.OpenFile(filepath.Join(s.config.Root, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			s.failed(err)
			return
		}
		s.file = file
		s.size = 0
		s.fileDay = day
		s.prune()
	}
	e.ID = base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s@%d", filepath.Base(s.file.Name()), s.size)))
	data, err := json.Marshal(e)
	// JSON escaping can expand an otherwise bounded message sixfold. Keep the
	// serialized record within the reader and transport limit as well.
	for err == nil && len(data) >= maxEntryBytes {
		e.Truncated = true
		if len(e.Stack) > 1024 {
			e.Stack, _ = bounded(e.Stack, len(e.Stack)/2)
		} else if len(e.Message) > 1024 {
			e.Message, _ = bounded(e.Message, len(e.Message)/2)
		} else {
			break
		}
		data, err = json.Marshal(e)
	}
	if err != nil {
		s.failed(err)
		return
	}
	data = append(data, '\n')
	n, err := s.file.Write(data)
	s.size += int64(n)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	if err != nil {
		s.failed(err)
		s.file.Close()
		s.file = nil
		return
	}
	now := time.Now().UTC()
	s.mu.Lock()
	s.health.Persistence = "ready"
	s.health.LastError = ""
	s.health.LastWriteAt = &now
	s.mu.Unlock()
}
func (s *Store) console(e shared.ServiceLogEntry) {
	if s.config.Console != nil {
		fmt.Fprintf(s.config.Console, "%s [%s] %s%s\n", e.Time.Format(time.RFC3339), strings.ToUpper(e.Level), e.Message, correlationText(e))
		if e.Stack != "" {
			fmt.Fprintln(s.config.Console, e.Stack)
		}
	}
}
func (s *Store) failed(err error) {
	s.retryAt = time.Now().Add(5 * time.Second)
	s.dropped.Add(1)
	s.mu.Lock()
	s.health.Persistence = "degraded"
	s.health.LastError = Redact(err.Error())
	s.health.WriteErrors++
	s.mu.Unlock()
	if s.config.Console != nil && time.Since(s.noticeAt) >= 30*time.Second {
		s.noticeAt = time.Now()
		fmt.Fprintf(s.config.Console, "[ServiceLogs] persistence failed: %s\n", Redact(err.Error()))
	}
}
func (s *Store) saveHealth() {
	v := s.Health()
	if v == s.savedHealth {
		return
	}
	data, _ := json.Marshal(v)
	if atomicJSON(s.config.Root, "health.json", data) == nil {
		s.savedHealth = v
	}
}
func (s *Store) loadHealth() {
	data, err := os.ReadFile(filepath.Join(s.config.Root, "health.json"))
	if err == nil {
		var previous shared.ServiceLogHealth
		if json.Unmarshal(data, &previous) == nil {
			s.health.Dropped = previous.Dropped
			s.health.WriteErrors = previous.WriteErrors
		}
	}
}
func atomicJSON(root, name string, data []byte) error {
	file, err := os.CreateTemp(root, ".log-state-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(file.Name(), filepath.Join(root, name))
}
func (s *Store) prune() {
	files, err := logFiles(s.config.Root)
	if err != nil {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	var total int64
	for _, f := range files {
		total += f.Size
	}
	maximum := int64(s.settings.MaxMiB) * 1024 * 1024
	// Reserve room for the current segment before it fills, so rotation does
	// not keep an extra full segment beyond the configured storage limit.
	if s.file != nil && s.config.FileBytes > s.size {
		total += s.config.FileBytes - s.size
	}
	cutoff := time.Now().Add(-time.Duration(s.settings.RetentionDays) * 24 * time.Hour)
	for _, f := range files {
		if s.file != nil && filepath.Base(s.file.Name()) == f.Name {
			continue
		}
		if total > maximum || f.Modified.Before(cutoff) {
			if os.Remove(filepath.Join(s.config.Root, f.Name)) == nil {
				total -= f.Size
			}
		}
	}
}
func normalize(e shared.ServiceLogEntry, c Config) shared.ServiceLogEntry {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if e.Component == "" {
		e.Component = c.Component
	}
	if e.Version == "" {
		e.Version = c.Version
	}
	if e.BootID == "" {
		e.BootID = c.BootID
	}
	switch e.Level {
	case "debug", "info", "warn", "error", "fatal":
	default:
		e.Level = "info"
	}
	e.Message = Redact(e.Message)
	e.Stack = Redact(e.Stack)
	var cut bool
	e.Message, cut = bounded(e.Message, 16*1024)
	e.Truncated = e.Truncated || cut
	e.Stack, cut = bounded(e.Stack, 8*1024)
	e.Truncated = e.Truncated || cut
	for _, v := range []*string{&e.RequestID, &e.JobID, &e.OperationID, &e.RoomID, &e.WorldID, &e.Component, &e.Code, &e.Version, &e.BootID, &e.Method, &e.Path} {
		*v = Redact(*v)
		*v = strings.Map(func(r rune) rune {
			if r < ' ' || r == 127 {
				return -1
			}
			return r
		}, *v)
		*v, _ = bounded(*v, 256)
	}
	return e
}
func correlationText(e shared.ServiceLogEntry) string {
	v := ""
	if e.RequestID != "" {
		v += " request_id=" + e.RequestID
	}
	if e.JobID != "" {
		v += " job_id=" + e.JobID
	}
	if e.Code != "" {
		v += " code=" + e.Code
	}
	return v
}

// SanitizeEntry applies the same bounds at authenticated remote/API boundaries.
func SanitizeEntry(e shared.ServiceLogEntry) shared.ServiceLogEntry { return normalize(e, Config{}) }
