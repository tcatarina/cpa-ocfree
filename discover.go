package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// discoverTimeout bounds the catalog read. It runs on plugin registration and on
// every reconfigure, and the host waits on both, so a hung upstream must not
// hold the config load open.
const discoverTimeout = 5 * time.Second

// discoverFreeModels reads the upstream catalog and returns the models the free
// tier serves.
//
// The catalog carries no free marker: every entry is the same four fields with
// the id the only difference, so the id is the discriminator. The `-free`
// suffix is how the tier separates a free id from its paid sibling
// (`muse-spark-1.3-contributor-free` against `muse-spark-1.3`), and it is what
// a new free model arrives with. A free model that does not carry the suffix —
// `big-pickle` — is kept by the caller's merge rather than guessed at here.
//
// An error means "keep what you have". A failed read must not empty the target
// set: every request for a known-good model would then pass through
// uncontracted and be refused by the tier.
func discoverFreeModels(cfg contractConfig) ([]string, error) {
	base := strings.TrimRight(cfg.upstream(), "/")
	if base == "" {
		return nil, fmt.Errorf("no upstream configured")
	}
	req, err := http.NewRequest(http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	// The identity this plugin already applies upstream. The catalog answers
	// without it today; sending it keeps the read from being turned away by the
	// same edge rule that gates completions should that rule widen.
	req.Header.Set("User-Agent", cfg.userAgent())
	req.Header.Set("x-opencode-client", cfg.Client)
	req.Header.Set("x-opencode-project", cfg.Project)
	req.Header.Set("x-opencode-session", canonicalID("ses_", cfg.SessionSalt))
	req.Header.Set("x-opencode-request", canonicalID("msg_", cfg.SessionSalt))

	resp, err := (&http.Client{Timeout: discoverTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog answered %s", resp.Status)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range payload.Data {
		if id := strings.TrimSpace(m.ID); isFreeModelID(id) {
			out = append(out, id)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("catalog listed no free model")
	}
	return out, nil
}

// suffixlessFreeModels are free-tier models whose id carries no `-free` suffix,
// so the catalog's naming convention cannot pick them out. `big-pickle` is the
// one measured: the free tier serves it and it answers 403 without the contract.
//
// The catalog carries no free marker of any kind — every entry is the same four
// fields — so this is a shipped list rather than something inferred. Same call
// OmniRoute makes with its FREE_MODEL_BUDGETS, for the same reason.
var suffixlessFreeModels = map[string]bool{"big-pickle": true}

// isFreeModelID reports whether the catalog id names a free-tier model.
func isFreeModelID(id string) bool {
	return strings.HasSuffix(id, "-free") || suffixlessFreeModels[id]
}

// mergeDiscovered folds the catalog's free models into the configured ones,
// configured first so the order an operator chose survives.
//
// The union rather than a replacement is deliberate. The catalog cannot say
// which suffix-less ids are free, so replacing would take a working configured
// model — `big-pickle` — with it; and an entry the operator added by hand is
// theirs to keep. What this buys is the direction that actually rotted: a free
// model OpenCode adds tomorrow is in scope without an edit.
func mergeDiscovered(cfg contractConfig) contractConfig {
	found, err := discoverFreeModels(cfg)
	if err != nil {
		logWarn("catalog read failed, keeping configured models: " + err.Error())
		return cfg
	}
	seen := make(map[string]bool, len(cfg.Models)+len(found))
	merged := make([]string, 0, len(cfg.Models)+len(found))
	for _, id := range append(append([]string(nil), cfg.Models...), found...) {
		if id = strings.TrimSpace(id); id == "" || seen[id] {
			continue
		}
		seen[id] = true
		merged = append(merged, id)
	}
	logInfo("catalog merged, models=" + itoa(len(merged)))
	cfg.Models = merged
	return cfg
}
