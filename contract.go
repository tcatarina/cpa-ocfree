package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"strings"
)

// The upstream free tier answers 403 unless the request carries all of:
//
//  1. stream: true
//  2. a non-empty tools array whose names the upstream currently accepts
//  3. canonical ses_/msg_ identity headers
//  4. a User-Agent naming opencode with major >= 1
//
// A request that omits the User-Agent never reaches the tier gate at all: the
// edge answers 403 Cloudflare 1010 first. Measured 2026-09-29 against
// https://opencode.ai/zen/v1/chat/completions.

const (
	base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	minUserAgentMi = 17
	// session/request ids are the shape the upstream checks, not the value.
	sessionHexLen   = 6
	sessionBase62Ln = 14
)

// defaultClientToolNames is the tool set the official client declares. The
// upstream accepts these names on the working free models and refuses smaller or
// invented sets, so a request carrying no tools borrows them. Per-model overrides
// live in the contract config because acceptance is not uniform.
var defaultClientToolNames = []string{
	"bash", "read", "write", "edit", "glob", "grep",
	"list", "todowrite", "webfetch", "task", "patch", "todoread",
}

// canonicalID renders `<prefix><12 hex><14 base62>`, the shape the upstream
// validates. A seed makes it deterministic, which is what keeps one conversation
// on one upstream session and therefore keeps the prompt cache warm: a fresh id
// per request would miss the cache on every turn.
func canonicalID(prefix, seed string) string {
	var sum []byte
	if seed != "" {
		d := sha256.Sum256([]byte("opencode\x00" + prefix + "\x00" + seed))
		sum = d[:]
	} else {
		d := sha256.Sum256([]byte(prefix + "\x00" + randomEntropy()))
		sum = d[:]
	}
	hexPart := hexEncode(sum[:sessionHexLen])
	rest := sum[sessionHexLen:]
	b62 := make([]byte, 0, sessionBase62Ln)
	for i := 0; len(b62) < sessionBase62Ln; i++ {
		b62 = append(b62, base62Alphabet[int(rest[i%len(rest)])%len(base62Alphabet)])
	}
	return prefix + hexPart + string(b62)
}

func hexEncode(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}

func validCanonicalID(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) != 4+sessionHexLen*2+sessionBase62Ln {
		return false
	}
	if v[:4] != "ses_" && v[:4] != "msg_" {
		return false
	}
	for _, c := range v[4 : 4+sessionHexLen*2] {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	for _, c := range v[4+sessionHexLen*2:] {
		if !strings.ContainsRune(base62Alphabet, c) {
			return false
		}
	}
	return true
}

// userAgentAccepted mirrors the upstream version floor: below 1.17 it answers
// 426 UpgradeRequired instead of reaching the tier gate.
func userAgentAccepted(ua string) bool {
	v := strings.ToLower(strings.TrimSpace(ua))
	idx := strings.Index(v, "opencode/")
	if idx < 0 {
		return false
	}
	rest := v[idx+len("opencode/"):]
	rest = strings.TrimPrefix(rest, "v")
	dot := strings.Index(rest, ".")
	if dot < 0 {
		return false
	}
	major, ok := atoi(rest[:dot])
	if !ok {
		return false
	}
	minorStr := rest[dot+1:]
	for i, c := range minorStr {
		if c < '0' || c > '9' {
			minorStr = minorStr[:i]
			break
		}
	}
	minor, ok := atoi(minorStr)
	if !ok {
		return false
	}
	return major > 1 || (major == 1 && minor >= minUserAgentMi)
}

// injection is the minimal set of changes applied to one upstream request.
type injection struct {
	Headers     map[string]string
	ClearHeader []string
	Body        []byte
	Applied     []string
}

// applyContract returns the changes needed to satisfy the free tier, or nil when
// the request already satisfies it. It never fails: a request it cannot repair is
// returned untouched so the upstream can answer with its own error.
func applyContract(body []byte, headers map[string]string, model, format string, cfg contractConfig, seed string) *injection {
	inj := &injection{Headers: map[string]string{}}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil
	}

	// 1. stream: true. The client asked for JSON; the upstream will only stream,
	// so the reply is rebuilt downstream. Force it either way.
	if !rawIs(doc["stream"], "true") {
		doc["stream"] = json.RawMessage("true")
		inj.Applied = append(inj.Applied, "stream")
	}

	// 2. non-empty tools carrying names the upstream accepts. A caller's own
	// tools are never replaced: the response would name a tool the caller cannot
	// dispatch. Missing accepted names are appended, which is what makes the
	// request pass the gate without changing what the client asked for.
	//
	// The body is still in the caller's protocol at this point — the host
	// translates it afterwards — so the tools have to be written in that
	// protocol's shape, or the translation produces an invalid request.
	if raw, added := toolsSatisfying(doc["tools"], toolNamesFor(cfg, model), format); added {
		doc["tools"] = raw
		inj.Applied = append(inj.Applied, "tools")
	}

	if len(inj.Applied) == 0 {
		// Body already carries the contract, but the headers still may not.
		inj.Body = nil
	}

	// 3. canonical identity headers.
	ua := headers["User-Agent"]
	if !userAgentAccepted(ua) {
		inj.Headers["User-Agent"] = cfg.userAgent()
		inj.Applied = append(inj.Applied, "user-agent")
	}
	if !validCanonicalID(headers["x-opencode-session"]) {
		inj.Headers["x-opencode-session"] = canonicalID("ses_", seed)
		inj.Applied = append(inj.Applied, "session")
	}
	if !validCanonicalID(headers["x-opencode-request"]) {
		inj.Headers["x-opencode-request"] = canonicalID("msg_", "")
		inj.Applied = append(inj.Applied, "request-id")
	}
	if strings.TrimSpace(headers["x-opencode-client"]) == "" {
		inj.Headers["x-opencode-client"] = cfg.Client
		inj.Applied = append(inj.Applied, "client")
	}
	if strings.TrimSpace(headers["x-opencode-project"]) == "" {
		inj.Headers["x-opencode-project"] = cfg.Project
		inj.Applied = append(inj.Applied, "project")
	}

	if len(inj.Applied) == 0 {
		return nil
	}
	if len(inj.Applied) > 0 {
		encoded, err := encodeBody(doc, body)
		if err == nil {
			inj.Body = encoded
		}
	}
	return inj
}

