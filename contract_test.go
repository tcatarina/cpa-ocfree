package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEmptyToolShapePerProtocol(t *testing.T) {
	cases := []struct {
		format string
		check  func(t *testing.T, raw json.RawMessage)
	}{
		{"openai", func(t *testing.T, raw json.RawMessage) {
			if !strings.Contains(string(raw), `"function"`) || !strings.Contains(string(raw), `"type":"function"`) {
				t.Errorf("openai shape wrong: %s", raw)
			}
		}},
		{"claude", func(t *testing.T, raw json.RawMessage) {
			if !strings.Contains(string(raw), `"input_schema"`) {
				t.Errorf("claude shape wrong: %s", raw)
			}
			if strings.Contains(string(raw), `"function"`) {
				t.Errorf("claude tool carries an openai wrapper: %s", raw)
			}
		}},
		{"gemini", func(t *testing.T, raw json.RawMessage) {
			if !strings.Contains(string(raw), `"functionDeclarations"`) {
				t.Errorf("gemini shape wrong: %s", raw)
			}
		}},
		{"openai-response", func(t *testing.T, raw json.RawMessage) {
			if strings.Contains(string(raw), `"function":{`) {
				t.Errorf("responses tool should keep name at the top level: %s", raw)
			}
			if !strings.Contains(string(raw), `"name":"bash"`) {
				t.Errorf("responses shape wrong: %s", raw)
			}
		}},
	}
	for _, tc := range cases {
		raw := emptyTool("bash", tc.format)
		tc.check(t, raw)
		if toolName(raw) != "bash" {
			t.Errorf("%s: tool name unreadable from %s", tc.format, raw)
		}
	}
}

func TestContractWritesToolsInCallerProtocol(t *testing.T) {
	// The body is still in the caller's protocol here; the host translates after.
	body := []byte(`{"model":"m","messages":[]}`)
	inj := applyContract(body, map[string]string{}, "m", "claude", defaultConfig(), "seed")
	if !contains(inj.Applied, "tools") {
		t.Fatal("no tools injected")
	}
	if !strings.Contains(string(mustField(t, inj.Body, "tools")), `"input_schema"`) {
		t.Fatalf("a claude caller received openai-shaped tools: %s", mustField(t, inj.Body, "tools"))
	}
}

func TestToolNameReadsEveryShape(t *testing.T) {
	cases := map[string]string{
		`{"type":"function","function":{"name":"a"}}`: "a",
		`{"type":"function","name":"b"}`:              "b",
		`{"name":"c","input_schema":{}}`:              "c",
		`{"functionDeclarations":[{"name":"d"}]}`:     "d",
		`{"nothing":1}`:                               "",
	}
	for raw, want := range cases {
		if got := toolName(json.RawMessage(raw)); got != want {
			t.Errorf("toolName(%s) = %q, want %q", raw, got, want)
		}
	}
}

func TestProviderBlockIsUsable(t *testing.T) {
	block := providerBlock(defaultConfig())
	for _, want := range []string{
		"base-url: https://opencode.ai/zen/v1",
		`- api-key: ""`,
		"User-Agent: \"opencode/1.18.31\"",
		"x-opencode-client: \"desktop\"",
		"- name: space-bunny-free",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("generated block is missing %q\n%s", want, block)
		}
	}
	// A populated key is sent upstream as a Bearer token and answered with 401.
	for _, line := range strings.Split(block, "\n") {
		if strings.Contains(line, "api-key:") && !strings.Contains(line, `api-key: ""`) {
			t.Errorf("generated a non-empty api key: %q", strings.TrimSpace(line))
		}
	}
	// Every identity header must carry a canonical id, or the tier refuses it.
	cfg := defaultConfig()
	cfg.SessionSalt = "salt"
	block = providerBlock(cfg)
	for _, prefix := range []string{"x-opencode-session:", "x-opencode-request:"} {
		for _, line := range strings.Split(block, "\n") {
			if strings.Contains(line, prefix) {
				id := strings.Trim(strings.TrimSpace(strings.SplitN(line, ":", 2)[1]), "\"")
				if !validCanonicalID(id) {
					t.Errorf("%s carries a malformed id %q", prefix, id)
				}
			}
		}
	}
}

