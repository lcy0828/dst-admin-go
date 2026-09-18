package adminserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"dont/internal/webui"
	"dont/routers"
)

const shutdownTimeout = 20 * time.Second

func Run(ctx context.Context, address string) error {
	application, err := routers.InitRuntimeHost()
	if err != nil {
		return fmt.Errorf("initialize application: %w", err)
	}
	if err := application.Start(ctx); err != nil {
		closeErr := application.Close(context.Background())
		return errors.Join(fmt.Errorf("start application: %w", err), closeErr)
	}
	handler, err := webui.Wrap(application.HTTPHandler(), strings.TrimSpace(os.Getenv("DST_ADMIN_WEB_ROOT")))
	if err != nil {
		closeErr := application.Close(context.Background())
		return errors.Join(fmt.Errorf("initialize web UI: %w", err), closeErr)
	}
	server := &http.Server{
		Addr:              address,
		Handler:           uploadReadTimeout(handler, 2*time.Hour),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.ListenAndServe() }()

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-serveErrors:
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownContext)
	closeErr := application.Close(shutdownContext)
	return errors.Join(serveErr, shutdownErr, closeErr)
}

// Extend only file-upload body reads; ordinary requests retain ReadTimeout and
// every connection still has the short ReadHeaderTimeout.
func uploadReadTimeout(next http.Handler, timeout time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		roomUpload := strings.HasPrefix(path, "/api/v2/rooms/") && strings.HasSuffix(path, "/backups/upload")
		if r.Method == http.MethodPost && (roomUpload || path == "/api/v2/save-imports/upload" ||
			path == "/api/v2/runtime-targets/luajit/packages" || path == "/api/v2/agent-releases") {
			if err := http.NewResponseController(w).SetReadDeadline(time.Now().Add(timeout)); err != nil {
				http.Error(w, "upload read deadline unavailable", http.StatusInternalServerError)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
