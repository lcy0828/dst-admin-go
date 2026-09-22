package artworkpack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"dont/internal/entitycatalog"
)

type Installed struct {
	Version     string    `json:"version"`
	GameVersion string    `json:"gameVersion"`
	Revision    string    `json:"revision"`
	Bytes       int64     `json:"bytes"`
	Entities    int       `json:"entities"`
	Illustrated int       `json:"illustrated"`
	Images      int       `json:"images"`
	InstalledAt time.Time `json:"installedAt"`
}

type Status struct {
	Available      Release    `json:"available"`
	Installed      *Installed `json:"installed"`
	Phase          string     `json:"phase"`
	Busy           bool       `json:"busy"`
	ReceivedBytes  int64      `json:"receivedBytes"`
	TotalBytes     int64      `json:"totalBytes"`
	BytesPerSecond int64      `json:"bytesPerSecond"`
	Error          string     `json:"error,omitempty"`
}

type activePack struct {
	Installed Installed `json:"installed"`
	Directory string    `json:"directory"`
	catalog   catalog
	artwork   map[string]string
}

type Service struct {
	root      string
	release   Release
	client    *http.Client
	mu        sync.RWMutex
	active    *activePack
	phase     string
	busy      bool
	closed    bool
	received  int64
	started   time.Time
	lastError string
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

func New(root string, release Release, client *http.Client) (*Service, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 8 * time.Minute}
	}
	s := &Service{root: root, release: release, client: client, phase: "idle"}
	data, err := os.ReadFile(filepath.Join(root, "active.json"))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		s.phase, s.lastError = "failed", "invalid_installed_pack"
		return s, nil
	}
	var active activePack
	if json.Unmarshal(data, &active) != nil || !validDirectory(active.Directory) {
		s.phase, s.lastError = "failed", "invalid_installed_pack"
		return s, nil
	}
	// Only the tiny catalog stays in memory; image bytes are read on demand.
	if err := active.load(filepath.Join(root, active.Directory)); err != nil {
		s.phase, s.lastError = "failed", "invalid_installed_pack"
		return s, nil
	}
	s.active = &active
	return s, nil
}

func validDirectory(name string) bool {
	return strings.HasPrefix(name, "pack-") && filepath.Base(name) == name && !strings.ContainsAny(name, "/\\\x00")
}

func (s *Service) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := Status{Available: s.release, Phase: s.phase, Busy: s.busy, ReceivedBytes: s.received, TotalBytes: s.release.ArchiveBytes, Error: s.lastError}
	if s.active != nil {
		value := s.active.Installed
		result.Installed = &value
	}
	if s.busy && !s.started.IsZero() {
		result.BytesPerSecond = int64(float64(s.received) / max(time.Since(s.started).Seconds(), 0.1))
	}
	return result
}

func (s *Service) begin(parent context.Context, phase string) (context.Context, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy || s.closed {
		return nil, ErrBusy
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	s.busy, s.phase, s.lastError, s.received, s.started, s.cancel = true, phase, "", 0, time.Now(), cancel
	s.wg.Add(1)
	return ctx, nil
}

func (s *Service) finish(err error) {
	s.mu.Lock()
	s.cancel()
	s.cancel = nil
	s.busy = false
	s.phase = "idle"
	if err != nil {
		s.phase = "failed"
		switch {
		case errors.Is(err, ErrInvalid):
			s.lastError = "invalid_package"
		case errors.Is(err, context.Canceled):
			s.lastError = "cancelled"
		case errors.Is(err, context.DeadlineExceeded):
			s.lastError = "timeout"
		default:
			s.lastError = "transfer_or_storage_failed"
		}
	}
	s.mu.Unlock()
	s.wg.Done()
}

func (s *Service) setPhase(phase string) { s.mu.Lock(); s.phase = phase; s.mu.Unlock() }

func (s *Service) Download(source string) error {
	url, ok := s.release.Sources[source]
	if !ok {
		return ErrSource
	}
	ctx, err := s.begin(context.Background(), "downloading")
	if err != nil {
		return err
	}
	go func() {
		err := func() error {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return err
			}
			resp, err := s.client.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("artwork download: HTTP %d", resp.StatusCode)
			}
			if resp.ContentLength > MaxArchiveBytes {
				return ErrInvalid
			}
			return s.install(ctx, resp.Body)
		}()
		s.finish(err)
	}()
	return nil
}

func (s *Service) Import(ctx context.Context, input io.Reader) error {
	ctx, err := s.begin(ctx, "uploading")
	if err != nil {
		return err
	}
	err = s.install(ctx, input)
	s.finish(err)
	return err
}

// Cancel never removes the active pack. Its replacement is only activated
// after download, checksum and complete extraction have all succeeded.
func (s *Service) Cancel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type progressReader struct {
	ctx     context.Context
	input   io.Reader
	service *Service
}

func (r progressReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.input.Read(p)
	r.service.mu.Lock()
	r.service.received += int64(n)
	r.service.mu.Unlock()
	return n, err
}

