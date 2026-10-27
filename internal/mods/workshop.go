package mods

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

type workshopSnapshot struct {
	config   NetworkConfig
	provider *SteamProvider
	version  string
}
type workshopCall struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int
	value   any
	err     error
}
type searchCacheEntry struct {
	result SearchResult
	used   time.Time
	bytes  int
}

// Workshop owns shared request/cache limits across all configuration generations.
// It performs no work until a caller requests data.
type Workshop struct {
	mu          sync.Mutex
	current     *workshopSnapshot
	store       *MetadataStore
	limits      *networkLimits
	calls       map[string]*workshopCall
	searches    map[string]searchCacheEntry
	searchBytes int
	cards       map[string]SteamMod
	imageRoot   string
	imageMu     sync.Mutex
	imageUsed   map[string]time.Time
}

func NewWorkshop(config NetworkConfig, store *MetadataStore, imageRoot string) (*Workshop, error) {
	w := &Workshop{store: store, limits: newNetworkLimits(), calls: make(map[string]*workshopCall), searches: make(map[string]searchCacheEntry), cards: make(map[string]SteamMod), imageRoot: imageRoot, imageUsed: make(map[string]time.Time)}
	commit, discard, err := w.Prepare(config)
	if err != nil {
		return nil, err
	}
	defer discard()
	commit()
	return w, nil
}

// Prepare constructs locally. Network availability never gates saving settings.
func (w *Workshop) Prepare(config NetworkConfig) (func(), func(), error) {
	config, err := config.Normalize()
	if err != nil {
		return nil, nil, err
	}
	p := NewSteamProviderWithBases(config.APIKey, config.AppID, config.APIBase, config.CommunityBase)
	p.HTTPClient = newWorkshopClient(config, w.limits)
	if w.store != nil {
		p.ConfigureMetadataStore(w.store.WithScope(config.scope()))
	}
	next := &workshopSnapshot{config: config, provider: p, version: uuid.NewString()}
	committed := false
	commit := func() {
		w.mu.Lock()
		previous := w.current
		w.current = next
		committed = true
		w.mu.Unlock()
		if previous != nil {
			previous.provider.HTTPClient.CloseIdleConnections()
		}
	}
	discard := func() {
		if !committed {
			p.HTTPClient.CloseIdleConnections()
		}
	}
	return commit, discard, nil
}

func (w *Workshop) snapshot() *workshopSnapshot { w.mu.Lock(); defer w.mu.Unlock(); return w.current }
func (w *Workshop) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, call := range w.calls {
		call.cancel()
	}
	if w.current != nil {
		w.current.provider.HTTPClient.CloseIdleConnections()
	}
	return nil
}

func (w *Workshop) coalesce(ctx context.Context, key string, timeout time.Duration, fn func(context.Context) (any, error)) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, safeNetworkError(err)
	}
	w.mu.Lock()
	call := w.calls[key]
	if call == nil {
		if len(w.calls) >= 32 {
			w.mu.Unlock()
			return nil, &NetworkError{Code: "WORKSHOP_BUSY"}
		}
		shared, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		call = &workshopCall{done: make(chan struct{}), cancel: cancel}
		w.calls[key] = call
		go func() {
			defer cancel()
			value, err := fn(shared)
			w.mu.Lock()
			call.value, call.err = value, err
			if w.calls[key] == call {
				delete(w.calls, key)
			}
			close(call.done)
			w.mu.Unlock()
		}()
	}
	if call.waiters >= 128 {
		w.mu.Unlock()
		return nil, &NetworkError{Code: "WORKSHOP_BUSY"}
	}
	call.waiters++
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		call.waiters--
		if call.waiters == 0 {
			call.cancel()
			if w.calls[key] == call {
				delete(w.calls, key)
			}
		}
		w.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return nil, safeNetworkError(ctx.Err())
	case <-call.done:
		return call.value, call.err
	}
}

