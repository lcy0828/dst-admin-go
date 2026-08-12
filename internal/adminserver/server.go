package adminserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"dont/routers"
)

const shutdownTimeout = 20 * time.Second

func Run(ctx context.Context, address string) error {
	application, err := routers.InitApplication()
	if err != nil {
		return fmt.Errorf("initialize application: %w", err)
	}
	if err := application.Start(ctx); err != nil {
		closeErr := application.Close(context.Background())
		return errors.Join(fmt.Errorf("start application: %w", err), closeErr)
	}
	server := &http.Server{
		Addr:              address,
		Handler:           application.HTTPHandler(),
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
