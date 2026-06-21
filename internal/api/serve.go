package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/imlach/nightwatch/internal/inventory"
	"github.com/imlach/nightwatch/internal/talos"
)

// ServeConfig configures the long-running trigger server.
type ServeConfig struct {
	Listen                  string
	Token                   string // required; Serve refuses to start when empty (fail closed)
	Kubeconfig, Talosconfig string
}

// Serve runs the HTTP trigger API until ctx is cancelled, then drains in-flight
// requests. It refuses to start without a token - the endpoint can power off
// nodes, so missing auth fails closed rather than opening an unauthenticated
// actuator. Lifecycle ops run SYNCHRONOUSLY within the request (a drain can take
// minutes); the server write timeout is sized for that, and clients must set a
// matching long read timeout.
func Serve(ctx context.Context, inv *inventory.Inventory, cfg ServeConfig) error {
	if cfg.Token == "" {
		return errors.New("NIGHTWATCH_API_TOKEN is unset; refusing to start an unauthenticated trigger API")
	}

	h := &Handler{
		Inv:         inv,
		Token:       cfg.Token,
		Builder:     nil, // RealBuilder
		Kubeconfig:  cfg.Kubeconfig,
		Talosconfig: cfg.Talosconfig,
		reachable:   talosReachable(cfg.Talosconfig),
	}

	srv := &http.Server{
		Addr:    cfg.Listen,
		Handler: h.Routes(),
		// A drain-shutdown runs synchronously inside the request, so the write
		// timeout must cover the full op (drain + storage gate + power-off waits).
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      20 * time.Minute,
		ReadTimeout:       0, // body is tiny; the long pole is the op, bounded by WriteTimeout
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("nightwatch serve", "listen", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("nightwatch serve: shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	}
}

// talosReachable returns a best-effort Talos reachability probe for /status,
// building a short-lived client per call so a missing/expired talosconfig just
// omits the field rather than failing the endpoint. Returns nil when no
// talosconfig is configured.
func talosReachable(talosconfig string) func(ctx context.Context, endpoint string) (bool, error) {
	if talosconfig == "" {
		return nil
	}
	return func(ctx context.Context, endpoint string) (bool, error) {
		tc, err := talos.New(ctx, talosconfig)
		if err != nil {
			return false, err
		}
		defer tc.Close()
		return tc.Reachable(ctx, endpoint), nil
	}
}