func workshopQuery(query string) string {
	u, err := url.Parse(strings.TrimSpace(query))
	if err == nil && (u.Scheme == "http" || u.Scheme == "https") && strings.EqualFold(u.Hostname(), "steamcommunity.com") && u.Path == "/sharedfiles/filedetails/" && validModID(u.Query().Get("id")) {
		return u.Query().Get("id")
	}
	return strings.TrimSpace(query)
}

func (w *Workshop) Search(ctx context.Context, options SearchOptions) (SearchResult, error) {
	s := w.snapshot()
	options.Query = workshopQuery(options.Query)
	options = normalizeSearchOptions(options)
	sort.Strings(options.Tags)
	if err := validateSearch(options); err != nil {
		return SearchResult{}, err
	}
	if options.Source != "" && options.Source != "api" && options.Source != "community" {
		return SearchResult{}, ErrInvalidRequest
	}
	if options.ConfigVersion != "" && options.ConfigVersion != s.version {
		return SearchResult{}, &NetworkError{Code: "WORKSHOP_CONFIG_CHANGED"}
	}
	source := options.Source
	if source == "" {
		source = "community"
		if s.config.APIKey != "" {
			source = "api"
		}
	}
	if source == "api" && s.config.APIKey == "" && !validModID(options.Query) {
		return SearchResult{}, ErrSteamKeyRequired
	}
	if validModID(options.Query) {
		source = "api"
	}
	options.Source, options.ConfigVersion = source, ""
	encoded, _ := json.Marshal(options)
	key := s.version + ":search:" + string(encoded)
	w.mu.Lock()
	cached, hasCache := w.searches[key]
	if hasCache {
		cached.used = time.Now()
		w.searches[key] = cached
	}
	w.mu.Unlock()
	if hasCache && time.Since(cached.result.ObservedAt) < 5*time.Minute {
		return cloneSearch(cached.result), nil
	}
	value, err := w.coalesce(ctx, key, 8*time.Second, func(ctx context.Context) (any, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		defer w.finish(s)
		var result SearchResult
		var err error
		if validModID(options.Query) {
			items, e := s.provider.BasicDetails(ctx, []string{options.Query})
			err = e
			result = SearchResult{Items: []SteamMod{}, Page: 1, PageSize: options.PageSize, TotalKind: "exact"}
			if item, ok := items[options.Query]; ok {
				result.Items = append(result.Items, item)
				result.Total = 1
				if err != nil {
					result.Warning = err.Error()
					err = nil
				}
			}
		} else if source == "community" {
			result, err = s.provider.searchCommunity(ctx, options)
		} else {
			result, err = s.provider.Search(ctx, options)
		}
		if err != nil {
			return nil, err
		}
		result.Source, result.ConfigVersion, result.ObservedAt = source, s.version, time.Now().UTC()
		if result.TotalKind == "" {
			result.TotalKind = "exact"
		}
		result.HasMore = result.Page*result.PageSize < result.Total
		w.remember(s, result.Items)
		w.cacheSearch(key, result)
		return result, nil
	})
	if err != nil {
		if hasCache && time.Since(cached.result.ObservedAt) < 24*time.Hour && !errors.Is(ctx.Err(), context.Canceled) {
			result := cloneSearch(cached.result)
			result.Stale = true
			result.Warning = fmt.Sprintf("WORKSHOP_CACHED_RESULT: %v", safeNetworkError(err))
			return result, nil
		}
		return SearchResult{}, err
	}
	return cloneSearch(value.(SearchResult)), nil
}

func cloneMod(item SteamMod) SteamMod {
	item.Tags = append([]string(nil), item.Tags...)
	if item.Dependencies != nil {
		item.Dependencies = append([]string{}, item.Dependencies...)
	}
	return item
}
func cloneSearch(value SearchResult) SearchResult {
	value.Items = append([]SteamMod{}, value.Items...)
	for i := range value.Items {
		value.Items[i] = cloneMod(value.Items[i])
	}
	return value
}

