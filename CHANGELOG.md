# Changelog

## v0.2.1

**Upgrade recommended for every deployment.**

### Fixed

- **Each event was stored once per poll cycle.** Every cycle re-read the
  trailing `LOOKBACK_HOURS` window and pushed every event in it. Readers
  dedupe DataHub events by id, so reported spend was correct, but the
  events table appends a row per push. With the defaults (`48h` window,
  `5m` interval) an event was stored up to ~576 times, or ~864 once the
  window is widened to whole days. Raw-table sums were inflated by the same
  factor, and so were storage and scan costs.

  The exporter still re-reads the same window, so late or corrected spend
  rows are still picked up. It now pushes only events that are new or whose
  content changed since the last successful cycle. Fingerprints of the last
  pushed window are kept in `sent.bin`, next to `STATE_FILE`.

  - The first cycle after upgrading re-sends the window once (there is no
    `sent.bin` yet). Losing the state directory later does the same: one
    full re-send, never a gap.
  - A failed push leaves `sent.bin` untouched, so the next cycle retries
    everything it could not confirm.
  - `daily` mode re-sends a day's aggregate only when it changes, normally
    just the current day.

### Added

- `/metrics`: `litellm_exporter_events_read_total` and
  `litellm_exporter_events_skipped_unchanged_total`.
  `litellm_exporter_events_pushed_total` now counts only events actually
  sent.

### Note

Rows already stored by earlier versions are not removed by upgrading.
Reports are unaffected (they dedupe at read time). Compacting the stored
rows is a DataHub-side operation.
