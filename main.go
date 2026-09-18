package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

)

// version is stamped by CI with -ldflags "-X main.version=...". The
// fallback is what a local `go build` produces.
var version = "dev"

func main() {
	// Every line carries who emitted it. Alloy adds pod and namespace
	// labels in Loki, but a line copied into a ticket, an alert or a
	// terminal arrives without them - and "shutting down" from an unnamed
	// service is not worth much.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)).With(
		"service", "approvald",
		"team", "me-myself-and-i",
		"version", version,
	))

	h, err := handler()
	if err != nil {
		slog.Error("building handler", "err", err)
		os.Exit(1)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	// A Server with no timeouts lets a slow client hold a connection open
	// indefinitely - the Slowloris shape - and govulncheck flags the
	// missing ReadHeaderTimeout specifically.
	srv := &http.Server{
		Addr:              "0.0.0.0:" + port,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Without this, SIGTERM kills in-flight requests on every deploy.
	// Kubernetes sends SIGTERM, waits terminationGracePeriodSeconds, then
	// SIGKILLs; draining inside that window is what makes a rollout
	// invisible to callers.
	idle := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		slog.Info("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			slog.Error("shutdown", "err", err)
		}
		close(idle)
	}()

	slog.Info("started", "addr", ":"+port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
	<-idle
}
