package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The Muse Spark family is served only on the Responses API, but CPA's
// openai-compatibility executor translates every responses request into chat
// completions and posts it to /chat/completions, which this family refuses
// with 400 ModelProtocolUnsupported. The request interceptor cannot change
// that: translation happens downstream of interception, inside the executor.
//
// So the plugin executes these two models itself — same binary, same config,
// no server fork. It registers them under its own provider, posts the
// Responses-shaped body verbatim to {upstream}/responses, and relays the
// answer. Everything else keeps flowing through the normal path.

const executorProviderName = "ocfree"

// executorModels are the ids this plugin executes directly. Small on purpose:
// every entry here is a model the plugin owns end to end, including the
// protocol translation the host would otherwise do.
var executorModels = []string{
	"muse-spark-1.3-contributor-free",
	"muse-spark-1.2-contributor-free",
}

// executorContextLength matches the published OpenCode catalogue window.
const executorContextLength = 1048576

func executedByPlugin(model string) bool {
	for _, id := range executorModels {
		if model == id {
			return true
		}
	}
	return false
}

type rpcExecutorRequest struct {
	Model           string      `json:"model"`
	Format          string      `json:"format"`
	SourceFormat    string      `json:"source_format"`
	Stream          bool        `json:"stream"`
	StreamID        string      `json:"stream_id"`
	Headers         http.Header `json:"headers"`
	Payload         []byte      `json:"payload"`
	OriginalRequest []byte      `json:"original_request"`
	HostCallbackID  string      `json:"host_callback_id,omitempty"`
}

// upstreamTimeout bounds a collected (non-streaming) call. Stream relay reads
// until the upstream closes, so reasoning models are not cut off mid-thought.
const upstreamTimeout = 5 * time.Minute

// executorStreamResponse returns headers only. The host opens a push stream and
// passes its stream_id to the plugin, so chunks are emitted asynchronously with
// host.stream.emit instead of being batched into this reply.
type executorStreamResponse struct {
	Headers http.Header `json:"headers,omitempty"`
}

type streamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type streamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

func emitPluginStreamChunk(streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("stream id is required to emit")
	}
	_, err := callHost(pluginabi.MethodHostStreamEmit, streamEmitRequest{
		StreamID: streamID,
		Payload:  payload,
	})
	return err
}

func closePluginStream(streamID string, cause error) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	_, _ = callHost(pluginabi.MethodHostStreamClose, streamCloseRequest{StreamID: streamID, Error: msg})
}

// handleExecutorModelRegister advertises the models this plugin executes
// itself, under its own provider. They must not also sit in the CPA provider
// block: a model with a native executor is never claimed by a plugin.
func handleExecutorModelRegister() ([]byte, error) {
	models := make([]pluginapi.ModelInfo, 0, len(executorModels))
	for _, id := range executorModels {
		models = append(models, pluginapi.ModelInfo{
			ID:            id,
			Object:        "model",
			OwnedBy:       executorProviderName,
			Type:          "model",
			DisplayName:   id,
			Name:          id,
			ContextLength: executorContextLength,
		})
	}
	return okEnvelope(pluginapi.ModelRegistrationResponse{
		Provider: executorProviderName,
		Models:   models,
	})
}

