package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Mode string

const (
	ModePerCall Mode = "per_call"
	ModeDaily   Mode = "daily"
)

type Config struct {
	LiteLLMBaseURL  string
	LiteLLMAPIKey   string
	DoiTAPIURL      string
	DoiTAPIKey      string
	Dataset         string
	DatasetLogoName string
	Mode            Mode
	PollInterval    time.Duration
	LookbackHours   int
	BackfillDays    int
	FeatureMetaKey  string
	TraceMetaKey    string
	EmitTraceLabels bool
	TagDenyPrefixes []string
	StateFile       string
	MetricsAddr     string
	MaxBatch        int
}

const datasetDescription = "Estimated LLM spend per model call, pushed from the self-hosted LiteLLM proxy by the DoiT litellm-datahub-exporter. Labeled by provider, model, virtual key, team, feature, and end customer. Cost basis: LiteLLM price map (showback, not billable actuals)."

func (c Config) DatasetDescription() string { return datasetDescription }

func FromEnv() (Config, error) {
	c := Config{
		LiteLLMBaseURL:  strings.TrimRight(getenv("LITELLM_BASE_URL", "http://localhost:4000"), "/"),
		LiteLLMAPIKey:   os.Getenv("LITELLM_API_KEY"),
		DoiTAPIURL:      strings.TrimRight(getenv("DOIT_API_URL", "https://api.doit.com"), "/"),
		DoiTAPIKey:      os.Getenv("DOIT_API_KEY"),
		Dataset:         getenv("DATASET", "LiteLLM"),
		DatasetLogoName: getenv("DATASET_LOGO_NAME", "litellm"),
		Mode:            Mode(getenv("MODE", string(ModePerCall))),
		FeatureMetaKey:  getenv("FEATURE_METADATA_KEY", "feature"),
		TraceMetaKey:    getenv("TRACE_METADATA_KEY", "parent_trace_id"),
		EmitTraceLabels: getenv("EMIT_TRACE_LABELS", "false") == "true",
		TagDenyPrefixes: strings.Split(getenv("TAG_DENY_PREFIXES", "User-Agent"), ","),
		StateFile:       getenv("STATE_FILE", "state.json"),
		MetricsAddr:     getenv("METRICS_ADDR", ":9464"),
	}

	var err error
	if c.PollInterval, err = time.ParseDuration(getenv("POLL_INTERVAL", "5m")); err != nil {
		return c, fmt.Errorf("POLL_INTERVAL: %w", err)
	}
	if c.LookbackHours, err = atoi("LOOKBACK_HOURS", "48"); err != nil {
		return c, err
	}
	if c.BackfillDays, err = atoi("BACKFILL_DAYS", "0"); err != nil {
		return c, err
	}
	if c.MaxBatch, err = atoi("MAX_BATCH", "5000"); err != nil {
		return c, err
	}
	if c.MaxBatch < 1 || c.MaxBatch > 50000 {
		return c, fmt.Errorf("MAX_BATCH must be within [1, 50000] (DataHub caps a request at 50000 events)")
	}
	if c.LiteLLMAPIKey == "" {
		return c, fmt.Errorf("LITELLM_API_KEY is required")
	}
	if c.DoiTAPIKey == "" {
		return c, fmt.Errorf("DOIT_API_KEY is required")
	}
	if c.Mode != ModePerCall && c.Mode != ModeDaily {
		return c, fmt.Errorf("MODE must be %q or %q", ModePerCall, ModeDaily)
	}

	return c, nil
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}

func atoi(key, def string) (int, error) {
	n, err := strconv.Atoi(getenv(key, def))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}

	return n, nil
}
