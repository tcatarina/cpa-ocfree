package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"sync"
)

func randomEntropy() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "0"
	}
	return hex.EncodeToString(b[:])
}

func atoi(v string) (int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
		if n > 1<<20 {
			return 0, false
		}
	}
	return n, true
}

// orderedKeys returns the document's own key order first, then any key the
// plugin added, so a rewritten body stays close to what the client sent.
func orderedKeys(original []byte, doc map[string]json.RawMessage) []string {
	seen := make(map[string]bool, len(doc))
	var keys []string
	dec := jsonKeyScanner{data: original}
	for dec.next() {
		if _, ok := doc[dec.key]; ok && !seen[dec.key] {
			seen[dec.key] = true
			keys = append(keys, dec.key)
		}
	}
	rest := make([]string, 0, len(doc))
	for k := range doc {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}

// jsonKeyScanner walks top-level object keys without decoding the whole document.
type jsonKeyScanner struct {
	data []byte
	pos  int
	key  string
}

func (s *jsonKeyScanner) next() bool {
	for s.pos < len(s.data) {
		switch s.data[s.pos] {
		case '{':
			s.pos++
			return s.readKey()
		default:
			s.pos++
		}
	}
	return false
}

func (s *jsonKeyScanner) readKey() bool {
	s.skipSpace()
	if s.pos >= len(s.data) {
		return false
	}
	if s.data[s.pos] == '}' {
		return false
	}
	if s.data[s.pos] != '"' {
		return false
	}
	key, ok := s.readString()
	if !ok {
		return false
	}
	s.skipSpace()
	if s.pos < len(s.data) && s.data[s.pos] == ':' {
		s.pos++
	}
	s.skipValue()
	s.key = key
	return true
}

func (s *jsonKeyScanner) readString() (string, bool) {
	s.pos++ // opening quote
	var out []byte
	for s.pos < len(s.data) {
		c := s.data[s.pos]
		if c == '\\' {
			if s.pos+1 < len(s.data) {
				out = append(out, s.data[s.pos+1])
				s.pos += 2
				continue
			}
			return "", false
		}
		if c == '"' {
			s.pos++
			return string(out), true
		}
		out = append(out, c)
		s.pos++
	}
	return "", false
}

func (s *jsonKeyScanner) skipSpace() {
	for s.pos < len(s.data) && (s.data[s.pos] == ' ' || s.data[s.pos] == '\n' || s.data[s.pos] == '\t' || s.data[s.pos] == '\r') {
		s.pos++
	}
}

func (s *jsonKeyScanner) skipValue() {
	s.skipSpace()
	if s.pos >= len(s.data) {
		return
	}
	switch s.data[s.pos] {
	case '"':
		_, _ = s.readString()
	case '{', '[':
		open := s.data[s.pos]
		closeCh := byte('}')
		if open == '[' {
			closeCh = ']'
		}
		depth := 0
		for s.pos < len(s.data) {
			c := s.data[s.pos]
			switch c {
			case '"':
				_, _ = s.readString()
				continue
			case open:
				depth++
			case closeCh:
				depth--
				if depth == 0 {
					s.pos++
					return
				}
			}
			s.pos++
		}
	default:
		for s.pos < len(s.data) && s.data[s.pos] != ',' && s.data[s.pos] != '}' {
			s.pos++
		}
	}
}

// contractConfig is the plugin-side half of the contract: the model set, the
// tool names, and the client identity. It is editable so an upstream change is a
// config edit rather than a code change.
//
// The json tags matter: the panel reads this back as JSON and a field without
// one is emitted capitalised, which the page does not read.
type contractConfig struct {
	Enabled     bool                `yaml:"enabled" json:"enabled"`
	Models      []string            `yaml:"models" json:"models"`
	ToolNames   []string            `yaml:"tool_names" json:"tool_names"`
	Tools       map[string][]string `yaml:"tools" json:"tools"`
	UserAgent   string              `yaml:"user_agent" json:"user_agent"`
	Client      string              `yaml:"client" json:"client"`
	Project     string              `yaml:"project" json:"project"`
	Upstream    string              `yaml:"upstream" json:"upstream"`
	SessionSalt string              `yaml:"session_salt" json:"session_salt"`
}

func (c contractConfig) userAgent() string {
	if v := strings.TrimSpace(c.UserAgent); v != "" && userAgentAccepted(v) {
		return v
	}
	return defaultUserAgent
}

func (c contractConfig) targets() map[string]bool {
	out := make(map[string]bool, len(c.Models))
	for _, m := range c.Models {
		if v := strings.TrimSpace(m); v != "" {
			out[v] = true
		}
	}
	return out
}

const defaultUserAgent = "opencode/1.18.31"

func defaultConfig() contractConfig {
	return contractConfig{
		Enabled:   true,
		Client:    "desktop",
		Project:   "global",
		Upstream:  "https://opencode.ai/zen/v1",
		ToolNames: append([]string(nil), defaultClientToolNames...),
		Models: []string{
			"big-pickle",
			"longcat-2.5-preview-free",
			"mimo-v2.5-free",
			"mimo-v2.6-flash-free",
			"nemotron-3-ultra-free",
			"nemotron-3.5-lightning-free",
			"space-bunny-free",
		},
	}
}

type configState struct {
	mu  sync.RWMutex
	cfg contractConfig
}

func (s *configState) get() contractConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *configState) set(cfg contractConfig) {
	s.mu.RLock()
	cur := s.cfg
	s.mu.RUnlock()
	if strings.TrimSpace(cfg.UserAgent) == "" {
		cfg.UserAgent = cur.UserAgent
	}
	if !userAgentAccepted(cfg.UserAgent) {
		cfg.UserAgent = defaultUserAgent
	}
	if len(cfg.ToolNames) == 0 {
		cfg.ToolNames = cur.ToolNames
	}
	if len(cfg.Models) == 0 {
		cfg.Models = cur.Models
	}
	if strings.TrimSpace(cfg.Client) == "" {
		cfg.Client = "desktop"
	}
	if strings.TrimSpace(cfg.Project) == "" {
		cfg.Project = "global"
	}
	if strings.TrimSpace(cfg.Upstream) == "" {
		cfg.Upstream = defaultConfig().Upstream
	}
	if strings.TrimSpace(cfg.SessionSalt) == "" {
		cfg.SessionSalt = cur.SessionSalt
	}
	if cfg.Tools == nil {
		cfg.Tools = cur.Tools
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
}

// seedFor derives the per-conversation seed. The same client session always maps
// to the same upstream session, which is what keeps the prompt cache warm.
func (c contractConfig) seedFor(clientSession, model string) string {
	parts := make([]string, 0, 4)
	for _, p := range []string{c.SessionSalt, clientSession, model} {
		if v := strings.TrimSpace(p); v != "" {
			parts = append(parts, v)
		}
	}
	if len(parts) == 0 {
		return randomEntropy()
	}
	return strings.Join(parts, "\x00")
}
