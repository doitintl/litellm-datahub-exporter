// Package litellm reads spend data from a LiteLLM proxy's HTTP APIs.
//
// Privacy contract: the response structs below are a strict field allowlist.
// Spend-log rows also carry `messages`, `response`, and `proxy_server_request`
// fields (populated when the customer enables prompt storage); they have no
// struct fields here, so they are dropped at JSON decode time and can never
// be serialized onward.
package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

func NewClient(baseURL, apiKey string) *Client {
	return &Client{baseURL: baseURL, apiKey: apiKey, http: &http.Client{Timeout: 120 * time.Second}}
}

type SpendRow struct {
	RequestID         string   `json:"request_id"`
	CallType          string   `json:"call_type"`
	APIKeyHash        string   `json:"api_key"`
	Spend             float64  `json:"spend"`
	TotalTokens       float64  `json:"total_tokens"`
	PromptTokens      float64  `json:"prompt_tokens"`
	CompletionTokens  float64  `json:"completion_tokens"`
	StartTime         string   `json:"startTime"`
	Model             string   `json:"model"`
	ModelGroup        string   `json:"model_group"`
	CustomLLMProvider string   `json:"custom_llm_provider"`
	EndUser           string   `json:"end_user"`
	TeamID            string   `json:"team_id"`
	RequestTags       []string `json:"request_tags"`
	Metadata          struct {
		UserAPIKeyAlias     string         `json:"user_api_key_alias"`
		UserAPIKeyTeamID    string         `json:"user_api_key_team_id"`
		UserAPIKeyUserID    string         `json:"user_api_key_user_id"`
		UserAPIKeyUserEmail string         `json:"user_api_key_user_email"`
		SpendLogsMeta       map[string]any `json:"spend_logs_metadata"`
	} `json:"metadata"`
}

func (c *Client) SpendLogs(ctx context.Context, startDate, endDate string) ([]SpendRow, error) {
	q := url.Values{"start_date": {startDate}, "end_date": {endDate}, "summarize": {"false"}}

	var rows []SpendRow
	if err := c.get(ctx, "/spend/logs?"+q.Encode(), &rows); err != nil {
		return nil, err
	}

	return rows, nil
}

type DailyMetrics struct {
	Spend              float64 `json:"spend"`
	PromptTokens       float64 `json:"prompt_tokens"`
	CompletionTokens   float64 `json:"completion_tokens"`
	TotalTokens        float64 `json:"total_tokens"`
	APIRequests        float64 `json:"api_requests"`
	SuccessfulRequests float64 `json:"successful_requests"`
	FailedRequests     float64 `json:"failed_requests"`
}

type DailyAPIKeyCell struct {
	Metrics  DailyMetrics `json:"metrics"`
	Metadata struct {
		KeyAlias string `json:"key_alias"`
		TeamID   string `json:"team_id"`
	} `json:"metadata"`
}

type DailyModelCell struct {
	Metrics         DailyMetrics               `json:"metrics"`
	APIKeyBreakdown map[string]DailyAPIKeyCell `json:"api_key_breakdown"`
}

// UnmarshalJSON accepts both daily-breakdown shapes: modern LiteLLM nests
// metrics under "metrics" with an "api_key_breakdown" map; older versions
// (v1.65-era) put the metric fields directly on the cell.
func (c *DailyModelCell) UnmarshalJSON(data []byte) error {
	type modern DailyModelCell

	var m modern
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}

	if m.Metrics == (DailyMetrics{}) && len(m.APIKeyBreakdown) == 0 {
		if err := json.Unmarshal(data, &m.Metrics); err != nil {
			return err
		}
	}

	*c = DailyModelCell(m)

	return nil
}

type DailyResult struct {
	Date      string `json:"date"`
	Breakdown struct {
		Models map[string]DailyModelCell `json:"models"`
	} `json:"breakdown"`
}

type dailyResponse struct {
	Results  []DailyResult `json:"results"`
	Metadata struct {
		TotalPages int `json:"total_pages"`
	} `json:"metadata"`
}

func (c *Client) UserDailyActivity(ctx context.Context, startDate, endDate string) ([]DailyResult, error) {
	var all []DailyResult

	for page := 1; ; page++ {
		q := url.Values{
			"start_date": {startDate},
			"end_date":   {endDate},
			"page":       {fmt.Sprint(page)},
			"page_size":  {"100"},
			// Modern proxies exclude the current UTC day by default; older
			// versions ignore the unknown parameter.
			"include_current_utc_day": {"true"},
		}

		var resp dailyResponse
		if err := c.get(ctx, "/user/daily/activity?"+q.Encode(), &resp); err != nil {
			return nil, err
		}

		all = append(all, resp.Results...)

		if resp.Metadata.TotalPages == 0 || page >= resp.Metadata.TotalPages {
			return all, nil
		}
	}
}

type Capabilities struct {
	SpendLogs bool
	// SpendLogsPerCall is true when /spend/logs supports the summarize
	// parameter — the marker for per-request rows. Older proxies (v1.65-era)
	// expose /spend/logs but only return day-aggregate rows without
	// request ids, which per_call mode cannot use.
	SpendLogsPerCall bool
	DailyActivity    bool
	Paths            int
}

func (c *Client) Probe(ctx context.Context) (Capabilities, error) {
	var spec struct {
		Paths map[string]struct {
			Get struct {
				Parameters []struct {
					Name string `json:"name"`
				} `json:"parameters"`
			} `json:"get"`
		} `json:"paths"`
	}

	if err := c.get(ctx, "/openapi.json", &spec); err != nil {
		return Capabilities{}, err
	}

	spendLogs, hasSpendLogs := spec.Paths["/spend/logs"]
	_, daily := spec.Paths["/user/daily/activity"]

	perCall := false
	for _, p := range spendLogs.Get.Parameters {
		if p.Name == "summarize" {
			perCall = true
		}
	}

	return Capabilities{SpendLogs: hasSpendLogs, SpendLogsPerCall: perCall, DailyActivity: daily, Paths: len(spec.Paths)}, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("litellm GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		return fmt.Errorf("litellm GET %s: read: %w", path, err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("litellm GET %s: HTTP %d: %.300s", path, resp.StatusCode, string(body))
	}

	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("litellm GET %s: decode: %w", path, err)
	}

	return nil
}