func TestCanonicalIDShape(t *testing.T) {
	id := canonicalID("ses_", "seed")
	if !validCanonicalID(id) {
		t.Fatalf("generated id is not canonical: %q", id)
	}
	if !strings.HasPrefix(id, "ses_") {
		t.Fatalf("prefix lost: %q", id)
	}
	if got := canonicalID("msg_", "seed"); !validCanonicalID(got) || !strings.HasPrefix(got, "msg_") {
		t.Fatalf("msg id invalid: %q", got)
	}
	// A seeded id must be stable, or the prompt cache never hits.
	if canonicalID("ses_", "seed") != id {
		t.Fatal("seeded id is not deterministic")
	}
	if canonicalID("ses_", "other") == id {
		t.Fatal("different seeds must not collide")
	}
	if canonicalID("ses_", "") == canonicalID("ses_", "") {
		t.Fatal("unseeded ids must differ per call")
	}
}

func TestUserAgentVersionFloor(t *testing.T) {
	cases := map[string]bool{
		"opencode/1.18.31":         true,
		"opencode/1.17.0":          true,
		"opencode/2.0.1":           true,
		"opencode/1.16.9":          false,
		"opencode/1.9.0":           false,
		"opencode/0.9.9":           false,
		"curl/8.4.0":               false,
		"":                         false,
		"bun/1.18.31 opencode/cli": false,
	}
	for ua, want := range cases {
		if got := userAgentAccepted(ua); got != want {
			t.Errorf("userAgentAccepted(%q) = %v, want %v", ua, got, want)
		}
	}
}

