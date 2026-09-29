package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

type rpcRequestInterceptRequest struct {
	pluginapi.RequestInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

func applyConfig(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var req rpcPluginLifecycle
	if err := json.Unmarshal(raw, &req); err != nil {
		return err
	}
	if len(req.ConfigYAML) == 0 {
		return nil
	}
	var cfg contractConfig
	if err := yaml.Unmarshal(req.ConfigYAML, &cfg); err != nil {
		return err
	}
	state.set(cfg)
	logInfo("configured models=" + strings.Join(state.get().Models, ","))
	return nil
}

// handleInterceptBefore satisfies the free tier for requests aimed at one of the
// configured models. Anything else is returned untouched.
func handleInterceptBefore(raw []byte) ([]byte, error) {
	var req rpcRequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	cfg := state.get()
	if !cfg.Enabled {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}

	model := firstNonEmpty(strings.TrimSpace(req.Model), strings.TrimSpace(req.RequestedModel))
	if model == "" || !cfg.targets()[model] {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}
	// Remembered so the forced upstream stream can be folded back for a caller
	// that asked for JSON.
	rememberRequest(req.RequestID, model, req.Stream)
	if len(req.Body) == 0 {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}

	headers := headerMap(req.Headers)
	clientSession := resolveClientSession(headers, req.Body)
	inj := applyContract(req.Body, headers, model, req.SourceFormat, cfg, cfg.seedFor(clientSession, model))
	if inj == nil {
		return okEnvelope(pluginapi.RequestInterceptResponse{})
	}

	// x-opencode-request is per attempt, never seeded from the conversation, so it
	// is regenerated on every pass while the session stays put.
	if req.RequestID != "" {
		if _, seeded := inj.Headers["x-opencode-request"]; seeded {
			inj.Headers["x-opencode-request"] = canonicalID("msg_", req.RequestID)
		}
	}

	h := make(http.Header, len(inj.Headers))
	for k, v := range inj.Headers {
		h.Set(k, v)
	}
	out := pluginapi.RequestInterceptResponse{Headers: h, Body: inj.Body}
	logInfo("contract model=" + model + " applied=" + strings.Join(inj.Applied, ","))
	return okEnvelope(out)
}

func headerMap(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

type rpcResponseInterceptRequest struct {
	pluginapi.ResponseInterceptRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// handleInterceptAfter rebuilds a JSON body for a caller that asked for one.
// The contract forces stream:true upstream, so without this a non-streaming
// client receives an event stream it cannot read.
func handleInterceptAfter(raw []byte) ([]byte, error) {
	var req rpcResponseInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	if req.Stream {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	note, known := takeRequest(req.RequestID)
	if !known || !note.clientJSON {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	if len(req.Body) == 0 || !looksLikeSSE(req.Body) {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	// A mid-stream error is a real failure, not an empty answer. Surfacing it is
	// better than fabricating a completion, and better than passing an event
	// stream to a caller that asked for JSON.
	if message, failed := upstreamErrorInStream(req.Body); failed {
		logWarn("upstream error in forced stream: " + message)
		raw, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"type":    "upstream_error",
				"message": message,
			},
		})
		h := cloneHeader(req.ResponseHeaders)
		if h == nil {
			h = http.Header{}
		}
		h.Set("Content-Type", "application/json")
		return okEnvelope(pluginapi.ResponseInterceptResponse{Headers: h, Body: raw})
	}

	rebuilt, ok := rebuildChatCompletion(req.Body)
	if !ok {
		return okEnvelope(pluginapi.ResponseInterceptResponse{})
	}
	h := cloneHeader(req.ResponseHeaders)
	if h == nil {
		h = http.Header{}
	}
	h.Set("Content-Type", "application/json")
	logInfo("rebuilt json body for " + note.model)
	return okEnvelope(pluginapi.ResponseInterceptResponse{Headers: h, Body: rebuilt})
}

func cloneHeader(h http.Header) http.Header {
	if h == nil {
		return nil
	}
	out := make(http.Header, len(h))
	for k, v := range h {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// resolveClientSession finds the identity the caller already uses, so its own
// conversation maps onto one upstream session instead of a fresh one per request.
func resolveClientSession(headers map[string]string, body []byte) string {
	for _, name := range []string{
		"X-Opencode-Session", "X-Session-Affinity", "X-Session-Id",
		"X-Claude-Code-Session-Id", "Session-Id",
	} {
		if v := strings.TrimSpace(lookupHeader(headers, name)); v != "" {
			return v
		}
	}
	var doc struct {
		SessionID any `json:"session_id"`
		SessionId any `json:"sessionId"`
		ThreadID  any `json:"thread_id"`
		Metadata  struct {
			SessionID any `json:"session_id"`
			ThreadID  any `json:"thread_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return ""
	}
	for _, v := range []any{doc.SessionID, doc.SessionId, doc.ThreadID, doc.Metadata.SessionID, doc.Metadata.ThreadID} {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func lookupHeader(headers map[string]string, name string) string {
	if v, ok := headers[name]; ok {
		return v
	}
	for k, v := range headers {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func sortedKeys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