// handleExecute serves an executor-owned model straight from the upstream
// Responses API. The body the host hands over is already in the executor's
// input format; the contract fixups still run because a bare caller body
// reaches the executor as bare as it reached the interceptor.
func handleExecute(raw []byte, stream bool) ([]byte, error) {
	var req rpcExecutorRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	model := strings.TrimSpace(req.Model)
	if model == "" || !executedByPlugin(model) {
		return errorEnvelope("no_target", "ocfree executes only "+strings.Join(executorModels, ", ")), nil
	}
	cfg := state.get()
	if !cfg.Enabled {
		return errorEnvelope("disabled", "ocfree plugin is disabled"), nil
	}

	body := req.Payload
	if len(body) == 0 {
		body = req.OriginalRequest
	}
	headers := headerMap(req.Headers)
	seed := resolveClientSession(headers, body)
	if inj := applyContract(body, headers, model, "openai-response", cfg, cfg.seedFor(seed, model)); inj != nil {
		if len(inj.Body) > 0 {
			body = inj.Body
		}
		for k, v := range inj.Headers {
			headers[k] = v
		}
	}

	upstream := strings.TrimRight(cfg.upstream(), "/") + "/responses"
	if stream {
		streamID := strings.TrimSpace(req.StreamID)
		if streamID == "" {
			return errorEnvelope("bad_request", "stream_id is required for streaming"), nil
		}
		go func() {
			closePluginStream(streamID, relayUpstreamStream(upstream, model, body, headers, streamID))
		}()
		return okEnvelope(executorStreamResponse{
			Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		})
	}

	respBody, status, err := postUpstream(upstream, body, headers, upstreamTimeout)
	if err != nil {
		return errorEnvelope("upstream_error", err.Error()), nil
	}
	if status != http.StatusOK {
		return errorEnvelope("upstream_error",
			fmt.Sprintf("upstream answered %d: %s", status, snippet(respBody))), nil
	}
	folded, ok := foldResponsesStream(respBody, model)
	if !ok {
		return errorEnvelope("upstream_error", "unreadable event stream"), nil
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return okEnvelope(pluginapi.ExecutorResponse{Payload: folded, Headers: h})
}

// postUpstream sends one Responses request with the client-contract identity.
// Keyless on purpose, like the provider block: a populated Authorization
// header is refused upstream with 401.
func postUpstream(url string, body []byte, headers map[string]string, timeout time.Duration) ([]byte, int, error) {
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if ua := headers["User-Agent"]; userAgentAccepted(ua) {
		httpReq.Header.Set("User-Agent", ua)
	} else {
		httpReq.Header.Set("User-Agent", defaultUserAgent)
	}
	for _, k := range []string{"x-opencode-session", "x-opencode-request", "x-opencode-client", "x-opencode-project"} {
		if v := lookupHeader(headers, k); v != "" {
			httpReq.Header.Set(k, v)
		}
	}
	if v := lookupHeader(headers, "x-opencode-session"); !validCanonicalID(v) {
		httpReq.Header.Set("x-opencode-session", canonicalID("ses_", ""))
	}
	if v := lookupHeader(headers, "x-opencode-request"); !validCanonicalID(v) {
		httpReq.Header.Set("x-opencode-request", canonicalID("msg_", ""))
	}
	if httpReq.Header.Get("x-opencode-client") == "" {
		httpReq.Header.Set("x-opencode-client", "desktop")
	}
	if httpReq.Header.Get("x-opencode-project") == "" {
		httpReq.Header.Set("x-opencode-project", "global")
	}

	resp, err := (&http.Client{Timeout: timeout}).Do(httpReq)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	return raw, resp.StatusCode, nil
}

// relayUpstreamStream posts with an open-ended read and emits every event line
// to the host stream verbatim. The caller asked for the upstream's stream, so
// there is nothing to translate — only to forward.
func relayUpstreamStream(url, model string, body []byte, headers map[string]string, streamID string) error {
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if ua := headers["User-Agent"]; userAgentAccepted(ua) {
		httpReq.Header.Set("User-Agent", ua)
	} else {
		httpReq.Header.Set("User-Agent", defaultUserAgent)
	}
	for _, k := range []string{"x-opencode-session", "x-opencode-request", "x-opencode-client", "x-opencode-project"} {
		if v := lookupHeader(headers, k); v != "" {
			httpReq.Header.Set(k, v)
		}
	}

	resp, err := (&http.Client{}).Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("upstream answered %d: %s", resp.StatusCode, snippet(raw))
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	emitted := false
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if err := emitPluginStreamChunk(streamID, append(append([]byte(nil), line...), '\n')); err != nil {
			return err
		}
		emitted = true
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !emitted {
		return fmt.Errorf("upstream sent an empty stream")
	}
	return nil
}

// foldResponsesStream rebuilds the single Responses object a non-streaming
// caller expects. The completed event carries the whole object, so it is used
// verbatim when present; otherwise the answer is synthesised from the text
// deltas, which is exactly what the event stream contains.
func foldResponsesStream(body []byte, model string) ([]byte, bool) {
	var (
		text      strings.Builder
		id        string
		usage     json.RawMessage
		completed json.RawMessage
		sawData   bool
	)
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		sawData = true
		var evt struct {
			Type     string          `json:"type"`
			Delta    string          `json:"delta"`
			Response json.RawMessage `json:"response"`
			Usage    json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(payload, &evt); err != nil {
			continue
		}
		switch evt.Type {
		case "response.completed":
			if len(evt.Response) > 0 {
				completed = evt.Response
			}
		case "response.output_text.delta":
			text.WriteString(evt.Delta)
		}
		if len(evt.Usage) > 0 {
			usage = evt.Usage
		}
		var probe struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(payload, &probe); err == nil && probe.ID != "" && id == "" {
			id = probe.ID
		}
	}
	if len(completed) > 0 {
		return completed, true
	}
	if !sawData {
		return nil, false
	}
	out := map[string]any{
		"id":     id,
		"object": "response",
		"model":  model,
		"output": []any{map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []any{map[string]any{
				"type": "output_text", "text": text.String(), "annotations": []any{},
			}},
		}},
	}
	if len(usage) > 0 {
		out["usage"] = usage
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return raw, true
}