func TestContractInjectsAllFiveParts(t *testing.T) {
	cfg := defaultConfig()
	body := []byte(`{"model":"space-bunny-free","messages":[{"role":"user","content":"hi"}]}`)
	inj := applyContract(body, map[string]string{}, "space-bunny-free", "openai", cfg, "seed-1")
	if inj == nil {
		t.Fatal("nothing was injected")
	}
	if !contains(inj.Applied, "stream") {
		t.Error("stream not forced")
	}
	if !contains(inj.Applied, "tools") {
		t.Error("tools not seeded")
	}
	for _, header := range []string{"User-Agent", "x-opencode-session", "x-opencode-request", "x-opencode-client", "x-opencode-project"} {
		if inj.Headers[header] == "" {
			t.Errorf("header %s missing", header)
		}
	}
	if !userAgentAccepted(inj.Headers["User-Agent"]) {
		t.Errorf("injected UA would be rejected: %q", inj.Headers["User-Agent"])
	}
	if !validCanonicalID(inj.Headers["x-opencode-session"]) {
		t.Errorf("injected session id invalid: %q", inj.Headers["x-opencode-session"])
	}

	var doc struct {
		Stream bool `json:"stream"`
		Tools  []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(inj.Body, &doc); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	if !doc.Stream {
		t.Error("stream not true in the rewritten body")
	}
	if len(doc.Tools) != len(defaultClientToolNames) {
		t.Fatalf("seeded %d tools, want %d", len(doc.Tools), len(defaultClientToolNames))
	}
	if doc.Tools[0].Function.Name != "bash" {
		t.Errorf("first tool is %q, want a real client name", doc.Tools[0].Function.Name)
	}
}

func TestContractIsIdempotent(t *testing.T) {
	cfg := defaultConfig()
	body := []byte(`{"model":"m","messages":[]}`)
	first := applyContract(body, map[string]string{}, "m", "openai", cfg, "seed")
	second := applyContract(first.Body, first.Headers, "m", "openai", cfg, "seed")
	if second != nil {
		t.Fatalf("a satisfied request was modified again: %v", second.Applied)
	}
}

func TestContractNeverDropsCallerTools(t *testing.T) {
	cfg := defaultConfig()
	body := []byte(`{"model":"m","stream":true,"tools":[{"type":"function","function":{"name":"get_weather"}}]}`)
	inj := applyContract(body, map[string]string{}, "m", "openai", cfg, "seed")
	var tools []json.RawMessage
	if err := json.Unmarshal(mustField(t, inj.Body, "tools"), &tools); err != nil {
		t.Fatal(err)
	}
	if toolName(tools[0]) != "get_weather" {
		t.Fatalf("the caller tool was dropped: %q", toolName(tools[0]))
	}
	if len(tools) != 1+len(defaultClientToolNames) {
		t.Fatalf("want caller tool + accepted names, got %d", len(tools))
	}
}

func TestContractAppendsRatherThanReplacesCallerTools(t *testing.T) {
	cfg := defaultConfig()
	body := []byte(`{"model":"m","stream":true,"tools":[{"type":"function","function":{"name":"get_weather"}}]}`)
	inj := applyContract(body, map[string]string{}, "m", "openai", cfg, "seed")
	if !contains(inj.Applied, "tools") {
		t.Fatal("accepted names were not appended")
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(mustField(t, inj.Body, "tools"), &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1+len(defaultClientToolNames) {
		t.Fatalf("want caller tool + %d accepted, got %d", len(defaultClientToolNames), len(tools))
	}
	if toolName(tools[0]) != "get_weather" {
		t.Fatalf("the caller tool was lost: %q", toolName(tools[0]))
	}
}

// Measured 2026-09-29: 4 of 5 free models answer 403 to an invented tool name and
// 200 to the full client set, so a partial set is topped up rather than trusted.
func TestContractTopsUpPartialToolSet(t *testing.T) {
	body := []byte(`{"model":"m","stream":true,"tools":[{"type":"function","function":{"name":"bash"}}]}`)
	inj := applyContract(body, map[string]string{}, "m", "openai", defaultConfig(), "seed")
	if !contains(inj.Applied, "tools") {
		t.Fatal("a partial accepted set was left short of names the gate needs")
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(mustField(t, inj.Body, "tools"), &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools) != len(defaultClientToolNames) {
		t.Fatalf("want the full accepted set, got %d", len(tools))
	}
	if toolName(tools[0]) != "bash" {
		t.Fatalf("the caller tool moved: %q", toolName(tools[0]))
	}
}

func TestContractLeavesCompleteToolSetAlone(t *testing.T) {
	tools := make([]string, 0, len(defaultClientToolNames))
	items := make([]string, 0, len(defaultClientToolNames))
	for _, n := range defaultClientToolNames {
		tools = append(tools, n)
		items = append(items, `{"type":"function","function":{"name":"`+n+`"}}`)
	}
	body := []byte(`{"model":"m","stream":true,"tools":[` + strings.Join(items, ",") + `]}`)
	inj := applyContract(body, map[string]string{}, "m", "openai", defaultConfig(), "seed")
	if contains(inj.Applied, "tools") {
		t.Error("a complete accepted tool set was modified")
	}
}

func mustField(t *testing.T, body []byte, key string) json.RawMessage {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	v, ok := doc[key]
	if !ok {
		t.Fatalf("field %q missing from %s", key, body)
	}
	return v
}

func TestContractHonoursPerModelTools(t *testing.T) {
	cfg := defaultConfig()
	cfg.Tools = map[string][]string{"muse-spark-1.3-contributor-free": {"bash"}}
	body := []byte(`{"model":"muse-spark-1.3-contributor-free","messages":[]}`)
	inj := applyContract(body, map[string]string{}, "muse-spark-1.3-contributor-free", "openai", cfg, "seed")
	var doc struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(inj.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Tools) != 1 || doc.Tools[0].Function.Name != "bash" {
		t.Fatalf("per-model tool set ignored, got %+v", doc.Tools)
	}
}

func TestContractPreservesKeyOrder(t *testing.T) {
	body := []byte(`{"model":"m","temperature":0.2,"max_tokens":10}`)
	inj := applyContract(body, map[string]string{}, "m", "openai", defaultConfig(), "seed")
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(inj.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["temperature"]; !ok {
		t.Error("an untouched field was dropped")
	}
	order := string(inj.Body)
	if !strings.HasPrefix(order, `{"model":`) {
		t.Errorf("key order was not preserved: %s", order[:60])
	}
}

func TestSeedIsStablePerConversation(t *testing.T) {
	cfg := defaultConfig()
	a := cfg.seedFor("sess-42", "m")
	b := cfg.seedFor("sess-42", "m")
	if a != b {
		t.Fatal("the same conversation produced two seeds")
	}
	if a == cfg.seedFor("sess-43", "m") {
		t.Fatal("two conversations shared a seed")
	}
	if a == cfg.seedFor("sess-42", "other") {
		t.Fatal("the same conversation on two models shared a seed")
	}
}

func TestResolveClientSession(t *testing.T) {
	if got := resolveClientSession(map[string]string{"X-Session-Id": "abc"}, nil); got != "abc" {
		t.Errorf("header ignored: %q", got)
	}
	body := []byte(`{"metadata":{"session_id":"from-meta"}}`)
	if got := resolveClientSession(map[string]string{}, body); got != "from-meta" {
		t.Errorf("metadata session ignored: %q", got)
	}
	if got := resolveClientSession(map[string]string{}, []byte(`{}`)); got != "" {
		t.Errorf("invented a session: %q", got)
	}
}

func TestConfigSetKeepsUnsetFields(t *testing.T) {
	s := &configState{cfg: defaultConfig()}
	s.cfg.SessionSalt = "keep-me"
	s.set(contractConfig{Enabled: true})
	if got := s.get(); got.SessionSalt != "keep-me" || len(got.Models) == 0 {
		t.Fatalf("a partial write dropped config: %+v", got)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
