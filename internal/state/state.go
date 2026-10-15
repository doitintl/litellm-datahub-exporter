// Package state persists the exporter's checkpoint and the fingerprints of
// what it last pushed. Losing either file is safe: a lost checkpoint only
// widens the next poll window, and a lost fingerprint set re-sends that
// window once. Re-sent events add rows to DataHub, which readers dedupe by
// event id, so the cost of loss is storage, never missing data.
package state

import (
	"encoding/json"
	"os"
	"time"
)

type State struct {
	LastSuccess time.Time `json:"last_success"`
}

func Load(path string) State {
	var s State

	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}

	_ = json.Unmarshal(data, &s)

	return s
}

func Save(path string, s State) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, path)
}
