package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExecutorOwnsOnlyMuseSpark(t *testing.T) {
	for _, id := range []string{"muse-spark-1.3-contributor-free", "muse-spark-1.2-contributor-free"} {
		if !executedByPlugin(id) {
			t.Errorf("%s should be executor-owned", id)
		}
	}
	for _, id := range []string{"space-bunny-free", "step-5-preview-free", "big-pickle", ""} {
		if executedByPlugin(id) {
			t.Errorf("%s must stay on the normal path", id)
		}
	}
}

func TestModelRegisterAdvertisesExecutorModels(t *testing.T) {
	out, err := handleExecutorModelRegister()
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Provider string `json:"provider"`
			Models   []struct {
				ID            string `json:"ID"`
				OwnedBy       string `json:"OwnedBy"`
				ContextLength int    `json:"ContextLength"`
			} `json:"models"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil {
		t.Fatalf("unreadable envelope: %v", err)
	}
	if env.Result.Provider != executorProviderName {
		t.Errorf("provider = %q, want %q", env.Result.Provider, executorProviderName)
	}
	if len(env.Result.Models) != len(executorModels) {
		t.Fatalf("models = %v, want %v", env.Result.Models, executorModels)
	}
	for _, m := range env.Result.Models {
		if m.OwnedBy != executorProviderName {
			t.Errorf("%s owned_by = %q", m.ID, m.OwnedBy)
		}
		if m.ContextLength != executorContextLength {
			t.Errorf("%s context_length = %d", m.ID, m.ContextLength)
		}
	}
}

func TestExecuteRefusesForeignModel(t *testing.T) {
	raw, _ := json.Marshal(rpcExecutorRequest{Model: "space-bunny-free"})
	out, err := handleExecute(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "no_target") {
		t.Errorf("foreign model was not refused: %s", out)
	}
}

// responsesSSE is a canned Responses event stream: deltas, then the completed
// event carrying the whole object, like the live surface sends.
const responsesSSE = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","model":"muse-spark-1.3-contributor-free"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","delta":"OK"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","model":"muse-spark-1.3-contributor-free","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]}}

data: [DONE]
`

func TestFoldPrefersCompletedObject(t *testing.T) {
	raw, ok := foldResponsesStream([]byte(responsesSSE), "muse-spark-1.3-contributor-free")
	if !ok {
		t.Fatal("fold failed")
	}
	var doc struct {
		Object string `json:"object"`
		ID     string `json:"id"`
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("folded body is not JSON: %v", err)
	}
	if doc.Object != "response" || doc.Output[0].Content[0].Text != "OK" {
		t.Errorf("fold lost the answer: %s", raw)
	}
}

func TestFoldSynthesisesFromDeltas(t *testing.T) {
	sse := `data: {"type":"response.output_text.delta","delta":"hel"}

data: {"type":"response.output_text.delta","delta":"lo"}

data: [DONE]
`
	raw, ok := foldResponsesStream([]byte(sse), "m")
	if !ok {
		t.Fatal("fold failed")
	}
	var doc struct {
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("folded body is not JSON: %v", err)
	}
	if doc.Output[0].Content[0].Text != "hello" {
		t.Errorf("text = %q, want hello", doc.Output[0].Content[0].Text)
	}
}

func TestFoldRejectsEmptyStream(t *testing.T) {
	if _, ok := foldResponsesStream([]byte("data: [DONE]\n"), "m"); ok {
		t.Error("an empty stream folded into an answer")
	}
}

// fakeZen stands in for the upstream Responses endpoint and records what the
// plugin actually sends: the URL path, the identity headers, and the body.
func fakeZen(t *testing.T, status int, body string) (*httptest.Server, *map[string]string, *[]byte) {
	t.Helper()
	seen := map[string]string{}
	var seenBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen["path"] = r.URL.Path
		seen["user-agent"] = r.Header.Get("User-Agent")
		seen["session"] = r.Header.Get("x-opencode-session")
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	return srv, &seen, &seenBody
}

func TestExecutePostsResponsesVerbatim(t *testing.T) {
	srv, seen, seenBody := fakeZen(t, 200, responsesSSE)
	defer srv.Close()

	prev := state.get()
	defer state.set(prev)
	state.set(contractConfig{Enabled: true, Client: "desktop", Project: "global", Upstream: srv.URL})

	raw, _ := json.Marshal(rpcExecutorRequest{
		Model:  "muse-spark-1.3-contributor-free",
		Format: "openai-response",
		Payload: []byte(`{"model":"muse-spark-1.3-contributor-free",` +
			`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
	})
	out, err := handleExecute(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			// Payload crosses the RPC boundary base64-encoded, like every
			// []byte field; the host decodes it before use.
			Payload string `json:"payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("execute failed: %s", out)
	}
	if (*seen)["path"] != "/responses" {
		t.Errorf("posted to %q, want /responses", (*seen)["path"])
	}
	if !validCanonicalID((*seen)["session"]) {
		t.Errorf("session header %q is not canonical", (*seen)["session"])
	}
	var sent struct {
		Stream bool  `json:"stream"`
		Tools  []any `json:"tools"`
	}
	if err := json.Unmarshal(*seenBody, &sent); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if !sent.Stream || len(sent.Tools) == 0 {
		t.Errorf("contract missing upstream: stream=%v tools=%d", sent.Stream, len(sent.Tools))
	}
	var doc struct {
		Output []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	folded, err := base64.StdEncoding.DecodeString(env.Result.Payload)
	if err != nil {
		t.Fatalf("payload is not base64: %v", err)
	}
	if err := json.Unmarshal(folded, &doc); err != nil {
		t.Fatalf("folded payload is not JSON: %v", err)
	}
	if doc.Output[0].Content[0].Text != "OK" {
		t.Errorf("answer = %q", doc.Output[0].Content[0].Text)
	}
}

func TestExecuteSurfacesUpstreamError(t *testing.T) {
	srv, _, _ := fakeZen(t, 400, `{"error":{"type":"ModelError","message":"nope"}}`)
	defer srv.Close()

	prev := state.get()
	defer state.set(prev)
	state.set(contractConfig{Enabled: true, Client: "desktop", Project: "global", Upstream: srv.URL})

	raw, _ := json.Marshal(rpcExecutorRequest{
		Model:   "muse-spark-1.3-contributor-free",
		Payload: []byte(`{"model":"muse-spark-1.3-contributor-free","input":"hi"}`),
	})
	out, err := handleExecute(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "upstream_error") || !strings.Contains(string(out), "400") {
		t.Errorf("upstream 400 was not surfaced: %s", out)
	}
}