func (w *Workshop) cacheSearch(key string, result SearchResult) {
	raw, _ := json.Marshal(result)
	if len(raw) > 8<<20 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if old, ok := w.searches[key]; ok {
		w.searchBytes -= old.bytes
	}
	w.searches[key] = searchCacheEntry{result: cloneSearch(result), used: time.Now(), bytes: len(raw)}
	w.searchBytes += len(raw)
	for len(w.searches) > 256 || w.searchBytes > 8<<20 {
		oldest := ""
		var when time.Time
		for k, entry := range w.searches {
			if oldest == "" || entry.used.Before(when) {
				oldest, when = k, entry.used
			}
		}
		w.searchBytes -= w.searches[oldest].bytes
		delete(w.searches, oldest)
	}
}
func (w *Workshop) remember(s *workshopSnapshot, items []SteamMod) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, item := range items {
		key := s.version + ":" + item.ID
		if len(w.cards) >= 512 {
			for k := range w.cards {
				delete(w.cards, k)
				break
			}
		}
		if old, ok := w.cards[key]; ok {
			if item.Name == "" {
				item.Name = old.Name
			}
			if item.PreviewURL == "" {
				item.PreviewURL = old.PreviewURL
			}
			if item.AuthorID == "" {
				item.AuthorID = old.AuthorID
			}
			if item.Author == "" {
				item.Author = old.Author
			}
		}
		if len(item.Name) > 4096 {
			item.Name = string([]rune(item.Name)[:min(512, len([]rune(item.Name)))])
		}
		if len(item.Author) > 512 {
			item.Author = ""
		}
		if len(item.PreviewURL) > 8192 {
			item.PreviewURL = ""
		}
		if len(item.AuthorID) > 32 {
			item.AuthorID = ""
		}
		w.cards[key] = SteamMod{ID: item.ID, Name: item.Name, Author: item.Author, AuthorID: item.AuthorID, PreviewURL: item.PreviewURL}
	}
}

func (w *Workshop) read(ctx context.Context, ids []string, mode string) (map[string]SteamMod, error) {
	if len(ids) > 100 {
		return nil, ErrInvalidRequest
	}
	for _, id := range ids {
		if !validModID(id) {
			return nil, ErrInvalidModID
		}
	}
	ids = uniqueModIDs(ids)
	sort.Strings(ids)
	s := w.snapshot()
	value, err := w.coalesce(ctx, s.version+":"+mode+":"+strings.Join(ids, ","), 8*time.Second, func(ctx context.Context) (any, error) {
		defer w.finish(s)
		var result map[string]SteamMod
		var err error
		switch mode {
		case "cached":
			result, err = s.provider.CachedMetadata(ctx, ids)
		case "summaries":
			result, err = s.provider.Summaries(ctx, ids)
		case "enrich", "authors":
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			result = make(map[string]SteamMod)
			if mode == "enrich" {
				result, err = s.provider.BasicDetails(ctx, ids)
			}
			if result == nil {
				result = make(map[string]SteamMod)
			}
			items := make([]SteamMod, 0, len(ids))
			w.mu.Lock()
			for _, id := range ids {
				item := result[id]
				if item.ID == "" {
					item = w.cards[s.version+":"+id]
				}
				item.ID = id
				items = append(items, item)
			}
			w.mu.Unlock()
			authorCtx := context.WithValue(ctx, optionalRequestKey{}, mode == "authors")
			s.provider.populateAuthors(authorCtx, items)
			if mode == "enrich" {
				err = errors.Join(err, s.provider.populateCommunityMetadata(ctx, items))
			}
			if result == nil {
				result = make(map[string]SteamMod)
			}
			for _, item := range items {
				result[item.ID] = item
			}
		default:
			result, err = s.provider.BasicDetails(ctx, ids)
		}
		items := make([]SteamMod, 0, len(result))
		for _, item := range result {
			items = append(items, item)
		}
		w.remember(s, items)
		return result, err
	})
	if value == nil {
		return nil, err
	}
	// Consumers merge Runtime fields; return independent mutable collections.
	result := make(map[string]SteamMod)
	for id, item := range value.(map[string]SteamMod) {
		result[id] = cloneMod(item)
	}

	return result, err
}

