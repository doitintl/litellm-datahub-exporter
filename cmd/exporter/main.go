package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/doitintl/litellm-datahub-exporter/internal/config"
	"github.com/doitintl/litellm-datahub-exporter/internal/datahub"
	"github.com/doitintl/litellm-datahub-exporter/internal/litellm"
	"github.com/doitintl/litellm-datahub-exporter/internal/metrics"
	"github.com/doitintl/litellm-datahub-exporter/internal/runner"
)

var version = "dev"

func main() {
	once := flag.Bool("once", false, "run a single export cycle and exit (for cron or verification)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("version", version)

	cfg, err := config.FromEnv()
	if err != nil {
		log.Error("configuration error", "err", err)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	lc := litellm.NewClient(cfg.LiteLLMBaseURL, cfg.LiteLLMAPIKey)
	dc := datahub.NewClient(cfg.DoiTAPIURL, cfg.DoiTAPIKey, "litellm-datahub-exporter/"+version, log)
	r := runner.New(cfg, lc, dc, log)

	if err := r.Startup(ctx); err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}

	if *once {
		if err := r.Cycle(ctx); err != nil {
			log.Error("cycle failed", "err", err)
			os.Exit(1)
		}

		return
	}

	metrics.Serve(cfg.MetricsAddr)
	log.Info("exporter started", "mode", cfg.Mode, "dataset", cfg.Dataset, "poll_interval", cfg.PollInterval, "metrics", cfg.MetricsAddr)

	if err := r.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("runner stopped", "err", err)
		os.Exit(1)
	}
}
