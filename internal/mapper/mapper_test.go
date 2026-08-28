package mapper

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/doitintl/litellm-datahub-exporter/internal/datahub"
	"github.com/doitintl/litellm-datahub-exporter/internal/litellm"
)

// Captured from a live LiteLLM v1.98.0 /spend/logs row (mock models, custom
// pricing). The extra fields (messages, response, cost_breakdown, ...) are
// intentionally present to prove the allowlist decode drops them.
const liveSpendRow = `{
  "request_id": "chatcmpl-b4316a00-a559-4169-8e63-37aa94425808",
  "call_type": "acompletion",
  "api_key": "2b6d8ae50f144aa3ef3475c57d89f8c6bc49fd64de61d65fde4bd1fff1f24983",
  "spend": 0.00033,
  "total_tokens": 30,
  "prompt_tokens": 10,
  "completion_tokens": 20,
  "startTime": "2026-08-27T12:37:44.874000Z",
  "model": "anthropic/claude-3-5-sonnet-20241022",
  "model_group": "claude-sonnet-5",
  "custom_llm_provider": "anthropic",
  "end_user": "end-user-731",
  "team_id": "c77c7db3-1902-4079-bc2f-e74ab4c03275",
  "request_tags": ["env:prod", "User-Agent: curl", "User-Agent: curl/8.7.1"],
  "messages": [{"role": "user", "content": "SECRET PROMPT"}],
  "response": {"choices": [{"message": {"content": "SECRET COMPLETION"}}]},
  "metadata": {
    "user_api_key_alias": "monarch-advice-chat",
    "user_api_key_team_id": "c77c7db3-1902-4079-bc2f-e74ab4c03275",
    "spend_logs_metadata": {"feature": "advice-chat", "parent_trace_id": "trace-xyz-9"}
  }
}`

func opts() Options {
	return Options{
		Dataset:         "LiteLLM",
		FeatureMetaKey:  "feature",
		TraceMetaKey:    "parent_trace_id",
		TagDenyPrefixes: []string{"User-Agent"},
	}
}

func decodeLiveRow(t *testing.T) litellm.SpendRow {
	t.Helper()

	var row litellm.SpendRow
	if err := json.Unmarshal([]byte(liveSpendRow), &row); err != nil {
		t.Fatal(err)
	}

	return row
}

func labels(e datahub.Event) map[string]string {
	m := map[string]string{}
	for _, d := range e.Dimensions {
		m[d.Type+"/"+d.Key] = d.Value
	}

	return m
}

func TestSpendRowMapping(t *testing.T) {
	event, err := SpendRowToEvent(decodeLiveRow(t), opts())
	if err != nil {
		t.Fatal(err)
	}

	if event.ID != "litellm-chatcmpl-b4316a00-a559-4169-8e63-37aa94425808" {
		t.Errorf("id = %q", event.ID)
	}

	if event.Time != "2026-08-27T12:37:44.874000Z" {
		t.Errorf("time = %q", event.Time)
	}

	want := map[string]string{
		"fixed/service_description":             "anthropic",
		"fixed/sku_description":                 "claude-sonnet-5",
		"label/provider":                        "anthropic",
		"label/model":                           "claude-sonnet-5",
		"label/virtual_key":                     "monarch-advice-chat",
		"label/team":                            "c77c7db3-1902-4079-bc2f-e74ab4c03275",
		"label/customer":                        "end-user-731",
		"label/feature":                         "advice-chat",
		"label/tag":                             "env:prod",
		"system_label/cost_basis":               "estimated",
		"system_label/litellm/call_type":        "acompletion",
		"system_label/litellm/underlying_model": "anthropic/claude-3-5-sonnet-20241022",
		"system_label/genai/genai_spend":        "true",
		"system_label/genai/model":              "claude-3-5-sonnet-20241022",
		"system_label/genai/model_family":       "Claude",
		"system_label/genai/is_model_serving":   "true",
		"system_label/genai/consumption_model":  "PAYG",
		"system_label/genai/user_id":            "end-user-731",
		"system_label/genai/api_key_name":       "monarch-advice-chat",
		"system_label/genai/feature":            "advice-chat",
	}

	got := labels(event)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("dimension %s = %q, want %q", k, got[k], v)
		}
	}

	if len(got) != len(want) {
		t.Errorf("got %d dimensions, want %d: %v", len(got), len(want), got)
	}

	metrics := map[string]float64{}
	for _, m := range event.Metrics {
		metrics[m.Type] = m.Value
	}

	if metrics["cost"] != 0.00033 || metrics["usage"] != 30 || metrics["Prompt Tokens"] != 10 || metrics["Completion Tokens"] != 20 {
		t.Errorf("metrics = %v", metrics)
	}
}

func TestPromptContentNeverSerialized(t *testing.T) {
	event, err := SpendRowToEvent(decodeLiveRow(t), opts())
	if err != nil {
		t.Fatal(err)
	}

	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(payload), "SECRET") {
		t.Fatalf("prompt content leaked into event payload: %s", payload)
	}
}

