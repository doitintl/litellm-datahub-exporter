package runner

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/doitintl/litellm-datahub-exporter/internal/config"
	"github.com/doitintl/litellm-datahub-exporter/internal/datahub"
	"github.com/doitintl/litellm-datahub-exporter/internal/litellm"
	"github.com/doitintl/litellm-datahub-exporter/internal/state"
)

// fakeProxy serves /spend/logs and /user/daily/activity from mutable fixtures.
type fakeProxy struct {
	mu    sync.Mutex
	rows  []map[string]any
	daily []map[string]any
}

func (p *fakeProxy) setRows(rows ...map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rows = rows
}

func (p *fakeProxy) setDaily(days ...map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.daily = days
}

func (p *fakeProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch r.URL.Path {
	case "/spend/logs":
		_ = json.NewEncoder(w).Encode(p.rows)
	case "/user/daily/activity":
		_ = json.NewEncoder(w).Encode(map[string]any{"results": p.daily, "metadata": map[string]any{"total_pages": 1}})
	default:
		http.NotFound(w, r)
	}
}

// fakeDataHub records every pushed event id per request and can reject
// chosen ids (400, quarantine) or fail whole requests.
type fakeDataHub struct {
	mu      sync.Mutex
	pushes  [][]string
	reject  map[string]bool
	failing bool
}

func (d *fakeDataHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Events []datahub.Event `json:"events"`
	}

	_ = json.NewDecoder(r.Body).Decode(&body)

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.failing {
		// 409 is a non-retried, non-bisected failure: the cycle errors at once.
		w.WriteHeader(http.StatusConflict)
		return
	}

	for _, e := range body.Events {
		if d.reject[e.ID] {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
	}

	ids := make([]string, 0, len(body.Events))
	for _, e := range body.Events {
		ids = append(ids, e.ID)
	}

	d.pushes = append(d.pushes, ids)
	w.WriteHeader(http.StatusCreated)
}

// take returns the ids pushed since the last call.
func (d *fakeDataHub) take() []string {
	d.mu.Lock()
	defer d.mu.Unlock()

	var ids []string
	for _, p := range d.pushes {
		ids = append(ids, p...)
	}

	d.pushes = nil

	return ids
}

type harness struct {
	proxy   *fakeProxy
	datahub *fakeDataHub
	runner  *Runner
	cfg     config.Config
}

func newHarness(t *testing.T, mode config.Mode) *harness {
	t.Helper()

	proxy := &fakeProxy{}
	dh := &fakeDataHub{reject: map[string]bool{}}

	ps := httptest.NewServer(proxy)
	ds := httptest.NewServer(dh)
	t.Cleanup(ps.Close)
	t.Cleanup(ds.Close)

	cfg := config.Config{
		Dataset:       "LiteLLM",
		Mode:          mode,
		LookbackHours: 48,
		StateFile:     filepath.Join(t.TempDir(), "state.json"),
		MaxBatch:      5000,
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := New(cfg, litellm.NewClient(ps.URL, "sk-test"), datahub.NewClient(ds.URL, "test", "test", log), log)

	return &harness{proxy: proxy, datahub: dh, runner: r, cfg: cfg}
}

func (h *harness) cycle(t *testing.T) []string {
	t.Helper()

	if err := h.runner.Cycle(context.Background()); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	return h.datahub.take()
}

// base is fixed so a row rebuilt from the same arguments is identical.
var base = time.Now().UTC().Truncate(time.Second)

func row(id string, spend float64, age time.Duration) map[string]any {
	return map[string]any{
		"request_id":          id,
		"call_type":           "acompletion",
		"spend":               spend,
		"total_tokens":        30,
		"prompt_tokens":       10,
		"completion_tokens":   20,
		"startTime":           base.Add(-age).Format(time.RFC3339Nano),
		"model":               "anthropic/claude-test",
		"model_group":         "test-model",
		"custom_llm_provider": "anthropic",
	}
}

func assertIDs(t *testing.T, got []string, want ...string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("pushed %v, want %v", got, want)
	}

	seen := map[string]bool{}
	for _, id := range got {
		seen[id] = true
	}

	for _, id := range want {
		if !seen[id] {
			t.Fatalf("pushed %v, want %v", got, want)
		}
	}
}

