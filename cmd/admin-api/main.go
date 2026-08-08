package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"dont/routers"
)

func main() {
	address := flag.String("addr", "127.0.0.1:18000", "HTTP listen address")
	flag.Parse()

	router, err := routers.InitRouter()
	if err != nil {
		log.Fatalf("initialize router: %v", err)
	}
	server := &http.Server{
		Addr:              *address,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		// SSE connections are long-lived; event batches and heartbeats remain bounded.
		WriteTimeout: 0,
		IdleTimeout:  90 * time.Second,
	}
	log.Printf("DST Admin API listening on http://%s", *address)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("serve: %v", err)
	}
}
