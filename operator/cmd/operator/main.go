// Command operator reconciles Channel custom resources against a SoroBeacon
// instance. It polls rather than watches (see docs/operator.md).
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sorotrail/sorobeacon/operator/internal/controller"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	base, token := os.Getenv("SOROBEACON_URL"), os.Getenv("SOROBEACON_TOKEN")
	if base == "" || token == "" {
		log.Error("SOROBEACON_URL and SOROBEACON_TOKEN are required")
		os.Exit(2)
	}
	interval := 30 * time.Second
	if v := os.Getenv("RECONCILE_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= time.Second {
			interval = d
		}
	}
	kube, err := controller.NewInClusterKube()
	if err != nil {
		log.Error("kubernetes client", "err", err)
		os.Exit(1)
	}
	r := &controller.Reconciler{Kube: kube, Beacon: &controller.BeaconClient{BaseURL: base, Token: token}, Log: log}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := r.ReconcileAll(ctx); err != nil {
			log.Warn("reconcile pass failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
