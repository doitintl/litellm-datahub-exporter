package runner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/doitintl/litellm-datahub-exporter/internal/config"
	"github.com/doitintl/litellm-datahub-exporter/internal/datahub"
	"github.com/doitintl/litellm-datahub-exporter/internal/litellm"
	"github.com/doitintl/litellm-datahub-exporter/internal/mapper"
	"github.com/doitintl/litellm-datahub-exporter/internal/metrics"
	"github.com/doitintl/litellm-datahub-exporter/internal/state"
)

type Runner struct {
	cfg     config.Config
	litellm *litellm.Client
	datahub *datahub.Client
	log     *slog.Logger
}

func New(cfg config.Config, lc *litellm.Client, dc *datahub.Client, log *slog.Logger) *Runner {
	return &Runner{cfg: cfg, litellm: lc, datahub: dc, log: log}
}

// Startup probes the proxy's capabilities and upserts the dataset
// (name + description) so it never appears description-less in the console.
func (r *Runner) Startup(ctx context.Context) error {
	caps, err := r.litellm.Probe(ctx)
	if err != nil {
		return fmt.Errorf("capability probe (GET /openapi.json): %w", err)
	}

	r.log.Info("proxy capabilities", "spend_logs", caps.SpendLogs, "per_call", caps.SpendLogsPerCall, "daily_activity", caps.DailyActivity, "paths", caps.Paths)

	if r.cfg.Mode == config.ModePerCall && !caps.SpendLogsPerCall {
		return fmt.Errorf("this LiteLLM version does not return per-request spend rows (/spend/logs has no summarize parameter); set MODE=daily, or upgrade the proxy for per-call granularity")
	}

	if r.cfg.Mode == config.ModeDaily && !caps.DailyActivity {
		return fmt.Errorf("proxy does not expose /user/daily/activity (requires LiteLLM >= ~v1.65); daily mode unsupported")
	}

	if err := r.datahub.EnsureDataset(ctx, r.cfg.Dataset, r.cfg.DatasetDescription(), r.cfg.DatasetLogoName); err != nil {
		return err
	}

	r.log.Info("dataset ensured", "dataset", r.cfg.Dataset)

	return nil
}

// Run executes cycles until ctx is done. Every cycle re-reads a trailing
// window (checkpoint- and lookback-based) so late or corrected spend rows are
// picked up, but pushes only events that are new or changed since the last
// successful cycle: DataHub appends a row per push, so re-sending the whole
// window would store every event hundreds of times.
func (r *Runner) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()

	for {
		if err := r.Cycle(ctx); err != nil {
			metrics.CycleErrors.Add(1)
			r.log.Error("cycle failed", "err", err)
		}

		select {
		case <-ticker.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (r *Runner) Cycle(ctx context.Context) error {
	metrics.Cycles.Add(1)

	now := time.Now().UTC()
	from := now.Add(-time.Duration(r.cfg.LookbackHours) * time.Hour)

	st := state.Load(r.cfg.StateFile)
	if st.LastSuccess.IsZero() && r.cfg.BackfillDays > 0 {
		from = now.AddDate(0, 0, -r.cfg.BackfillDays)
		r.log.Info("no checkpoint, backfilling", "days", r.cfg.BackfillDays)
	} else if !st.LastSuccess.IsZero() && st.LastSuccess.Before(from) {
		from = st.LastSuccess.Add(-time.Hour)
		r.log.Info("checkpoint older than lookback, widening window", "from", from)
	}

	startDate := from.Format("2006-01-02")
	endDate := now.AddDate(0, 0, 1).Format("2006-01-02")

	events, rows, err := r.collect(ctx, startDate, endDate)
	if err != nil {
		return err
	}

	metrics.RowsRead.Add(int64(rows))
	metrics.EventsRead.Add(int64(len(events)))

	// The new set is everything read this cycle, so entries for events that
	// have aged out of the window drop out on their own.
	sentPath := state.SentPath(r.cfg.StateFile)
	prev := state.LoadSent(sentPath)
	next := make(state.Sent, len(events))

	var changed []datahub.Event

	for _, e := range events {
		fp, err := fingerprint(e)
		if err != nil {
			return err
		}

		next[fp] = struct{}{}

		if _, ok := prev[fp]; !ok {
			changed = append(changed, e)
		}
	}

	if len(changed) > 0 {
		quarantined, err := r.datahub.Push(ctx, changed, r.cfg.MaxBatch)
		if err != nil {
			// prev stays on disk, so the next cycle re-sends this cycle's
			// changes: possibly twice, never skipped.
			return err
		}

		// A quarantined event was not stored; keep it out of the set so it
		// is retried, as it was before the set existed.
		rejected := make(map[string]bool, len(quarantined))
		for _, id := range quarantined {
			rejected[id] = true
		}

		for _, e := range changed {
			if rejected[e.ID] {
				fp, _ := fingerprint(e)
				delete(next, fp)
			}
		}
	}

	metrics.EventsPushed.Add(int64(len(changed)))
	metrics.EventsUnchanged.Add(int64(len(events) - len(changed)))
	metrics.Quarantined.Store(int64(r.datahub.Quarantined))
	metrics.SetLastSuccess(now)

	if err := state.SaveSent(sentPath, next); err != nil {
		r.log.Warn("sent-set save failed (safe, next cycle re-sends the window)", "err", err)
	}

	if err := state.Save(r.cfg.StateFile, state.State{LastSuccess: now}); err != nil {
		r.log.Warn("checkpoint save failed (safe, will re-export)", "err", err)
	}

	r.log.Info("cycle complete", "window", startDate+".."+endDate, "rows", rows, "events", len(events), "pushed", len(changed), "unchanged", len(events)-len(changed), "quarantined", r.datahub.Quarantined)

	return nil
}

func (r *Runner) collect(ctx context.Context, startDate, endDate string) ([]datahub.Event, int, error) {
	opts := mapper.Options{
		Dataset:         r.cfg.Dataset,
		FeatureMetaKey:  r.cfg.FeatureMetaKey,
		TraceMetaKey:    r.cfg.TraceMetaKey,
		EmitTraceLabels: r.cfg.EmitTraceLabels,
		UserEmailSource: r.cfg.UserEmailSource,
		TagDenyPrefixes: r.cfg.TagDenyPrefixes,
	}

	if r.cfg.Mode == config.ModeDaily {
		results, err := r.litellm.UserDailyActivity(ctx, startDate, endDate)
		if err != nil {
			return nil, 0, err
		}

		return mapper.DailyToEvents(results, opts), len(results), nil
	}

	rows, err := r.litellm.SpendLogs(ctx, startDate, endDate)
	if err != nil {
		return nil, 0, err
	}

	events := make([]datahub.Event, 0, len(rows))

	for _, row := range rows {
		event, err := mapper.SpendRowToEvent(row, opts)
		if err != nil {
			r.log.Warn("row skipped", "err", err)
			continue
		}

		events = append(events, event)
	}

	return events, len(rows), nil
}

// fingerprint identifies an event's id and full content, so any change to a
// cost, a token count or a label yields a new fingerprint and a re-push.
func fingerprint(e datahub.Event) ([16]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return [16]byte{}, err
	}

	sum := sha256.Sum256(data)

	return [16]byte(sum[:16]), nil
}
