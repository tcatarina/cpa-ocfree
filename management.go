package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Menu        string `json:"menu,omitempty"`
	Description string `json:"description,omitempty"`
}

type managementRouteSet struct {
	Routes []managementRoute `json:"routes"`
}

func managementRouteSetResponse() managementRouteSet {
	return managementRouteSet{Routes: []managementRoute{
		{Method: http.MethodGet, Path: resourcePath, Menu: "OpenCode Free", Description: "Free-tier request contract"},
		{Method: http.MethodGet, Path: resourcePath + "/api", Description: "Read the contract config"},
		{Method: http.MethodPut, Path: resourcePath + "/api", Description: "Write the contract config"},
	}}
}

func handleManagement(raw []byte) ([]byte, error) {
	var req rpcManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	path := normalizePath(req.Path)
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	if method == "" {
		method = http.MethodGet
	}
	api := resourcePath + "/api"

	switch {
	case path == resourcePath && method == http.MethodGet:
		switch req.Query.Get("asset") {
		case "css":
			return okEnvelope(assetResponse("text/css; charset=utf-8", pageCSS))
		case "js":
			js := strings.ReplaceAll(pageJS, "__API__", "/v0/management"+api)
			return okEnvelope(assetResponse("text/javascript; charset=utf-8", js))
		case "config":
			out, err := yaml.Marshal(state.get())
			if err != nil {
				return okEnvelope(assetResponse("text/yaml; charset=utf-8", "# "+err.Error()))
			}
			return okEnvelope(assetResponse("text/yaml; charset=utf-8", string(out)))
		case "provider":
			return okEnvelope(assetResponse("text/yaml; charset=utf-8", providerBlock(state.get())))
		}
		return okEnvelope(assetResponse("text/html; charset=utf-8", pageHTML))
	case path == api && method == http.MethodGet:
		return okEnvelope(jsonResponse(200, state.get()))
	case path == api && method == http.MethodPut:
		if len(req.Body) == 0 {
			return okEnvelope(jsonResponse(400, map[string]any{"error": "body is required"}))
		}
		var cfg contractConfig
		if err := yaml.Unmarshal(req.Body, &cfg); err != nil {
			return okEnvelope(jsonResponse(400, map[string]any{"error": err.Error()}))
		}
		state.set(cfg)
		return okEnvelope(jsonResponse(200, state.get()))
	default:
		return okEnvelope(jsonResponse(404, map[string]any{"error": "not found"}))
	}
}

// normalizePath strips the mount prefix the host dispatched on so the result
// can be compared against the declared route paths.
//
// The two prefixes need different treatment. A management path keeps its
// plugin-id segment: /v0/management/ocfree/api becomes /ocfree/api. A resource
// path repeats the id: /v0/resource/plugins/ocfree/ocfree/api, so the first
// segment is dropped and /ocfree/api is kept.
func normalizePath(p string) string {
	t := strings.TrimSpace(p)
	if t == "" {
		return resourcePath
	}
	switch {
	case strings.HasPrefix(t, "/v0/resource/plugins/"):
		rest := t[len("/v0/resource/plugins/"):]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			t = rest[slash:]
		} else {
			t = "/"
		}
	case strings.HasPrefix(t, "/v0/management/"):
		t = t[len("/v0/management/"):]
	}
	if !strings.HasPrefix(t, "/") {
		t = "/" + t
	}
	if len(t) > 1 {
		t = strings.TrimRight(t, "/")
	}
	return t
}

func assetResponse(contentType, body string) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: 200,
		Headers: http.Header{
			"Content-Type":           []string{contentType},
			"Cache-Control":          []string{"no-store"},
			"X-Content-Type-Options": []string{"nosniff"},
		},
		Body: []byte(body),
	}
}

func jsonResponse(status int, payload any) pluginapi.ManagementResponse {
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = []byte(`{"error":"marshal failed"}`)
		status = 500
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       raw,
	}
}

// providerBlock renders the openai-compatibility entry that points CPA at the
// free tier.
//
// Two details are load-bearing and were both measured rather than guessed:
//
//   - the api keys must be EMPTY. A non-empty key is sent as `Authorization:
//     Bearer <key>` and the upstream answers 401 Invalid API key. Empty keys
//     also still yield one auth record per entry, because the stable id
//     generator suffixes duplicates, which is what gives rotation its members.
//   - the identity headers have to be declared here, not injected by this
//     plugin. CPA's compat executor overwrites User-Agent and does not forward
//     interceptor headers, so the only route upstream is the provider's own
//     headers map.
func providerBlock(cfg contractConfig) string {
	var b strings.Builder
	b.WriteString("openai-compatibility:\n")
	b.WriteString("  - name: opencode-free\n")
	b.WriteString("    base-url: " + cfg.Upstream + "\n")
	b.WriteString("    # No api-key on purpose: an empty one sends no Authorization\n")
	b.WriteString("    # header, and a populated one is refused upstream with 401\n")
	b.WriteString("    # Invalid API key. Each entry is still its own auth record,\n")
	b.WriteString("    # so several of them give rotation several members.\n")
	b.WriteString("    api-key-entries:\n")
	for i := 1; i <= 3; i++ {
		b.WriteString("      - {}\n")
	}
	b.WriteString("    headers:\n")
	b.WriteString("      User-Agent: \"" + cfg.userAgent() + "\"\n")
	b.WriteString("      x-opencode-session: \"" + canonicalID("ses_", cfg.SessionSalt) + "\"\n")
	b.WriteString("      x-opencode-request: \"" + canonicalID("msg_", cfg.SessionSalt) + "\"\n")
	b.WriteString("      x-opencode-project: \"" + cfg.Project + "\"\n")
	b.WriteString("      x-opencode-client: \"" + cfg.Client + "\"\n")
	b.WriteString("    models:\n")
	for _, m := range cfg.Models {
		b.WriteString("      - name: " + m + "\n")
	}
	return b.String()
}
