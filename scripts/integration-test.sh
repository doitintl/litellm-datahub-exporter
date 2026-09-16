#!/usr/bin/env bash
# Integration test: real LiteLLM (docker) -> exporter -> local DataHub stub.
# Usage: scripts/integration-test.sh [litellm image tag, default main-stable]
set -euo pipefail

TAG="${1:-main-stable}"
DIR="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
ITEST_EMAIL="itest@example.com"
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

# The fourth request carries an EMAIL-SHAPED end_user. A proxy that
# authenticates people puts the person's address there, and that is the only
# per-request identity a shared virtual key carries -- see
# GENAI_USER_EMAIL_SOURCE in the README.
for i in 1 2 3; do
  curl -sf -o /dev/null -X POST http://localhost:4010/v1/chat/completions \
    -H "Authorization: Bearer sk-integration-master" -H 'Content-Type: application/json' \
    -d "{\"model\":\"test-model\",\"user\":\"itest-user\",\"messages\":[{\"role\":\"user\",\"content\":\"req $i\"}]}"
done
curl -sf -o /dev/null -X POST http://localhost:4010/v1/chat/completions \
  -H "Authorization: Bearer sk-integration-master" -H 'Content-Type: application/json' \
  -d "{\"model\":\"test-model\",\"user\":\"$ITEST_EMAIL\",\"messages\":[{\"role\":\"user\",\"content\":\"req email\"}]}"
sleep 12

# $2 sets GENAI_USER_EMAIL_SOURCE. With no $2 the variable is LEFT UNSET
# rather than set to "none" -- the guarantee under test is what an existing
# deployment does, and an existing deployment does not set it at all. Passing
# "none" explicitly would mask a change to the DEFAULT in config.FromEnv,
# which is exactly the regression these runs exist to catch.
run_once() {
  src=()
  [ -n "${2:-}" ] && src=(GENAI_USER_EMAIL_SOURCE="$2")

  env LITELLM_BASE_URL=http://localhost:4010 LITELLM_API_KEY=sk-integration-master \
    DOIT_API_URL=http://localhost:8181 DOIT_API_KEY=stub DATASET=LiteLLM \
    STATE_FILE="$WORK/state.json" MODE="${1:-per_call}" \
    ${src[@]+"${src[@]}"} go run "$DIR/cmd/exporter" --once
}

# Distinct values the stub saw for one dimension, as a bare sorted list.
seen_dimension() {
  curl -sf "http://localhost:8181/dimension?key=$1" | tr -d '[]"' 
}

if run_once per_call; then
  run_once per_call  # idempotency: re-run must not error and must not add unique events

  RECEIVED=$(curl -sf http://localhost:8181/received | sed 's/[^0-9]//g')
  if [ "$RECEIVED" -lt 3 ]; then
    echo "FAIL: expected >=3 unique per-call events at the stub, got $RECEIVED" >&2
    exit 1
  fi

  # THE DEFAULT MUST EMIT NOTHING. Both runs above left
  # GENAI_USER_EMAIL_SOURCE unset, and one of the rows carries an
  # email-shaped end_user -- so an exporter that promoted it without being
  # told would show up here. This is the backward-compatibility guarantee
  # for every deployment that does not set the variable.
  if [ -n "$(seen_dimension system_label/genai/user_email)" ]; then
    echo "FAIL: genai/user_email emitted with GENAI_USER_EMAIL_SOURCE unset: $(seen_dimension system_label/genai/user_email)" >&2
    exit 1
  fi

  # DECLARED: the address must now reach DataHub. Proves the whole chain
  # against a REAL proxy -- that LiteLLM puts the value in end_user, that the
  # allowlist decode keeps it, and that the dimension survives to the event.
  run_once per_call end_user
  if [ "$(seen_dimension system_label/genai/user_email)" != "$ITEST_EMAIL" ]; then
    echo "FAIL: GENAI_USER_EMAIL_SOURCE=end_user did not export $ITEST_EMAIL, saw: $(seen_dimension system_label/genai/user_email)" >&2
    exit 1
  fi

  # The opaque ids in the other three rows must NOT have been promoted.
  if [ "$(seen_dimension system_label/genai/user_id | tr ',' '\n' | grep -c .)" -lt 2 ]; then
    echo "FAIL: expected several distinct genai/user_id values" >&2
    exit 1
  fi
  echo "per-call: genai/user_email absent by default, $ITEST_EMAIL when declared"
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
