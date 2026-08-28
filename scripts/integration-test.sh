#!/usr/bin/env bash
# Integration test: real LiteLLM (docker) -> exporter -> local DataHub stub.
# Usage: scripts/integration-test.sh [litellm image tag, default main-stable]
set -euo pipefail

TAG="${1:-main-stable}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'docker compose -f "$WORK/compose.yaml" down -v >/dev/null 2>&1 || true; [ -n "${STUB_PID:-}" ] && kill "$STUB_PID" 2>/dev/null || true; rm -rf "$WORK"' EXIT

cat > "$WORK/litellm-config.yaml" <<'EOF'
model_list:
  - model_name: test-model
    litellm_params:
      model: anthropic/claude-3-5-sonnet-20241022
      api_key: dummy
      mock_response: "integration test response"
      input_cost_per_token: 0.000003
      output_cost_per_token: 0.000015
general_settings:
  master_key: os.environ/LITELLM_MASTER_KEY
  database_url: os.environ/DATABASE_URL
EOF

cat > "$WORK/compose.yaml" <<EOF
services:
  db:
    image: postgres:16-alpine
    environment: {POSTGRES_USER: llm, POSTGRES_PASSWORD: llm, POSTGRES_DB: litellm}
    healthcheck: {test: ["CMD-SHELL", "pg_isready -U llm -d litellm"], interval: 2s, retries: 30}
  litellm:
    image: ghcr.io/berriai/litellm:$TAG
    depends_on: {db: {condition: service_healthy}}
    ports: ["4010:4000"]
    volumes: ["$WORK/litellm-config.yaml:/app/config.yaml"]
    command: ["--config", "/app/config.yaml", "--port", "4000"]
    environment:
      DATABASE_URL: postgresql://llm:llm@db:5432/litellm
      LITELLM_MASTER_KEY: sk-integration-master
EOF

docker compose -f "$WORK/compose.yaml" up -d
for _ in $(seq 1 60); do curl -sf -o /dev/null http://localhost:4010/health/liveliness && break; sleep 3; done

go build -o "$WORK/datahub-stub" "$DIR/scripts/datahub-stub"
"$WORK/datahub-stub" :8181 &
STUB_PID=$!
sleep 1

for i in 1 2 3; do
  curl -sf -o /dev/null -X POST http://localhost:4010/v1/chat/completions \
    -H "Authorization: Bearer sk-integration-master" -H 'Content-Type: application/json' \
    -d "{\"model\":\"test-model\",\"user\":\"itest-user\",\"messages\":[{\"role\":\"user\",\"content\":\"req $i\"}]}"
done
sleep 12

run_once() {
  LITELLM_BASE_URL=http://localhost:4010 LITELLM_API_KEY=sk-integration-master \
  DOIT_API_URL=http://localhost:8181 DOIT_API_KEY=stub DATASET=LiteLLM \
  STATE_FILE="$WORK/state.json" MODE="${1:-per_call}" go run "$DIR/cmd/exporter" --once
}

if run_once per_call; then
  run_once per_call  # idempotency: re-run must not error and must not add unique events

  RECEIVED=$(curl -sf http://localhost:8181/received | sed 's/[^0-9]//g')
  if [ "$RECEIVED" -lt 3 ]; then
    echo "FAIL: expected >=3 unique per-call events at the stub, got $RECEIVED" >&2
    exit 1
  fi
else
  # Old proxies (v1.65-era) have no per-request spend rows; the startup
  # probe must reject per_call mode rather than exporting garbage.
  echo "per_call rejected by capability probe on litellm:$TAG — daily mode is the supported path"
  RECEIVED=0
fi

# LiteLLM populates its daily aggregate tables asynchronously — wait for the
# aggregation to land before asserting on daily mode.
for _ in $(seq 1 24); do
  DAILY_READY=$(curl -sf "http://localhost:4010/user/daily/activity?start_date=$(date -u -d yesterday +%F 2>/dev/null || date -u -v-1d +%F)&end_date=$(date -u -d tomorrow +%F 2>/dev/null || date -u -v+1d +%F)&include_current_utc_day=true" \
    -H "Authorization: Bearer sk-integration-master" | grep -c '"date"' || true)
  [ "$DAILY_READY" -ge 1 ] && break
  sleep 5
done

run_once daily  # aggregate mode against the same proxy (daily endpoints, pagination)

RECEIVED_AFTER_DAILY=$(curl -sf http://localhost:8181/received | sed 's/[^0-9]//g')
if [ "$RECEIVED_AFTER_DAILY" -le "$RECEIVED" ]; then
  echo "FAIL: daily mode added no events ($RECEIVED -> $RECEIVED_AFTER_DAILY)" >&2
  exit 1
fi

echo "PASS: $RECEIVED per-call + $((RECEIVED_AFTER_DAILY - RECEIVED)) daily unique events exported against litellm:$TAG"
