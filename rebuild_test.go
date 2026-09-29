package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const sampleSSE = "data: {\"id\":\"x1\",\"created\":123,\"model\":\"space-bunny-free\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"think\"}}]}\n\n" +
	"data: {\"id\":\"x1\",\"created\":123,\"model\":\"space-bunny-free\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"PONG\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":9}}\n\n" +
	"data: [DONE]\n\n"

func TestLooksLikeSSE(t *testing.T) {
	if !looksLikeSSE([]byte(sampleSSE)) {
		t.Error("event stream not detected")
	}
	if looksLikeSSE([]byte(`{"a":1}`)) {
		t.Error("plain JSON misdetected as a stream")
	}
}

func TestRebuildChatCompletion(t *testing.T) {
	raw, ok := rebuildChatCompletion([]byte(sampleSSE))
	if !ok {
		t.Fatal("rebuild failed")
	}
	var doc struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("rebuilt body is not JSON: %v", err)
	}
	if doc.Object != "chat.completion" {
		t.Errorf("object = %q", doc.Object)
	}
	if doc.Choices[0].Message.Content != "PONG" {
		t.Errorf("content = %q", doc.Choices[0].Message.Content)
	}
	if doc.Choices[0].Message.ReasoningContent != "think" {
		t.Errorf("reasoning lost: %q", doc.Choices[0].Message.ReasoningContent)
	}
	if doc.Choices[0].FinishReason != "stop" {
		t.Errorf("finish = %q", doc.Choices[0].FinishReason)
	}
	if doc.Usage.TotalTokens != 9 {
		t.Errorf("usage lost: %d", doc.Usage.TotalTokens)
	}
}

func TestLooksLikeSSESkipsLeadingComments(t *testing.T) {
	// nemotron-3-ultra-free opens with keep-alive comments while it thinks.
	stream := ": keep-alive\n: keep-alive\n\ndata: {\"id\":\"x\",\"choices\":[]}\n\ndata: [DONE]\n\n"
	if !looksLikeSSE([]byte(stream)) {
		t.Error("a stream behind keep-alive comments was not detected")
	}
	raw, ok := rebuildChatCompletion([]byte(stream))
	if !ok {
		t.Fatal("rebuild failed on a comment-prefixed stream")
	}
	var doc struct {
		Object  string `json:"object"`
		Choices []any  `json:"choices"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("rebuilt body is not JSON: %v", err)
	}
	if doc.Object != "chat.completion" {
		t.Errorf("object = %q", doc.Object)
	}
	if bytes.Contains(raw, []byte("keep-alive")) {
		t.Error("a comment line leaked into the rebuilt body")
	}
}

// A forced stream turns a mid-stream 503 into a data event. Measured 2026-09-29:
// upstream sent `Streaming response failed: [503] Upstream error from Nvidia`.
func TestUpstreamErrorInStreamIsFound(t *testing.T) {
	body := `data: {"error":{"type":"server_error","message":"Streaming response failed: [503] Upstream error from Nvidia: Service temporarily overloaded"}}` + "\n\ndata: [DONE]\n\n"
	msg, failed := upstreamErrorInStream([]byte(body))
	if !failed {
		t.Fatal("a mid-stream error was not detected")
	}
	if !strings.Contains(msg, "503") {
		t.Errorf("error message lost the status: %q", msg)
	}
}

func TestUpstreamErrorInStreamIgnoresGoodStreams(t *testing.T) {
	if _, failed := upstreamErrorInStream([]byte(sampleSSE)); failed {
		t.Error("a healthy stream was reported as an error")
	}
}

func TestRebuildRefusesToFabricateOverAnError(t *testing.T) {
	body := `data: {"error":{"type":"server_error","message":"boom"}}` + "\n\n"
	if _, ok := rebuildChatCompletion([]byte(body)); ok {
		t.Error("an error event was rebuilt into a completion")
	}
}

func TestRebuildCarriesToolCalls(t *testing.T) {
	// Built by marshalling rather than by hand: a hand-written fixture here is
	// easy to malform and then the test fails for the wrong reason.
	toolDelta := func(id, name, args string, finish string) string {
		idx := 0
		tc := map[string]any{
			"index":    idx,
			"function": map[string]any{},
		}
		fn := tc["function"].(map[string]any)
		if id != "" {
			tc["id"] = id
			tc["type"] = "function"
		}
		if name != "" {
			fn["name"] = name
		}
		if args != "" {
			fn["arguments"] = args
		}
		choice := map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{tc}}}
		if finish != "" {
			choice["finish_reason"] = finish
		}
		raw, err := json.Marshal(map[string]any{
			"id": "x", "created": 1, "model": "m", "object": "chat.completion.chunk",
			"choices": []any{choice},
		})
		if err != nil {
			t.Fatal(err)
		}
		return "data: " + string(raw) + "\n\n"
	}
	sse := toolDelta("call_1", "get_", "", "") +
		toolDelta("", "weather", `{"city":`, "") +
		toolDelta("", "", `"Paris"}`, "") +
		toolDelta("", "", "", "tool_calls") +
		"data: [DONE]\n\n"
	raw, ok := rebuildChatCompletion([]byte(sse))
	if !ok {
		t.Fatal("rebuild failed")
	}
	var doc struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	calls := doc.Choices[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(calls))
	}
	if calls[0].Function.Name != "get_weather" {
		t.Errorf("streamed name not joined: %q", calls[0].Function.Name)
	}
	if calls[0].Function.Arguments != `{"city":"Paris"}` {
		t.Errorf("streamed arguments not joined: %q", calls[0].Function.Arguments)
	}
	if calls[0].ID != "call_1" {
		t.Errorf("tool call id lost: %q", calls[0].ID)
	}
}

func TestRebuildDefaultsFinishReason(t *testing.T) {
	sse := "data: {\"id\":\"x\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	raw, ok := rebuildChatCompletion([]byte(sse))
	if !ok {
		t.Fatal("rebuild failed")
	}
	var doc struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	_ = json.Unmarshal(raw, &doc)
	if doc.Choices[0].FinishReason != "stop" {
		t.Errorf("finish = %q, want stop", doc.Choices[0].FinishReason)
	}
}

func TestRebuildRejectsNonStream(t *testing.T) {
	if _, ok := rebuildChatCompletion([]byte(`{"object":"chat.completion"}`)); ok {
		t.Error("a plain body was treated as a stream")
	}
}

func TestRequestNoteLifecycle(t *testing.T) {
	rememberRequest("req-1", "m", false)
	note, ok := takeRequest("req-1")
	if !ok || !note.clientJSON || note.model != "m" {
		t.Fatalf("note lost: %+v ok=%v", note, ok)
	}
	if _, ok := takeRequest("req-1"); ok {
		t.Error("a note was handed out twice")
	}
	rememberRequest("req-2", "m", true)
	if note, _ := takeRequest("req-2"); note.clientJSON {
		t.Error("a streaming caller was marked as JSON")
	}
}