func (s *Service) install(ctx context.Context, input io.Reader) error {
	if err := os.MkdirAll(s.root, 0700); err != nil {
		return err
	}
	archive, err := os.CreateTemp(s.root, "download-")
	if err != nil {
		return err
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(progressReader{ctx, input, s}, MaxArchiveBytes+1))
	if err != nil {
		return err
	}
	s.setPhase("verifying")
	if n != s.release.ArchiveBytes || n > MaxArchiveBytes || hex.EncodeToString(hash.Sum(nil)) != s.release.SHA256 {
		return ErrInvalid
	}
	if _, err = archive.Seek(0, io.SeekStart); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(s.root, "pack-")
	if err != nil {
		return err
	}
	activated := false
	defer func() {
		if !activated {
			_ = os.RemoveAll(stage)
		}
	}()
	s.setPhase("installing")
	bytes, err := extract(ctx, archive, stage)
	if err != nil {
		return err
	}
	active := &activePack{Directory: filepath.Base(stage), Installed: Installed{
		Version: s.release.Version, GameVersion: s.release.GameVersion, Revision: s.release.SHA256, Bytes: bytes,
		Entities: s.release.Entities, Illustrated: s.release.Illustrated, Images: s.release.Images, InstalledAt: time.Now().UTC(),
	}}
	if bytes != s.release.InstalledBytes {
		return ErrInvalid
	}
	if err = active.load(stage); err != nil {
		return err
	}
	data, err := json.Marshal(active)
	if err != nil {
		return err
	}
	pointer, err := os.CreateTemp(s.root, "active-")
	if err != nil {
		return err
	}
	defer os.Remove(pointer.Name())
	_, writeErr := pointer.Write(data)
	syncErr := pointer.Sync()
	closeErr := pointer.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	s.mu.Lock()
	if err = ctx.Err(); err != nil {
		s.mu.Unlock()
		return err
	}
	if err = os.Rename(pointer.Name(), filepath.Join(s.root, "active.json")); err != nil {
		s.mu.Unlock()
		return err
	}
	old := s.active
	s.active = active
	activated = true
	s.mu.Unlock()
	if old != nil && old.Directory != active.Directory {
		_ = os.RemoveAll(filepath.Join(s.root, old.Directory))
	}
	return nil
}

func (s *Service) Uninstall() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy || s.closed {
		return ErrBusy
	}
	if err := os.Remove(filepath.Join(s.root, "active.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.active = nil
	// This is a dedicated resource directory, never a game/save directory.
	entries, err := os.ReadDir(s.root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if validDirectory(entry.Name()) || strings.HasPrefix(entry.Name(), "download-") || strings.HasPrefix(entry.Name(), "active-") {
			if err = os.RemoveAll(filepath.Join(s.root, entry.Name())); err != nil {
				return err
			}
		}
	}
	s.phase, s.lastError, s.received = "idle", "", 0
	return nil
}

func (s *Service) Index() (string, []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := []string{}
	if s.active == nil {
		return "", ids
	}
	for id := range s.active.artwork {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return s.active.Installed.Revision, ids
}

func (s *Service) Artwork(prefab string) ([]byte, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil {
		return nil, "", ErrNotInstalled
	}
	name, ok := s.active.artwork[prefab]
	if !ok {
		return nil, "", os.ErrNotExist
	}
	root, err := os.OpenRoot(filepath.Join(s.root, s.active.Directory))
	if err != nil {
		return nil, "", err
	}
	defer root.Close()
	file, err := root.Open(filepath.FromSlash(name))
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (256<<10)+1))
	if len(data) > 256<<10 {
		return nil, "", ErrInvalid
	}
	return data, s.active.Installed.Revision, err
}

func (s *Service) Search(options entitycatalog.RuntimeSearchOptions) (entitycatalog.RuntimeSearchResult, error) {
	query := strings.ToLower(strings.TrimSpace(options.Query))
	if len([]rune(query)) > 100 || options.Limit < 1 || options.Limit > 120 || options.Offset < 0 || options.Offset > 50000 {
		return entitycatalog.RuntimeSearchResult{}, entitycatalog.ErrInvalidSearch
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.active == nil {
		return entitycatalog.RuntimeSearchResult{}, ErrNotInstalled
	}
	result := entitycatalog.RuntimeSearchResult{Items: []entitycatalog.Entity{}, Mods: []entitycatalog.RuntimeMod{}, Offset: options.Offset, ObservedAt: s.active.Installed.InstalledAt}
	for _, item := range s.active.catalog.Items {
		if query != "" && !strings.Contains(strings.ToLower(item.ID+" "+item.NameZhCN+" "+item.NameEn), query) {
			continue
		}
		result.Total++
		if result.Total <= options.Offset || len(result.Items) >= options.Limit {
			continue
		}
		result.Items = append(result.Items, entitycatalog.Entity{ID: item.ID, Key: "vanilla:" + item.ID, Namespace: "vanilla", NameZhCN: item.NameZhCN, NameEn: item.NameEn, Type: item.Type, Source: "artwork-pack", Capabilities: []entitycatalog.Capability{entitycatalog.CapabilityGive, entitycatalog.CapabilitySpawn, entitycatalog.CapabilityRemove}})
	}
	result.HasMore = options.Offset+len(result.Items) < result.Total
	return result, nil
}
