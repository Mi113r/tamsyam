// Command tamsyam-server runs the tunnel server behind a TLS-terminating proxy.
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

	"github.com/Mi113r/tamsyam/internal/server"
	"github.com/Mi113r/tamsyam/internal/store"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg := server.Config{
		Domain:        env("TAMSYAM_DOMAIN", ""),
		PublicScheme:  env("TAMSYAM_PUBLIC_SCHEME", "https"),
		PublicPort:    env("TAMSYAM_PUBLIC_PORT", ""),
		AdminPassword: env("TAMSYAM_ADMIN_PASSWORD", ""),
	}
	listen := env("TAMSYAM_LISTEN", "127.0.0.1:8090")
	dbPath := env("TAMSYAM_DB", "tamsyam.db")

	if cfg.Domain == "" || cfg.AdminPassword == "" {
		log.Error("TAMSYAM_DOMAIN and TAMSYAM_ADMIN_PASSWORD are required")
		os.Exit(2)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	srv := &http.Server{
		Addr:              listen,
		Handler:           server.New(cfg, st, log),
		ReadHeaderTimeout: 15 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("listening", "addr", listen, "domain", cfg.Domain)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
