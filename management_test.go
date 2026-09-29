package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// callResource drives the handler the way the host does for a resource route
// and returns the response body.
func callResource(t *testing.T, rawQuery string) string {
	t.Helper()
	parsed, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatal(err)
	}
	req := rpcManagementRequest{
		ManagementRequest: pluginapi.ManagementRequest{
			Method: http.MethodGet,
			Path:   "/v0/resource/plugins/ocfree/ocfree",
			Query:  parsed,
		},
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := handleManagement(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			StatusCode int `json:"StatusCode"`
			Body       []byte
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("unreadable envelope %s: %v", out, err)
	}
	if !env.OK {
		t.Fatalf("handler reported failure: %s", out)
	}
	if env.Result.StatusCode != http.StatusOK {
		t.Fatalf("asset %s answered %d", rawQuery, env.Result.StatusCode)
	}
	return string(env.Result.Body)
}

// The page reads this payload as JSON with lowercase keys. Without the json
// tags Go emits capitalised field names and the panel renders an empty config.
func TestConfigJSONUsesLowercaseKeys(t *testing.T) {
	raw, err := json.Marshal(defaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"enabled", "models", "tool_names", "tools", "user_agent", "session_salt"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("missing json key %q in %s", key, raw)
		}
	}
	for _, key := range []string{"Enabled", "Models", "ToolNames", "UserAgent"} {
		if _, ok := doc[key]; ok {
			t.Errorf("capitalised key %q leaked; the page reads lowercase", key)
		}
	}
	if models, _ := doc["models"].([]any); len(models) == 0 {
		t.Error("models did not survive the round trip")
	}
}

// The host dispatches management and resource requests with different mount
// shapes. Getting these wrong made every management call fall through to the
// plugin's own 404, which looks exactly like a missing route.
// The page used to fetch the config without checking the status, so a 401
// body was rendered as if it were the config: zero models, a fallback user
// agent, and the error text in the editor.
func TestPageChecksResponseStatus(t *testing.T) {
	if !strings.Contains(pageJS, "if (!r.ok)") {
		t.Error("the page never checks r.ok, so an error body is rendered as config")
	}
	if strings.Contains(pageJS, "return r.json(); }).then(render)") {
		t.Error("the page still pipes a response straight into render")
	}
}

// The page reads from its own resource route, so it needs no management key
// and there is nothing for it to fail to authenticate against.
func TestPageReadsStateWithoutAManagementKey(t *testing.T) {
	if !strings.Contains(pageJS, `get("?asset=state")`) {
		t.Error("the page does not read its state from the resource route")
	}
	if strings.Contains(pageJS, "Authorization") {
		t.Error("the page should not be sending a management key")
	}
	if strings.Contains(pageJS, "v0/management") {
		t.Error("the page still depends on the management API")
	}
}

// The state asset must be JSON the page can parse, with the keys it reads.
func TestStateAssetIsValidJSON(t *testing.T) {
	body := callResource(t, "asset=state")
	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("state asset is not valid JSON: %v (%s)", err, body)
	}
	if models, _ := doc["models"].([]any); len(models) == 0 {
		t.Errorf("state asset reported no models: %s", body)
	}
	if _, ok := doc["enabled"]; !ok {
		t.Errorf("state asset has no enabled flag: %s", body)
	}
}

// Every asset the page fetches must exist, or it silently renders nothing.
func TestPageAssetsAllResolve(t *testing.T) {
	for _, asset := range []string{"css", "js", "config", "provider", "state"} {
		if body := callResource(t, "asset="+asset); body == "" {
			t.Errorf("asset %s returned an empty body", asset)
		}
	}
}

func TestNormalizePath(t *testing.T) {
	cases := []struct {
		in   string
		want string
		why  string
	}{
		{"/v0/management/ocfree/api", "/ocfree/api", "management keeps the plugin id"},
		{"/v0/management/ocfree/api/", "/ocfree/api", "trailing slash trimmed"},
		{"/v0/management/ocfree", "/ocfree", "bare management path"},
		{"/v0/resource/plugins/ocfree/ocfree", "/ocfree", "resource path drops the first id"},
		{"/v0/resource/plugins/ocfree/ocfree/api", "/ocfree/api", "resource api path"},
		{"/v0/resource/plugins/ocfree/ocfree/api/config", "/ocfree/api/config", "nested resource api"},
		{"/ocfree/api", "/ocfree/api", "already normalised"},
		{"", "/ocfree", "empty falls back to the resource root"},
	}
	for _, tc := range cases {
		if got := normalizePath(tc.in); got != tc.want {
			t.Errorf("normalizePath(%q) = %q, want %q (%s)", tc.in, got, tc.want, tc.why)
		}
	}
}

// Every path the switch actually compares against must be reachable, or the
// handler silently answers with its own 404.
func TestEveryDeclaredRouteIsReachableThroughTheSwitch(t *testing.T) {
	routes := managementRouteSetResponse().Routes
	for _, route := range routes {
		if route.Menu != "" {
			continue
		}
		in := "/v0/management" + route.Path
		if got := normalizePath(in); got != route.Path {
			t.Errorf("route %s %s is unreachable: %s normalises to %q",
				route.Method, route.Path, in, got)
		}
		in = "/v0/resource/plugins/ocfree/ocfree"
		if route.Path != resourcePath {
			in += route.Path[len(resourcePath):]
		}
		if got := normalizePath(in); got != route.Path {
			t.Errorf("resource route %s %s is unreachable: %s normalises to %q",
				route.Method, route.Path, in, got)
		}
	}
}
