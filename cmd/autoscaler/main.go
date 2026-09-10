// Command autoscaler runs the scaling controller.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/casperlundberg/autoscaler/internal/app"
)

func main() {
	cfg, err := app.LoadConfig(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "autoscaler:", err)
		os.Exit(1)
	}

	// SIGTERM is how Kubernetes asks for a shutdown; honouring it is what
	// makes a rolling update finish in-flight decisions instead of abandoning
	// half-applied plans.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := app.Run(ctx, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "autoscaler:", err)
		os.Exit(1)
	}
}
