package routers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"dont/internal/systemsettings"
	"dont/models"
	"dont/pkg/setting"
)

type requestTicket struct {
	cancel context.CancelFunc
	read   bool
}
type ticketKey struct{}

// RuntimeHost preserves the listener and process database while replacing idle
// runtime modules. There is no polling or extra process in the standalone path.
type RuntimeHost struct {
	transitionMu sync.Mutex
	mu           sync.Mutex
	app          *Application
	ctx          context.Context
	reloading    bool
	closed       bool
	active       map[*requestTicket]struct{}
	changed      chan struct{}
	build        func(setting.Snapshot) (*Application, error)
}

func InitRuntimeHost() (*RuntimeHost, error) {
	build := func(config setting.Snapshot) (*Application, error) { return initApplicationConfig(true, false, config) }
	app, err := build(setting.CurrentSnapshot())
	if err != nil {
		_ = models.CloseDB()
		return nil, err
	}
	host := &RuntimeHost{app: app, active: make(map[*requestTicket]struct{}), changed: make(chan struct{}, 1), build: build}
	app.settings.SetRuntimeApplier(host.apply)
	return host, nil
}

func (h *RuntimeHost) Start(ctx context.Context) error { h.ctx = ctx; return h.app.Start(ctx) }
func (h *RuntimeHost) HTTPHandler() http.Handler       { return h }

func (h *RuntimeHost) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	if h.reloading || h.closed {
		h.mu.Unlock()
		w.Header().Set("Retry-After", "1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"data":null,"error":{"code":"SYSTEM_RELOADING","message":"正在应用系统设置，请稍后重试"}}`))
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	ticket := &requestTicket{cancel: cancel, read: r.Method == http.MethodGet}
	h.active[ticket] = struct{}{}
	app := h.app
	h.mu.Unlock()
	defer func() {
		cancel()
		h.mu.Lock()
		delete(h.active, ticket)
		h.mu.Unlock()
		select {
		case h.changed <- struct{}{}:
		default:
		}
	}()
	app.HTTPHandler().ServeHTTP(w, r.WithContext(context.WithValue(ctx, ticketKey{}, ticket)))
}

func (h *RuntimeHost) barrier(ctx context.Context) (func(), error) {
	ticket, ok := ctx.Value(ticketKey{}).(*requestTicket)
	if !ok {
		return nil, errors.New("runtime settings require a managed HTTP request")
	}
	h.mu.Lock()
	if h.reloading || h.closed {
		h.mu.Unlock()
		return nil, systemsettings.ErrRuntimeBusy
	}
	h.reloading = true
	for active := range h.active {
		// SSE and read streams reconnect to the new generation. Writes finish
		// normally, so no partially applied mutation is abandoned here.
		if active != ticket && active.read {
			active.cancel()
		}
	}
	h.mu.Unlock()
	release := func() { h.mu.Lock(); h.reloading = false; h.mu.Unlock() }
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		h.mu.Lock()
		pending := len(h.active)
		h.mu.Unlock()
		if pending == 1 {
			return release, nil
		}
		select {
		case <-h.changed:
		case <-waitCtx.Done():
			release()
			return nil, systemsettings.ErrRuntimeBusy
		}
	}
}

func (h *RuntimeHost) apply(ctx context.Context, persist, rollback func() error) (resultErr error) {
	defer func() {
		if resultErr != nil {
			log.Printf("[Settings] runtime apply failed: %v", resultErr)
		}
	}()
	if !h.transitionMu.TryLock() {
		return systemsettings.ErrRuntimeBusy
	}
	defer h.transitionMu.Unlock()
	release, err := h.barrier(ctx)
	if err != nil {
		return err
	}
	defer release()
	old := h.app
	resume, err := old.prepareReload(ctx)
	if err != nil {
		return err
	}
	defer resume()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := persist(); err != nil {
		return err
	}
	config, err := old.config.Reload()
	if err != nil {
		return errors.Join(err, rollback())
	}
	// Finish replacement even if the browser disconnects after persistence.
	// Old background workers must exit before the new generation may start.
	if err := old.Close(context.Background()); err != nil {
		return errors.Join(fmt.Errorf("close previous runtime: %w", err), rollback(), h.restore(old.config))
	}
	next, err := h.build(config)
	if err == nil {
		next.settings.SetRuntimeApplier(h.apply)
		err = next.Start(h.ctx)
		if err != nil {
			_ = next.Close(context.Background())
		}
	}
	if err != nil {
		return errors.Join(fmt.Errorf("activate runtime settings: %w", err), rollback(), h.restore(old.config))
	}
	h.mu.Lock()
	h.app = next
	h.mu.Unlock()
	log.Print("[Settings] runtime configuration applied without restarting the process")
	return nil
}

func (h *RuntimeHost) restore(config setting.Snapshot) error {
	app, err := h.build(config)
	if err == nil {
		app.settings.SetRuntimeApplier(h.apply)
		err = app.Start(h.ctx)
		if err != nil {
			_ = app.Close(context.Background())
		}
	}
	if err != nil {
		h.mu.Lock()
		h.closed = true
		h.mu.Unlock()
		log.Printf("[Settings] failed to restore previous runtime: %v", err)
		return fmt.Errorf("restore previous runtime: %w", err)
	}
	h.mu.Lock()
	h.app = app
	h.mu.Unlock()
	return nil
}

func (h *RuntimeHost) Close(ctx context.Context) error {
	h.transitionMu.Lock()
	defer h.transitionMu.Unlock()
	h.mu.Lock()
	h.closed = true
	app := h.app
	h.mu.Unlock()
	return errors.Join(app.Close(ctx), models.CloseDB())
}
