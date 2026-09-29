package main

import (
	"encoding/json"
	"testing"
)

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
