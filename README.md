# litellm-datahub-exporter

Export spend from your self-hosted [LiteLLM](https://docs.litellm.ai/) proxy into [DoiT Cloud Intelligence](https://www.doit.com/) — one DataHub event per model call, labeled by provider, model, virtual key, team, feature, and end customer, so your LLM spend lands in Cloud Analytics next to your cloud bill.

```
LiteLLM proxy + Postgres ──(polls /spend/logs)──▶ exporter ──HTTPS──▶ api.doit.com/datahub/v1/events ──▶ Cloud Analytics (~15 min)
        your network ────────────────────────────────────┘
```

- **Runs entirely in your infrastructure.** The exporter polls your proxy's spend APIs and makes outbound HTTPS calls to `api.doit.com` — nothing else. Your LiteLLM admin key never leaves your network; DoiT needs no network path in.
- **Never ships prompt content.** Spend rows are decoded through a strict field allowlist; `messages`/`response` fields are dropped at decode time and cannot be serialized onward (covered by a test).
- **Crash-safe and idempotent.** Every event has a deterministic id and DataHub dedups by id (last write wins), so restarts, re-polls, and backfills overwrite instead of double-counting. Losing the checkpoint file is harmless.
- **Estimated spend.** LiteLLM computes cost from its price map × tokens (including any custom pricing you configured) — treat this as showback, not billable actuals. Every event carries a `cost_basis: estimated` system label.

## Quick start

Requirements: LiteLLM ≥ v1.65 with the database (spend logging) enabled; a DoiT API token with the `DataHubAdmin` scope (Console → User view → API); a DataHub subscription on your DoiT tier.

```sh
docker run -d --name litellm-datahub-exporter \
  -e LITELLM_BASE_URL=http://litellm:4000 \
  -e LITELLM_API_KEY=sk-... \
  -e DOIT_API_KEY=... \
  -v litellm-exporter-state:/state -e STATE_FILE=/state/state.json \
  ghcr.io/doitintl/litellm-datahub-exporter:latest
```

Or grab a static binary from Releases and run it under systemd. On Kubernetes, run **one replica per LiteLLM deployment** (replicated proxies share one Postgres — do not run the exporter as a sidecar), pointed at the proxy's Service.

Verify a single cycle without starting the loop:

```sh
litellm-datahub-exporter --once
```

Within ~15 minutes your spend appears in the DoiT console under **DataHub → Datasets → LiteLLM**, and in Cloud Analytics reports as *Cloud provider = LiteLLM*.

## Configuration

| Env var | Default | Meaning |
|---|---|---|
| `LITELLM_BASE_URL` | `http://localhost:4000` | Your proxy URL |
| `LITELLM_API_KEY` | — (required) | Admin/master virtual key, used only against your proxy |
| `DOIT_API_KEY` | — (required) | DoiT API token with `DataHubAdmin` scope |
| `DOIT_API_URL` | `https://api.doit.com` | DoiT API host |
| `DATASET` | `LiteLLM` | DataHub dataset name (letters, digits, `_`, `-`, single spaces) |
| `MODE` | `per_call` | `per_call` (one event per request) or `daily` (one event per day × model × key; for very high volume, roughly >1M calls/day) |
| `POLL_INTERVAL` | `5m` | Cycle interval |
| `LOOKBACK_HOURS` | `48` | Trailing window re-read each cycle (absorbs LiteLLM's async spend writes; overlap is idempotent) |
| `BACKFILL_DAYS` | `0` | On first run, export this much history (bounded by your `maximum_spend_logs_retention_period` and DataHub's ±2-year window) |
| `FEATURE_METADATA_KEY` | `feature` | Key read from `spend_logs_metadata` into the `feature` label |
| `EMIT_TRACE_LABELS` | `false` | Also emit `request_id`/`parent_trace_id` labels (high cardinality — leave off unless you need them) |
| `TAG_DENY_PREFIXES` | `User-Agent` | Comma-separated tag prefixes to drop (LiteLLM auto-injects `User-Agent:` tags) |
| `STATE_FILE` | `state.json` | Checkpoint path |
| `METRICS_ADDR` | `:9464` | Prometheus `/metrics` + `/healthz` listener (`""` disables) |
| `MAX_BATCH` | `5000` | Events per DataHub request (hard API cap: 50,000) |

## Labeling your traffic

The labels come from how you instrument calls through LiteLLM:

- **customer** ← the OpenAI `user` request parameter (LiteLLM's `end_user`)
- **feature** ← `metadata: {spend_logs_metadata: {feature: "advice-chat"}}` on the request
- **team / virtual_key** ← the LiteLLM key and team the call was made with (key alias used when set)
- **tag** ← LiteLLM request tags (body `metadata.tags` or the `x-litellm-tags` header)

## Building from source

```sh
make build   # CGO_ENABLED=0, -trimpath → reproducible static binary
make test
```

No dependencies outside the Go standard library.

## License

Apache-2.0.
