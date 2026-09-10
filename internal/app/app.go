// Package app wires the service together: which platforms exist, where state
// lives, and how the HTTP server and the control loop are started and stopped.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/casperlundberg/autoscaler/internal/api"
	"github.com/casperlundberg/autoscaler/internal/controller"
	"github.com/casperlundberg/autoscaler/internal/platform"
	"github.com/casperlundberg/autoscaler/internal/platform/colonycontainers"
	"github.com/casperlundberg/autoscaler/internal/platform/colonypods"
	"github.com/casperlundberg/autoscaler/internal/platform/kubernetes"
	"github.com/casperlundberg/autoscaler/internal/platform/simulation"
	"github.com/casperlundberg/autoscaler/internal/registry"
)

// Config is the service's own configuration — as distinct from a target's
// settings, which are changed at runtime through the API. These are the few
// things that genuinely cannot change without a restart.
type Config struct {
	Address string

	// Token guards every endpoint but the probes. Empty means unauthenticated,
	// which is only reasonable behind something else that authenticates.
	Token string

	// StateFile is where registered targets and their access keys are kept.
	// Empty keeps them in memory, which is what a simulation harness wants and
	// what a service scaling real infrastructure must not use.
	StateFile string

	// Tick is the scheduler granularity for autonomous targets, not any
	// target's cadence — each one is cycled on its own decision interval.
	Tick time.Duration

	LogLevel slog.Level
}

// LoadConfig reads the environment, failing at startup on anything it cannot
// parse.
//
// Failing to start is much better than starting and behaving oddly: a
// container that will not come up is immediately visible, while one that came
// up with a tick of zero is not.
func LoadConfig(lookup func(string) string) (Config, error) {
	cfg := Config{
		Address:   valueOr(lookup, "AUTOSCALER_ADDRESS", ":8080"),
		Token:     lookup("AUTOSCALER_API_TOKEN"),
		StateFile: lookup("AUTOSCALER_STATE_FILE"),
		Tick:      time.Second,
		LogLevel:  slog.LevelInfo,
	}

	if raw := lookup("AUTOSCALER_TICK"); raw != "" {
		tick, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("AUTOSCALER_TICK=%q is not a duration: %w", raw, err)
		}
		if tick <= 0 {
			return Config{}, fmt.Errorf("AUTOSCALER_TICK=%q must be positive", raw)
		}
		cfg.Tick = tick
	}

	if raw := lookup("AUTOSCALER_LOG_LEVEL"); raw != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(strings.ToUpper(raw))); err != nil {
			return Config{}, fmt.Errorf("AUTOSCALER_LOG_LEVEL=%q is not a level: %w", raw, err)
		}
	}

	return cfg, nil
}

// Platforms is every adapter this build supports.
//
// Registered here rather than discovered, so the set is a fact about the
// binary that a reader can see in one place — and so a target naming a
// platform this build does not have is refused with a list of the ones it
// does.
func Platforms() (*platform.Registry, error) {
	return platform.NewRegistry(
		simulation.New(),
		kubernetes.New(),
		colonypods.New(),
		colonycontainers.New(),
	)
}

// Run starts the service and blocks until its context is cancelled.
func Run(ctx context.Context, cfg Config) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)

	platforms, err := Platforms()
	if err != nil {
		return err
	}

	store, err := openStore(cfg, logger)
	if err != nil {
		return err
	}
	targets, err := registry.New(platforms, store)
	if err != nil {
		return err
	}

	engine := controller.New(platforms, targets)
	runner := controller.NewRunner(engine, targets).WithLogger(logger)

	server := &http.Server{
		Addr: cfg.Address,
		Handler: api.New(api.Options{
			Platforms:  platforms,
			Targets:    targets,
			Controller: engine,
			Token:      cfg.Token,
			Logger:     logger,
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	if cfg.Token == "" {
		logger.Warn("no API token is set: every endpoint is unauthenticated, and this " +
			"service holds platform access keys")
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("listening", "address", cfg.Address, "platforms", platforms.Kinds())
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
		close(serverErrors)
	}()

	go runner.Run(ctx, cfg.Tick)

	select {
	case err := <-serverErrors:
		return err
	case <-ctx.Done():
	}

	// Give in-flight requests a moment to finish rather than cutting a caller
	// off mid-decision.
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	logger.Info("shutting down")
	if err := server.Shutdown(shutdown); err != nil {
		return fmt.Errorf("shutting down: %w", err)
	}
	runner.Wait()
	return nil
}

func openStore(cfg Config, logger *slog.Logger) (registry.Store, error) {
	if cfg.StateFile == "" {
		// Deliberately loud. Losing every target and access key on restart is
		// fine for a simulation harness and disastrous for anything else.
		logger.Warn("AUTOSCALER_STATE_FILE is not set: registered targets and their " +
			"access keys will be lost when this process restarts")
		return registry.NewMemoryStore(), nil
	}
	return registry.NewFileStore(cfg.StateFile)
}

func valueOr(lookup func(string) string, key, fallback string) string {
	if value := lookup(key); value != "" {
		return value
	}
	return fallback
}
