package routers

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
)

var ErrApplicationClosed = errors.New("application is already closed")

type applicationHooks struct {
	start   []func(context.Context) error
	workers []func(context.Context)
	stop    []func(context.Context) error
	final   []func() error
}

// Application owns the HTTP handler and every process-scoped background task.
type Application struct {
	router *gin.Engine
	hooks  applicationHooks

	mu      sync.Mutex
	started bool
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func newApplication(router *gin.Engine, hooks applicationHooks) *Application {
	return &Application{router: router, hooks: hooks}
}

func (a *Application) HTTPHandler() http.Handler { return a.router }

func (a *Application) Router() *gin.Engine { return a.router }

func (a *Application) Context() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

func (a *Application) Start(parent context.Context) error {
	if parent == nil {
		parent = context.Background()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return ErrApplicationClosed
	}
	if a.started {
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	if err := ctx.Err(); err != nil {
		cancel()
		return err
	}
	started := 0
	for _, start := range a.hooks.start {
		if err := start(ctx); err != nil {
			cancel()
			for index := min(started, len(a.hooks.stop)) - 1; index >= 0; index-- {
				_ = a.hooks.stop[index](context.Background())
			}
			return err
		}
		started++
	}
	a.ctx, a.cancel, a.started = ctx, cancel, true
	for _, worker := range a.hooks.workers {
		worker := worker
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			worker(ctx)
		}()
	}
	return nil
}

func (a *Application) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil
	}
	a.closed = true
	cancel, started := a.cancel, a.started
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	var result error
	if started {
		for index := len(a.hooks.stop) - 1; index >= 0; index-- {
			result = errors.Join(result, a.hooks.stop[index](ctx))
		}
	}
	waited := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-ctx.Done():
		result = errors.Join(result, ctx.Err())
	}
	for index := len(a.hooks.final) - 1; index >= 0; index-- {
		result = errors.Join(result, a.hooks.final[index]())
	}
	return result
}
