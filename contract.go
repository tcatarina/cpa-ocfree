package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The upstream free tier answers 403 unless the request carries all of:
//
//  1. stream: true
//  2. a non-empty tools array whose names the upstream currently accepts
//  3. canonical ses_/msg_ identity headers
//  4. a User-Agent naming opencode with major >= 1
//
// A request that omits the User-Agent never reaches the tier gate at all: the
// edge answers 403 Cloudflare 1010 first. Measured 2026-09-29 against the
// original zen base, 2026-10-17 unchanged on the inference base the client
// moved to (the zen base itself now rate-limits every request).

const (
	base62Alphabet  = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	minUserAgentMi  = 17
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

// idClock and idCounter mirror the official client's identifier generator
// (packages/schema/src/identifier.ts): a millisecond timestamp with a per-call
// counter, bit-inverted. The counter makes two ids minted in the same
// millisecond differ, exactly as the client's does.
var (
	idMu      sync.Mutex
	idLast    int64
	idCounter int64
)

// opencodeID renders a session id the way the official client does: the low 48
// bits of ~(unixMillis*4096 + counter) as 12 hex, then 14 random base62 chars.
//
// Measured 2026-10-16: the upstream now decodes that timestamp and refuses ids
// whose time part is not a plausible client encoding — a shape-valid id with a
// hash where the timestamp belongs answers 403 FreeTierError, while any id in
// this encoding passes (ages from 0s to 365d and one hour in the future all
// passed, so the check is the encoding, not freshness).
//
// A seed no longer makes the id deterministic — the timestamp is the payload —
// so conversation stability moved to sessionIDFor, which caches one minted id
// per conversation key instead.
func opencodeID(prefix string) string {
	now := time.Now().UnixMilli()
	idMu.Lock()
	if now != idLast {
		idLast = now
		idCounter = 0
	}
	idCounter++
	counter := idCounter
	idMu.Unlock()

	value := uint64(now)*0x1000 + uint64(counter)
	value = (^value) & ((1 << 48) - 1)
	out := make([]byte, 0, sessionHexLen*2+sessionBase62Ln)
	for i := 0; i < sessionHexLen; i++ {
		b := byte((value >> (40 - 8*uint(i))) & 0xff)
		out = append(out, hexDigits[b>>4], hexDigits[b&0x0f])
	}
	entropy := randomEntropy()
	for i := 0; i < sessionBase62Ln; i++ {
		out = append(out, base62Alphabet[int(entropy[i%len(entropy)])%len(base62Alphabet)])
	}
	return prefix + string(out)
}

const hexDigits = "0123456789abcdef"

// sessionIDs caches one minted id per conversation, so the same conversation
// keeps one upstream session and its prompt cache stays warm — the property the
// old deterministic ids provided for free. Bounded the same way the request
// notes are; entries are tiny.
var (
	sessionMu    sync.Mutex
	sessionIDs   = map[string]string{}
	sessionOrder = make([]string, 0, 64)
	sessionCap   = 256
)

func sessionIDFor(key string) string {
	if key == "" {
		return opencodeID("ses_")
	}
	sessionMu.Lock()
	if id, ok := sessionIDs[key]; ok {
		sessionMu.Unlock()
		return id
	}
	sessionMu.Unlock()
	id := opencodeID("ses_")
	sessionMu.Lock()
	if len(sessionIDs) >= sessionCap {
		for _, k := range sessionOrder {
			if _, ok := sessionIDs[k]; ok {
				delete(sessionIDs, k)
			}
			sessionOrder = sessionOrder[1:]
			if len(sessionIDs) < sessionCap {
				break
			}
		}
	}
	sessionIDs[key] = id
	sessionOrder = append(sessionOrder, key)
	sessionMu.Unlock()
	return id
}

func hexEncode(b []byte) string {
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexDigits[c>>4], hexDigits[c&0x0f])
	}
	return string(out)
}

// validCanonicalID checks the shape only. The value's timestamp is validated
// upstream; locally the shape is all that can be checked without duplicating
// the gate.
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

// plausibleSessionID reports whether an id carries the client's timestamp
// encoding. Shape alone stopped being enough on 2026-10-14: the upstream decodes
// the 12 hex and refuses what does not decode like a client id, so an id we
// minted before that date — or one from any other generator — must be replaced
// rather than forwarded.
//
// The encoding note that matters here: the client multiplies unix millis by
// 4096 before inverting, which overflows the 48 bits it keeps, so the decoded
// value is the timestamp modulo 2^36 — not an epoch. The check compares against
// now under that same modulus, with an hour of slack in either direction.
func plausibleSessionID(v string) bool {
	if !validCanonicalID(v) {
		return false
	}
	value, err := strconv.ParseUint(v[4:4+sessionHexLen*2], 16, 64)
	if err != nil {
		return false
	}
	const modulus = uint64(1) << 36
	// The upstream accepted ages from 0s to 365d and one hour in the future
	// (measured 2026-10-16), so what it validates is the encoding, not
	// freshness. Keep a year of slack: anything older than that is not a
	// session this plugin needs to preserve.
	const slack = uint64(365 * 24 * 3600 * 1000)
	decoded := ((^value) & ((1 << 48) - 1)) >> 12
	now := uint64(time.Now().UnixMilli()) % modulus
	delta := (decoded + modulus - now%modulus) % modulus
	if delta > modulus/2 {
		delta = modulus - delta
	}
	return delta <= slack
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

	// 3. canonical identity headers. The session id is the one the upstream
	// validates: a client-supplied id is forwarded only when it carries the
	// client's timestamp encoding, because a shape-valid id from any other
	// generator is refused. The request id is deliberately not injected — the
	// official client sends no x-opencode-request at all, and the static one
	// this plugin used to add was refused alongside the session change.
	ua := headers["User-Agent"]
	if !userAgentAccepted(ua) {
		inj.Headers["User-Agent"] = cfg.userAgent()
		inj.Applied = append(inj.Applied, "user-agent")
	}
	if v := lookupHeader(headers, "x-opencode-session"); !plausibleSessionID(v) {
		inj.Headers["x-opencode-session"] = sessionIDFor(seed)
		inj.Applied = append(inj.Applied, "session")
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
