package litellm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Pagination contract for /user/daily/activity: page/page_size params,
// metadata.total_pages drives the loop, absent total_pages means single page.
func TestUserDailyActivityPagination(t *testing.T) {
	var requestedPages []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/daily/activity" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}

		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q", got)
		}

		page := r.URL.Query().Get("page")
		requestedPages = append(requestedPages, page)

		fmt.Fprintf(w, `{
			"results": [{"date": "2026-08-2%s", "breakdown": {"models": {}}}],
			"metadata": {"total_pages": 3, "page": %s}
		}`, page, page)
	}))
	defer server.Close()

	results, err := NewClient(server.URL, "test-key").UserDailyActivity(context.Background(), "2026-08-20", "2026-08-28")
	if err != nil {
		t.Fatal(err)
	}

	if len(results) != 3 {
		t.Errorf("got %d results, want 3 (one per page)", len(results))
	}

	if len(requestedPages) != 3 || requestedPages[0] != "1" || requestedPages[2] != "3" {
		t.Errorf("requested pages = %v, want [1 2 3]", requestedPages)
	}
}

// Older LiteLLM versions omit metadata.total_pages — the client must treat
// the response as a single page instead of looping forever.
func TestUserDailyActivityNoTotalPages(t *testing.T) {
	calls := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results":  []map[string]any{{"date": "2026-08-27"}},
			"metadata": map[string]any{"total_spend": 1.23},
		})
	}))
	defer server.Close()

	results, err := NewClient(server.URL, "test-key").UserDailyActivity(context.Background(), "2026-08-27", "2026-08-28")
	if err != nil {
		t.Fatal(err)
	}

	if calls != 1 || len(results) != 1 {
		t.Errorf("calls = %d, results = %d; want 1 and 1", calls, len(results))
	}
}

// v1.65-era daily cells are flat metric objects with no nested "metrics"
// key and no api_key_breakdown; both shapes must decode.
func TestDailyModelCellBothShapes(t *testing.T) {
	var modern DailyModelCell
	if err := json.Unmarshal([]byte(`{"metrics":{"spend":1.5,"total_tokens":10},"api_key_breakdown":{"h":{"metrics":{"spend":1.5}}}}`), &modern); err != nil {
		t.Fatal(err)
	}

	if modern.Metrics.Spend != 1.5 || len(modern.APIKeyBreakdown) != 1 {
		t.Errorf("modern shape decoded wrong: %+v", modern)
	}

	var legacy DailyModelCell
	if err := json.Unmarshal([]byte(`{"spend":0.99,"prompt_tokens":30,"completion_tokens":60,"total_tokens":90,"api_requests":3}`), &legacy); err != nil {
		t.Fatal(err)
	}

	if legacy.Metrics.Spend != 0.99 || legacy.Metrics.TotalTokens != 90 || len(legacy.APIKeyBreakdown) != 0 {
		t.Errorf("legacy shape decoded wrong: %+v", legacy)
	}
}

// The allowlist decode must drop prompt-bearing fields at the JSON layer.
func TestSpendLogsAllowlistDecode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"request_id": "r1", "spend": 0.1, "messages": [{"content": "SECRET"}], "response": {"content": "SECRET"}}]`)
	}))
	defer server.Close()

	rows, err := NewClient(server.URL, "test-key").SpendLogs(context.Background(), "2026-08-27", "2026-08-28")
	if err != nil {
		t.Fatal(err)
	}

	if len(rows) != 1 || rows[0].RequestID != "r1" {
		t.Fatalf("unexpected rows: %+v", rows)
	}

	serialized, _ := json.Marshal(rows)
	if strings.Contains(string(serialized), "SECRET") {
		t.Fatalf("prompt content survived the allowlist decode: %s", serialized)
	}
}