func (w *Workshop) Details(ctx context.Context, ids []string) (map[string]SteamMod, error) {
	return w.read(ctx, ids, "basic")
}
func (w *Workshop) DisplayMetadata(ctx context.Context, ids []string) (map[string]SteamMod, error) {
	return w.read(ctx, ids, "basic")
}
func (w *Workshop) Summaries(ctx context.Context, ids []string) (map[string]SteamMod, error) {
	return w.read(ctx, ids, "summaries")
}
func (w *Workshop) CachedMetadata(ctx context.Context, ids []string) (map[string]SteamMod, error) {
	return w.read(ctx, ids, "cached")
}
func (w *Workshop) Enrich(ctx context.Context, ids []string) (map[string]SteamMod, error) {
	return w.read(ctx, ids, "enrich")
}

// Test uses an isolated, unsaved snapshot but shares the global network limits.
type ConnectionStage struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Code       string `json:"code,omitempty"`
	DurationMS int64  `json:"durationMs"`
}
type ConnectionTest struct {
	Stages    []ConnectionStage `json:"stages"`
	CheckedAt time.Time         `json:"checkedAt"`
}

func (w *Workshop) Test(ctx context.Context, config NetworkConfig, sampleID string) (ConnectionTest, error) {
	config, err := config.Normalize()
	if err != nil {
		return ConnectionTest{}, ErrInvalidRequest
	}
	ctx = context.WithValue(ctx, connectionTestKey{}, true)
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	p := NewSteamProviderWithBases(config.APIKey, config.AppID, config.APIBase, config.CommunityBase)
	p.HTTPClient = newWorkshopClient(config, w.limits)
	defer p.HTTPClient.CloseIdleConnections()
	result := ConnectionTest{Stages: []ConnectionStage{}, CheckedAt: time.Now().UTC()}
	run := func(name string, fn func(context.Context) error) {
		start := time.Now()
		child, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		err := fn(child)
		stage := ConnectionStage{Name: name, Status: "available", DurationMS: time.Since(start).Milliseconds()}
		if err != nil {
			stage.Status = "unavailable"
			stage.Code = safeNetworkError(err).Error()
		}
		result.Stages = append(result.Stages, stage)
	}
	var preview string
	run("search", func(ctx context.Context) error {
		value, err := p.Search(ctx, SearchOptions{Page: 1, PageSize: 1, Sort: SearchSortTrend})
		if len(value.Items) > 0 {
			if sampleID == "" {
				sampleID = value.Items[0].ID
			}
			preview = value.Items[0].PreviewURL
		}
		return err
	})
	if validModID(sampleID) {
		run("details", func(ctx context.Context) error {
			items, err := p.Summaries(ctx, []string{sampleID})
			if item, ok := items[sampleID]; ok {
				if item.PreviewURL != "" {
					preview = item.PreviewURL
				}
			}
			return err
		})
	} else {
		result.Stages = append(result.Stages, ConnectionStage{Name: "details", Status: "not_tested", Code: "WORKSHOP_SAMPLE_REQUIRED"})
	}
	if preview != "" {
		run("image", func(ctx context.Context) error { _, err := fetchThumbnail(ctx, p.HTTPClient, preview, 80); return err })
	} else {
		result.Stages = append(result.Stages, ConnectionStage{Name: "image", Status: "not_tested", Code: "WORKSHOP_SAMPLE_REQUIRED"})
	}
	return result, nil
}

func (w *Workshop) Version() string { return w.snapshot().version }

func (w *Workshop) Authors(ctx context.Context, ids []string) (map[string]SteamMod, error) {
	return w.read(ctx, ids, "authors")
}

func (w *Workshop) finish(s *workshopSnapshot) {
	if w.snapshot() != s {
		s.provider.HTTPClient.CloseIdleConnections()
	}
}
