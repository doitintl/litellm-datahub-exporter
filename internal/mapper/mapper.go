// Package mapper turns LiteLLM spend data into DataHub events.
//
// The mapping is the one specified (and lab-validated against DoiT's
// ingestion validator) in the omni spec
// specs/CloudIntelligence/litellm-datahub-spend-integration/TECH.md §4.2.
package mapper

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/doitintl/litellm-datahub-exporter/internal/datahub"
	"github.com/doitintl/litellm-datahub-exporter/internal/litellm"
)

type Options struct {
	Dataset         string
	FeatureMetaKey  string
	TraceMetaKey    string
	EmitTraceLabels bool
	TagDenyPrefixes []string
}

func SpendRowToEvent(r litellm.SpendRow, o Options) (datahub.Event, error) {
	if r.RequestID == "" {
		return datahub.Event{}, fmt.Errorf("spend row without request_id")
	}

	ts, err := parseStartTime(r.StartTime)
	if err != nil {
		return datahub.Event{}, fmt.Errorf("spend row %s: %w", r.RequestID, err)
	}

	model := r.ModelGroup
	if model == "" {
		model = r.Model
	}

	dims := []datahub.Dimension{
		{Key: "service_description", Type: "fixed", Value: r.CustomLLMProvider},
		{Key: "sku_description", Type: "fixed", Value: model},
		{Key: "provider", Type: "label", Value: r.CustomLLMProvider},
		{Key: "model", Type: "label", Value: model},
		{Key: "virtual_key", Type: "label", Value: coalesce(r.Metadata.UserAPIKeyAlias, r.APIKeyHash)},
		{Key: "cost_basis", Type: "system_label", Value: "estimated"},
		{Key: "litellm/call_type", Type: "system_label", Value: r.CallType},
		{Key: "litellm/underlying_model", Type: "system_label", Value: r.Model},
	}

	if team := coalesce(r.TeamID, r.Metadata.UserAPIKeyTeamID); team != "" {
		dims = append(dims, datahub.Dimension{Key: "team", Type: "label", Value: team})
	}

	if r.EndUser != "" {
		dims = append(dims, datahub.Dimension{Key: "customer", Type: "label", Value: r.EndUser})
	}

	if feature, ok := r.Metadata.SpendLogsMeta[o.FeatureMetaKey].(string); ok && feature != "" {
		dims = append(dims, datahub.Dimension{Key: "feature", Type: "label", Value: feature})
	}

	if tags := filterTags(r.RequestTags, o.TagDenyPrefixes); len(tags) > 0 {
		dims = append(dims, datahub.Dimension{Key: "tag", Type: "label", Value: tags[0]})
	}

	feature, _ := r.Metadata.SpendLogsMeta[o.FeatureMetaKey].(string)
	dims = append(dims, genaiDimensions(
		r.Model,
		coalesce(r.EndUser, r.Metadata.UserAPIKeyUserID),
		r.Metadata.UserAPIKeyUserEmail,
		r.Metadata.UserAPIKeyAlias,
		feature,
	)...)

	if o.EmitTraceLabels {
		dims = append(dims, datahub.Dimension{Key: "request_id", Type: "label", Value: r.RequestID})

		if trace, ok := r.Metadata.SpendLogsMeta[o.TraceMetaKey].(string); ok && trace != "" {
			dims = append(dims, datahub.Dimension{Key: "parent_trace_id", Type: "label", Value: trace})
		}
	}

	return datahub.Event{
		Provider:   o.Dataset,
		ID:         "litellm-" + r.RequestID,
		Time:       ts.UTC().Format("2006-01-02T15:04:05.000000Z"),
		Dimensions: dropEmpty(dims),
		Metrics: []datahub.Metric{
			{Type: "cost", Value: r.Spend},
			{Type: "usage", Value: r.TotalTokens},
			{Type: "Prompt Tokens", Value: r.PromptTokens},
			{Type: "Completion Tokens", Value: r.CompletionTokens},
		},
	}, nil
}

func DailyToEvents(results []litellm.DailyResult, o Options) []datahub.Event {
	var events []datahub.Event

	for _, day := range results {
		for model, modelCell := range day.Breakdown.Models {
			keyCells := modelCell.APIKeyBreakdown
			if len(keyCells) == 0 {
				// v1.65-era daily shape has no per-key nesting under models;
				// emit one event per (day x model) from the flat cell.
				keyCells = map[string]litellm.DailyAPIKeyCell{"": {Metrics: modelCell.Metrics}}
			}

			for keyHash, keyCell := range keyCells {
				provider, _, hasProvider := strings.Cut(model, "/")
				if !hasProvider {
					provider = ""
				}

				sum := sha256.Sum256([]byte(day.Date + "|" + model + "|" + keyHash))

				dims := []datahub.Dimension{
					{Key: "service_description", Type: "fixed", Value: provider},
					{Key: "sku_description", Type: "fixed", Value: model},
					{Key: "provider", Type: "label", Value: provider},
					{Key: "model", Type: "label", Value: model},
					{Key: "virtual_key", Type: "label", Value: coalesce(keyCell.Metadata.KeyAlias, keyHash)},
					{Key: "team", Type: "label", Value: keyCell.Metadata.TeamID},
					{Key: "cost_basis", Type: "system_label", Value: "estimated"},
				}

				dims = append(dims, genaiDimensions(model, "", "", keyCell.Metadata.KeyAlias, "")...)

				events = append(events, datahub.Event{
					Provider:   o.Dataset,
					ID:         "litellm-daily-" + hex.EncodeToString(sum[:])[:32],
					Time:       day.Date + "T00:00:00Z",
					Dimensions: dropEmpty(dims),
					Metrics: []datahub.Metric{
						{Type: "cost", Value: keyCell.Metrics.Spend},
						{Type: "usage", Value: keyCell.Metrics.TotalTokens},
						{Type: "Prompt Tokens", Value: keyCell.Metrics.PromptTokens},
						{Type: "Completion Tokens", Value: keyCell.Metrics.CompletionTokens},
						{Type: "Requests", Value: keyCell.Metrics.APIRequests},
						{Type: "Failed Requests", Value: keyCell.Metrics.FailedRequests},
					},
				})
			}
		}
	}

	return events
}

// parseStartTime accepts the timestamp shapes LiteLLM emits across versions:
// RFC3339 with or without fractional seconds, and the same with a space
// separator and no zone (treated as UTC).
func parseStartTime(s string) (time.Time, error) {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.999999",
		"2006-01-02 15:04:05.999999",
	} {
		if ts, err := time.Parse(layout, s); err == nil {
			return ts, nil
		}
	}

	return time.Time{}, fmt.Errorf("unparseable startTime %q", s)
}

func filterTags(tags, denyPrefixes []string) []string {
	var out []string

tagLoop:
	for _, tag := range tags {
		for _, deny := range denyPrefixes {
			if deny != "" && strings.HasPrefix(tag, deny) {
				continue tagLoop
			}
		}

		out = append(out, tag)
	}

	return out
}

func dropEmpty(dims []datahub.Dimension) []datahub.Dimension {
	out := dims[:0]

	for _, d := range dims {
		if d.Value != "" {
			out = append(out, d)
		}
	}

	return out
}

func coalesce(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}
