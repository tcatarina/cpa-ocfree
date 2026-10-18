package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func catalogServer(t *testing.T, ids []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if ua := r.Header.Get("User-Agent"); ua == "" {
			t.Error("catalog read sent no User-Agent")
		}
		data := make([]map[string]any, 0, len(ids))
		for _, id := range ids {
			data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "opencode"})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	}))
}

func TestDiscoverKeepsOnlyFreeIDs(t *testing.T) {
	srv := catalogServer(t, []string{
		"space-bunny-free", "muse-spark-1.3-contributor-free", "big-pickle",
		"muse-spark-1.3", "gpt-6.1-sol", "step-5-preview-free",
	})
	defer srv.Close()

	got, err := discoverFreeModels(contractConfig{Upstream: srv.URL, Client: "desktop", Project: "global"})
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	// big-pickle is free and gated but carries no suffix, so it comes from the
	// shipped table rather than the naming convention.
	want := []string{"space-bunny-free", "muse-spark-1.3-contributor-free", "big-pickle", "step-5-preview-free"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("free ids = %v, want %v", got, want)
	}
}

// A free model with no suffix cannot be told apart from a paid one by the
// catalog, so the merge is what has to keep it.
func TestMergeKeepsConfiguredSuffixlessModel(t *testing.T) {
	srv := catalogServer(t, []string{"big-pickle", "step-5-preview-free", "gpt-6.1-sol"})
	defer srv.Close()

	cfg := contractConfig{
		Upstream: srv.URL, Client: "desktop", Project: "global",
		Models: []string{"big-pickle"},
	}
	got := mergeDiscovered(cfg).Models
	want := []string{"big-pickle", "step-5-preview-free"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merged = %v, want %v", got, want)
	}
}

// The failure that matters: an unreadable catalog must not empty the target set,
// or every known-good model starts passing through uncontracted.
func TestMergeFallsBackWhenCatalogUnreadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	cfg := contractConfig{
		Upstream: srv.URL, Client: "desktop", Project: "global",
		Models: []string{"space-bunny-free", "step-5-preview-free"},
	}
	if got := mergeDiscovered(cfg).Models; !reflect.DeepEqual(got, cfg.Models) {
		t.Errorf("models = %v, want the configured %v", got, cfg.Models)
	}
}

// The config a plugin actually receives carries no upstream key, so discovery
// has to resolve the default the same way state.set does — otherwise every
// real deployment reads as "no upstream configured" and silently keeps its old
// list.
func TestDiscoverUsesTheDefaultUpstreamWhenUnset(t *testing.T) {
	if got := (contractConfig{}).upstream(); got != defaultConfig().Upstream {
		t.Errorf("upstream = %q, want the default %q", got, defaultConfig().Upstream)
	}
	if got := (contractConfig{Upstream: "https://example.test/v1"}).upstream(); got != "https://example.test/v1" {
		t.Errorf("upstream = %q, want the configured one", got)
	}
}

// The refresh asset merges the catalogue into the live set and reports the
// delta, so the page can show what changed without another round trip.
func TestRefreshMergesAndReportsAdded(t *testing.T) {
	srv := catalogServer(t, []string{"space-bunny-free", "step-5-preview-free", "gpt-6.1-sol"})
	defer srv.Close()

	prev := state.get()
	defer state.set(prev)
	state.set(contractConfig{
		Enabled: true, Client: "desktop", Project: "global",
		Upstream: srv.URL, Catalog: srv.URL, Models: []string{"space-bunny-free", "big-pickle"},
	})

	resp := refreshResponse()
	if resp.StatusCode != 200 {
		t.Fatalf("refresh answered %d", resp.StatusCode)
	}
	var got struct {
		Models []string `json:"models"`
		Added  []string `json:"added"`
		Total  int      `json:"total"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("unreadable refresh body: %v", err)
	}
	if !reflect.DeepEqual(got.Added, []string{"step-5-preview-free"}) {
		t.Errorf("added = %v, want [step-5-preview-free]", got.Added)
	}
	if got.Total != 3 || !reflect.DeepEqual(got.Models, []string{"space-bunny-free", "big-pickle", "step-5-preview-free"}) {
		t.Errorf("models = %v total %d, want the merged three", got.Models, got.Total)
	}
	if live := state.get().Models; !reflect.DeepEqual(live, got.Models) {
		t.Errorf("live set = %v, want the merged list", live)
	}
}

// A failed catalogue read keeps the set and reports nothing added, never an
// error: the button must degrade to "nothing new", not break the page.
func TestRefreshKeepsSetWhenCatalogueUnreadable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	prev := state.get()
	defer state.set(prev)
	state.set(contractConfig{
		Enabled: true, Client: "desktop", Project: "global",
		Upstream: srv.URL, Catalog: srv.URL, Models: []string{"space-bunny-free"},
	})

	resp := refreshResponse()
	var got struct {
		Models []string `json:"models"`
		Added  []string `json:"added"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("unreadable refresh body: %v", err)
	}
	if len(got.Added) != 0 || !reflect.DeepEqual(got.Models, []string{"space-bunny-free"}) {
		t.Errorf("models = %v added = %v, want the set kept", got.Models, got.Added)
	}
}
func TestDiscoverRejectsCatalogWithNoFreeModel(t *testing.T) {
	srv := catalogServer(t, []string{"gpt-6.1-sol", "claude-opus-5-5"})
	defer srv.Close()

	if _, err := discoverFreeModels(contractConfig{Upstream: srv.URL}); err == nil {
		t.Error("a paid-only catalog was accepted as a target set")
	}
}