// emptyTool renders one placeholder tool in the caller's protocol shape.
func emptyTool(name, format string) json.RawMessage {
	switch normalizeFormat(format) {
	case "claude":
		raw, _ := json.Marshal(map[string]any{
			"name":         name,
			"description":  "",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{}},
		})
		return raw
	case "gemini":
		raw, _ := json.Marshal(map[string]any{
			"functionDeclarations": []any{map[string]any{
				"name":        name,
				"description": "",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			}},
		})
		return raw
	case "openai-response":
		// The Responses surface keeps name/parameters at the top level.
		raw, _ := json.Marshal(map[string]any{
			"type":        "function",
			"name":        name,
			"description": "",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		})
		return raw
	default:
		raw, _ := json.Marshal(map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": "",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		})
		return raw
	}
}

func normalizeFormat(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// toolsSatisfying returns a tools array that carries at least one name the
// upstream accepts, and reports whether it had to change anything. Names the
// caller already declared are preserved in order.
func toolsSatisfying(raw json.RawMessage, accepted []string, format string) (json.RawMessage, bool) {
	present := map[string]bool{}
	var existing []json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &existing); err != nil {
			existing = nil
		}
	}
	for _, item := range existing {
		if name := toolName(item); name != "" {
			present[name] = true
		}
	}
	var missing []string
	for _, n := range accepted {
		if !present[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return raw, false
	}
	out := append([]json.RawMessage(nil), existing...)
	for _, n := range missing {
		out = append(out, emptyTool(n, format))
	}
	merged, err := json.Marshal(out)
	if err != nil {
		return raw, false
	}
	return merged, true
}

// toolName reads a tool name out of any of the supported shapes.
// countChunks counts the data lines in an event stream body.
func countChunks(body []byte) int {
	n := 0
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) && !bytes.Contains(line, []byte("[DONE]")) {
			n++
		}
	}
	return n
}

func snippet(body []byte) string {
	const maxSnippet = 500
	trimmed := strings.TrimSpace(string(body))
	if len(trimmed) > maxSnippet {
		return trimmed[:maxSnippet] + "..."
	}
	return trimmed
}

func toolName(raw json.RawMessage) string {
	var probe struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Name                 string `json:"name"`
		FunctionDeclarations []struct {
			Name string `json:"name"`
		} `json:"functionDeclarations"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	switch {
	case probe.Function.Name != "":
		return probe.Function.Name
	case probe.Name != "":
		return probe.Name
	case len(probe.FunctionDeclarations) > 0:
		return probe.FunctionDeclarations[0].Name
	}
	return ""
}

func encodeTools(names []string) (json.RawMessage, error) {
	tools := make([]map[string]any, 0, len(names))
	for _, n := range names {
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        n,
				"description": "",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		})
	}
	return json.Marshal(tools)
}

// encodeBody re-serialises the document while preserving key order of the
// original, which keeps the diff small and avoids reordering client fields.
func encodeBody(doc map[string]json.RawMessage, original []byte) ([]byte, error) {
	if !bodyNeedsRewrite(doc, original) {
		return original, nil
	}
	var out []byte
	out = append(out, '{')
	first := true
	for _, key := range orderedKeys(original, doc) {
		value, ok := doc[key]
		if !ok {
			continue
		}
		if !first {
			out = append(out, ',')
		}
		first = false
		encoded, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		out = append(out, encoded...)
		out = append(out, ':')
		out = append(out, value...)
	}
	out = append(out, '}')
	return out, nil
}

func bodyNeedsRewrite(doc map[string]json.RawMessage, original []byte) bool {
	var orig map[string]json.RawMessage
	if err := json.Unmarshal(original, &orig); err != nil {
		return true
	}
	if len(orig) != len(doc) {
		return true
	}
	for k, v := range doc {
		if ov, ok := orig[k]; !ok || !jsonEqual(ov, v) {
			return true
		}
	}
	return false
}

func jsonEqual(a, b json.RawMessage) bool {
	return strings.TrimSpace(string(a)) == strings.TrimSpace(string(b))
}

func rawIs(raw json.RawMessage, want string) bool {
	return strings.EqualFold(strings.TrimSpace(string(raw)), want)
}

func toolNamesFor(cfg contractConfig, model string) []string {
	if names := cfg.Tools[model]; len(names) > 0 {
		return names
	}
	if len(cfg.ToolNames) > 0 {
		return cfg.ToolNames
	}
	return defaultClientToolNames
}
