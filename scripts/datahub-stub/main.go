// datahub-stub is a minimal stand-in for the DoiT DataHub API used by the
// integration test: it accepts event batches, enforces the contract limits
// the real API enforces (batch size, provider pattern, within-batch id
// uniqueness), and reports what it received on GET /received.
//
// It also records the DIMENSIONS of every event, so a test can assert what a
// label CONTAINS and not merely that a well-formed event arrived. GET
// /dimension?key=<key> answers with the distinct values seen for that
// dimension, which is how the GENAI_USER_EMAIL_SOURCE cases check that the
// address reached DataHub -- and that it did not when the source is unset.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"sync"
)

var providerPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+( [a-zA-Z0-9_-]+)*$`)

func main() {
	addr := ":8181"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	}

	var (
		mu       sync.Mutex
		eventIDs = map[string]int{}
		datasets = map[string]bool{}
		// "<type>/<key>" -> the distinct values seen, e.g.
		// "system_label/genai/user_email" -> {"itest@example.com"}.
		dimensions = map[string]map[string]bool{}
	)

	mux := http.NewServeMux()

	mux.HandleFunc("GET /datahub/v1/datasets/{name}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if !datasets[r.PathValue("name")] {
			w.WriteHeader(http.StatusNotFound)
			return
		}

		_ = json.NewEncoder(w).Encode(map[string]any{"name": r.PathValue("name")})
	})

	mux.HandleFunc("POST /datahub/v1/datasets", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&body)

		mu.Lock()
		defer mu.Unlock()

		if datasets[body.Name] {
			http.Error(w, `{"error":"Failed to create dataset"}`, http.StatusInternalServerError)
			return
		}

		datasets[body.Name] = true
		w.WriteHeader(http.StatusCreated)
	})

	mux.HandleFunc("POST /datahub/v1/events", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []struct {
				Provider   string `json:"provider"`
				ID         string `json:"id"`
				Metrics    []any  `json:"metrics"`
				Dimensions []struct {
					Key   string `json:"key"`
					Type  string `json:"type"`
					Value string `json:"value"`
				} `json:"dimensions"`
			} `json:"events"`
		}

		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Events) == 0 || len(body.Events) > 50000 {
			http.Error(w, `{"error":"invalid batch"}`, http.StatusBadRequest)
			return
		}

		seen := map[string]bool{}

		for _, e := range body.Events {
			if !providerPattern.MatchString(e.Provider) || seen[e.ID] || len(e.Metrics) > 255 {
				http.Error(w, `{"error":"validation"}`, http.StatusBadRequest)
				return
			}

			seen[e.ID] = true
		}

		mu.Lock()
		for _, e := range body.Events {
			eventIDs[e.ID]++

			for _, d := range e.Dimensions {
				k := d.Type + "/" + d.Key
				if dimensions[k] == nil {
					dimensions[k] = map[string]bool{}
				}

				dimensions[k][d.Value] = true
			}
		}
		mu.Unlock()

		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"message":"Ingestion success"}`))
	})

	mux.HandleFunc("GET /received", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]any{"unique_events": len(eventIDs)})
	})

	// Distinct values seen for one dimension, as a JSON array. An absent
	// dimension answers [] rather than 404, so a test can assert absence the
	// same way it asserts a value.
	mux.HandleFunc("GET /dimension", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		values := []string{}
		for v := range dimensions[r.URL.Query().Get("key")] {
			values = append(values, v)
		}

		sort.Strings(values)
		_ = json.NewEncoder(w).Encode(values)
	})

	log.Fatal(http.ListenAndServe(addr, mux))
}