func TestUnchangedEventIsNotResent(t *testing.T) {
	h := newHarness(t, config.ModePerCall)
	h.proxy.setRows(row("req-a", 0.01, time.Hour), row("req-b", 0.02, 2*time.Hour))

	assertIDs(t, h.cycle(t), "litellm-req-a", "litellm-req-b")
	assertIDs(t, h.cycle(t))
	assertIDs(t, h.cycle(t))
}

func TestChangedEventIsResent(t *testing.T) {
	h := newHarness(t, config.ModePerCall)
	h.proxy.setRows(row("req-a", 0.01, time.Hour), row("req-b", 0.02, time.Hour))
	h.cycle(t)

	// LiteLLM corrected req-b's spend after the first read.
	h.proxy.setRows(row("req-a", 0.01, time.Hour), row("req-b", 0.05, time.Hour))
	assertIDs(t, h.cycle(t), "litellm-req-b")
}

func TestLateRowInsideLookbackIsSent(t *testing.T) {
	h := newHarness(t, config.ModePerCall)
	h.proxy.setRows(row("req-a", 0.01, time.Hour))
	h.cycle(t)

	// A row written late, timestamped well before the previous cycle.
	h.proxy.setRows(row("req-a", 0.01, time.Hour), row("req-late", 0.03, 30*time.Hour))
	assertIDs(t, h.cycle(t), "litellm-req-late")
}

func TestLostStateResendsWindow(t *testing.T) {
	h := newHarness(t, config.ModePerCall)
	h.proxy.setRows(row("req-a", 0.01, time.Hour), row("req-b", 0.02, time.Hour))
	h.cycle(t)

	for _, p := range []string{h.cfg.StateFile, state.SentPath(h.cfg.StateFile)} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}

	assertIDs(t, h.cycle(t), "litellm-req-a", "litellm-req-b")
	assertIDs(t, h.cycle(t))
}

func TestCorruptSentFileResendsWindow(t *testing.T) {
	h := newHarness(t, config.ModePerCall)
	h.proxy.setRows(row("req-a", 0.01, time.Hour))
	h.cycle(t)

	if err := os.WriteFile(state.SentPath(h.cfg.StateFile), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}

	assertIDs(t, h.cycle(t), "litellm-req-a")
}

func TestFailedPushIsRetriedNextCycle(t *testing.T) {
	h := newHarness(t, config.ModePerCall)
	h.proxy.setRows(row("req-a", 0.01, time.Hour))
	h.cycle(t)

	h.proxy.setRows(row("req-a", 0.01, time.Hour), row("req-b", 0.02, time.Hour))
	h.datahub.failing = true

	if err := h.runner.Cycle(context.Background()); err == nil {
		t.Fatal("cycle succeeded against a failing DataHub")
	}

	h.datahub.failing = false
	assertIDs(t, h.cycle(t), "litellm-req-b")
}

func TestQuarantinedEventIsRetried(t *testing.T) {
	h := newHarness(t, config.ModePerCall)
	h.proxy.setRows(row("req-a", 0.01, time.Hour), row("req-bad", 0.02, time.Hour))
	h.datahub.reject["litellm-req-bad"] = true

	assertIDs(t, h.cycle(t), "litellm-req-a")

	h.datahub.reject = map[string]bool{}
	assertIDs(t, h.cycle(t), "litellm-req-bad")
}

func TestDailyModeResendsOnlyTheGrowingDay(t *testing.T) {
	h := newHarness(t, config.ModeDaily)

	day := func(date string, spend float64) map[string]any {
		return map[string]any{
			"date": date,
			"breakdown": map[string]any{"models": map[string]any{
				"anthropic/claude-test": map[string]any{
					"metrics": map[string]any{"spend": spend, "api_requests": 1},
					"api_key_breakdown": map[string]any{
						"hash-1": map[string]any{"metrics": map[string]any{"spend": spend, "api_requests": 1}},
					},
				},
			}},
		}
	}

	yesterday := time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	today := time.Now().UTC().Format("2006-01-02")

	h.proxy.setDaily(day(yesterday, 1.0), day(today, 0.5))
	if got := h.cycle(t); len(got) != 2 {
		t.Fatalf("first cycle pushed %v, want both days", got)
	}

	assertIDs(t, h.cycle(t))

	h.proxy.setDaily(day(yesterday, 1.0), day(today, 0.75))
	got := h.cycle(t)
	if len(got) != 1 {
		t.Fatalf("pushed %v, want only today's aggregate", got)
	}
}
