package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

// Configuration, from environment variables:
//
//	PORT      port to listen on (default 8080)
//	DATA_DIR  directory for state.json (default ./data)
//	HOST_PIN  secret for host controls; host routes are disabled if unset
func main() {
	addr := ":" + env("PORT", "8080")
	dataDir := env("DATA_DIR", "data")
	hostPin := os.Getenv("HOST_PIN")

	store, err := OpenStore(filepath.Join(dataDir, "state.json"))
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	if hostPin == "" {
		log.Println("WARNING: HOST_PIN is not set, so host controls are disabled")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              addr,
		Handler:           routes(NewServer(store, hostPin)),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: event streams stay open indefinitely.
		// Request contexts derive from ctx, so open streams end on shutdown.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	storeDone := make(chan struct{})
	go func() {
		store.Run(ctx, 500*time.Millisecond)
		close(storeDone)
	}()

	go func() {
		log.Printf("listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	<-storeDone
	if err := store.Flush(); err != nil {
		log.Printf("final save failed: %v", err)
	} else {
		log.Println("state saved")
	}
}

func routes(api *Server) http.Handler {
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /stream", handleStream)
	api.Register(mux)
	return mux
}

// handleStream sends the server time once a second over SSE. It exists to
// verify that the reverse proxy streams events without buffering them.
func handleStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(w)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", time.Now().UTC().Format(time.RFC3339)); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
