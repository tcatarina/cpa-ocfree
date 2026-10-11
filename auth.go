package main

import (
	"encoding/json"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The executor-owned models live under the plugin's own provider, which owns
// no credentials of its own — the free tier is keyless. But the host selects
// an auth before it runs any executor, and with zero auths every request dies
// with auth_not_found before the executor is reached.
//
// So the plugin mints one keyless auth from a stub file. The file carries no
// secret; it exists because auth records come from auth material, and this is
// the material:
//
//	auths/ocfree.json: {"type": "ocfree"}
//
// Native parsing ignores the unknown type and the host offers the file to
// plugin parsers, which is where this answers.
func handleAuthParse(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return errorEnvelope("bad_request", err.Error()), nil
	}
	if strings.ToLower(strings.TrimSpace(req.Provider)) != executorProviderName {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	label := "OpenCode Free (keyless)"
	var doc struct {
		Label string `json:"label"`
	}
	if err := json.Unmarshal(req.RawJSON, &doc); err == nil && strings.TrimSpace(doc.Label) != "" {
		label = strings.TrimSpace(doc.Label)
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth: pluginapi.AuthData{
			Provider: executorProviderName,
			ID:       "ocfree-keyless",
			Label:    label,
		},
	})
}
