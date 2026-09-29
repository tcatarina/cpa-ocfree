package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// The contract forces stream:true upstream, so a caller that asked for JSON gets
// an event stream back. This remembers which requests were non-streaming and
// rebuilds a single chat.completion for them; a streaming caller is left alone.

type requestNote struct {
	model      string
	clientJSON bool
	at         time.Time
}

var (
	noteMu    sync.Mutex
	noteByID  = map[string]requestNote{}
	noteLimit = 4096
)

func rememberRequest(id string, model string, stream bool) {
	if id == "" {
		return
	}
	noteMu.Lock()
	defer noteMu.Unlock()
	if len(noteByID) >= noteLimit {
		cutoff := time.Now().Add(-10 * time.Minute)
		for k, v := range noteByID {
			if v.at.Before(cutoff) {
				delete(noteByID, k)
			}
		}
	}
	noteByID[id] = requestNote{model: model, clientJSON: !stream, at: time.Now()}
}

func takeRequest(id string) (requestNote, bool) {
	if id == "" {
		return requestNote{}, false
	}
	noteMu.Lock()
	defer noteMu.Unlock()
	note, ok := noteByID[id]
	if ok {
		delete(noteByID, id)
	}
	return note, ok
}

type sseChunk struct {
	ID      string          `json:"id"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Object  string          `json:"object"`
	Choices []sseChoice     `json:"choices"`
	Usage   json.RawMessage `json:"usage"`
}

type sseChoice struct {
	Index        int      `json:"index"`
	Delta        sseDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

type sseDelta struct {
	Role             string        `json:"role"`
	Content          string        `json:"content"`
	ReasoningContent string        `json:"reasoning_content"`
	Name             string        `json:"name"`
	ToolCalls        []sseToolCall `json:"tool_calls"`
}

type sseToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// looksLikeSSE reports whether the body is an event stream. A stream may open
// with comment lines — some models answer ": keep-alive" while thinking — so the
// first data/event line is looked for rather than the very first byte.
func looksLikeSSE(body []byte) bool {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		return bytes.HasPrefix(line, []byte("data:")) || bytes.HasPrefix(line, []byte("event:"))
	}
	return false
}

// rebuildChatCompletion folds an OpenAI chat event stream back into the single
// JSON body a non-streaming caller expects.
// upstreamErrorInStream finds an error the upstream injected into the event
// stream. A forced stream means a mid-stream 503 arrives as a data event, not an
// HTTP status, so without this the rebuild would answer 200 with empty content
// and the caller would read a real outage as a model that said nothing.
func upstreamErrorInStream(body []byte) (string, bool) {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[len("data:"):])
		if len(payload) == 0 || bytes.Contains(payload, []byte("[DONE]")) {
			continue
		}
		var probe struct {
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(payload, &probe); err != nil {
			continue
		}
		if probe.Error != nil {
			kind := probe.Error.Type
			if kind == "" {
				kind = "upstream_error"
			}
			return kind + ": " + probe.Error.Message, true
		}
		if probe.Message != "" && bytes.Contains(payload, []byte("\"error\"")) {
			return probe.Message, true
		}
	}
	return "", false
}

func rebuildChatCompletion(body []byte) ([]byte, bool) {
	// Never turn an error into a completion. The caller surfaces the error
	// separately; refusing here keeps that guarantee local to the fold.
	if _, failed := upstreamErrorInStream(body); failed {
		return nil, false
	}
	var (
		id        string
		created   int64
		model     string
		role      string
		content   strings.Builder
		reasoning strings.Builder
		finish    any
		usage     json.RawMessage
		tools     = map[int]*sseToolCall{}
		order     []int
		sawChunk  bool
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
		var chunk sseChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			return nil, false
		}
		sawChunk = true
		if chunk.ID != "" {
			id = chunk.ID
		}
		if chunk.Created != 0 {
			created = chunk.Created
		}
		if chunk.Model != "" {
			model = chunk.Model
		}
		if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Role != "" {
				role = choice.Delta.Role
			}
			content.WriteString(choice.Delta.Content)
			reasoning.WriteString(choice.Delta.ReasoningContent)
			if choice.FinishReason != nil {
				finish = *choice.FinishReason
			}
			for _, tc := range choice.Delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}
				cur, ok := tools[idx]
				if !ok {
					cur = &sseToolCall{Index: tc.Index, Type: "function"}
					if cur.Type == "" {
						cur.Type = "function"
					}
					tools[idx] = cur
					order = append(order, idx)
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Type != "" {
					cur.Type = tc.Type
				}
				cur.Function.Name += tc.Function.Name
				cur.Function.Arguments += tc.Function.Arguments
			}
		}
	}
	if !sawChunk {
		return nil, false
	}
	if content.Len() == 0 {
		// The upstream produced a stream with no content. Worth seeing: a
		// truncated body and a model that only reasoned look identical here.
		logWarn("rebuild produced empty content; chunks=" + itoa(countChunks(body)) +
			" reasoning=" + itoa(reasoning.Len()) + " finish=" + toString(finish) +
			" body=" + snippet(body))
	}
	if finish == nil {
		finish = "stop"
	}
	if role == "" {
		role = "assistant"
	}

	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(order) > 0 {
		calls := make([]map[string]any, 0, len(order))
		for _, idx := range order {
			tc := tools[idx]
			calls = append(calls, map[string]any{
				"id":       tc.ID,
				"type":     tc.Type,
				"function": map[string]any{"name": tc.Function.Name, "arguments": tc.Function.Arguments},
			})
		}
		message["tool_calls"] = calls
	}

	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
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

func toString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}