func TestTraceLabelsOffByDefault(t *testing.T) {
	event, err := SpendRowToEvent(decodeLiveRow(t), opts())
	if err != nil {
		t.Fatal(err)
	}

	got := labels(event)
	if _, ok := got["label/request_id"]; ok {
		t.Error("request_id label emitted without EMIT_TRACE_LABELS")
	}

	o := opts()
	o.EmitTraceLabels = true

	event, err = SpendRowToEvent(decodeLiveRow(t), o)
	if err != nil {
		t.Fatal(err)
	}

	got = labels(event)
	if got["label/request_id"] == "" || got["label/parent_trace_id"] != "trace-xyz-9" {
		t.Errorf("trace labels missing when enabled: %v", got)
	}
}

func TestModelFamily(t *testing.T) {
	cases := map[string]string{
		"anthropic/claude-3-5-sonnet-20241022":  "Claude",
		"openai/gpt-4o-mini":                    "GPT",
		"gpt-oss-120b":                          "GPT OSS",
		"mistralai/Mixtral-8x7B-Instruct-v0.1":  "Mixtral",
		"mistral/mistral-large-latest":          "Mistral",
		"gemini/gemini-2.5-pro":                 "Gemini",
		"bedrock/meta.llama3-70b-instruct-v1:0": "Meta Llama",
		"deepseek/deepseek-chat":                "DeepSeek",
		"xai/grok-4":                            "Grok",
		"openai/text-embedding-3-small":         "Embedding",
		"my-org/fine-tuned-internal-model":      "Custom Model",
	}

	for model, want := range cases {
		if got := modelFamily(model); got != want {
			t.Errorf("modelFamily(%q) = %q, want %q", model, got, want)
		}
	}
}

func TestDatasetNameMatchesProviderPattern(t *testing.T) {
	// DataHub's provider (dataset) validation pattern, from the DoiT OpenAPI spec.
	pattern := regexp.MustCompile(`^[a-zA-Z0-9_-]+( [a-zA-Z0-9_-]+)*$`)
	if !pattern.MatchString(opts().Dataset) {
		t.Errorf("default dataset name %q violates the DataHub provider pattern", opts().Dataset)
	}
}

func TestSpendRowDeterminism(t *testing.T) {
	a, err := SpendRowToEvent(decodeLiveRow(t), opts())
	if err != nil {
		t.Fatal(err)
	}

	b, err := SpendRowToEvent(decodeLiveRow(t), opts())
	if err != nil {
		t.Fatal(err)
	}

	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)

	if string(aj) != string(bj) {
		t.Error("mapping is not deterministic")
	}
}

func TestStartTimeFormats(t *testing.T) {
	for _, s := range []string{
		"2026-08-27T12:37:44.874000Z",
		"2026-08-27T12:37:44Z",
		"2026-08-27T12:37:44.874000",
		"2026-08-27 12:37:44.874000",
	} {
		if _, err := parseStartTime(s); err != nil {
			t.Errorf("parseStartTime(%q): %v", s, err)
		}
	}

	if _, err := parseStartTime("yesterday"); err == nil {
		t.Error("expected error for junk timestamp")
	}
}

func TestDailyToEvents(t *testing.T) {
	daily := []litellm.DailyResult{{Date: "2026-08-27"}}
	daily[0].Breakdown.Models = map[string]litellm.DailyModelCell{
		"anthropic/claude-3-5-sonnet-20241022": {
			APIKeyBreakdown: map[string]litellm.DailyAPIKeyCell{
				"hash1": func() litellm.DailyAPIKeyCell {
					c := litellm.DailyAPIKeyCell{
						Metrics: litellm.DailyMetrics{Spend: 0.00165, TotalTokens: 150, PromptTokens: 50, CompletionTokens: 100, APIRequests: 5},
					}
					c.Metadata.KeyAlias = "monarch-advice-chat"
					c.Metadata.TeamID = "team-1"

					return c
				}(),
			},
		},
	}

	events := DailyToEvents(daily, opts())
	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}

	e := events[0]
	if !strings.HasPrefix(e.ID, "litellm-daily-") || len(e.ID) != len("litellm-daily-")+32 {
		t.Errorf("daily id = %q", e.ID)
	}

	if e.Time != "2026-08-27T00:00:00Z" {
		t.Errorf("daily time = %q", e.Time)
	}

	got := labels(e)
	if got["label/provider"] != "anthropic" || got["label/virtual_key"] != "monarch-advice-chat" || got["label/team"] != "team-1" {
		t.Errorf("daily labels = %v", got)
	}

	if got["system_label/genai/genai_spend"] != "true" || got["system_label/genai/model_family"] != "Claude" || got["system_label/genai/consumption_model"] != "PAYG" {
		t.Errorf("daily genai labels = %v", got)
	}

	again := DailyToEvents(daily, opts())
	if again[0].ID != e.ID {
		t.Error("daily id not deterministic")
	}
}
