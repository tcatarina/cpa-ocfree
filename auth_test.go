package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestAuthParseIgnoresForeignProviders(t *testing.T) {
	raw, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: "codex"})
	out, err := handleAuthParse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Handled bool `json:"Handled"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("parse failed: %s", out)
	}
	if env.Result.Handled {
		t.Error("a foreign provider file was claimed")
	}
}

func TestAuthParseMintsKeylessAuth(t *testing.T) {
	raw, _ := json.Marshal(pluginapi.AuthParseRequest{
		Provider: "ocfree",
		FileName: "ocfree.json",
		RawJSON:  []byte(`{"type":"ocfree"}`),
	})
	out, err := handleAuthParse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool `json:"ok"`
		Result struct {
			Handled bool `json:"Handled"`
			Auth    struct {
				Provider string `json:"Provider"`
				ID       string `json:"ID"`
			} `json:"Auth"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("parse failed: %s", out)
	}
	if !env.Result.Handled {
		t.Fatal("the ocfree stub file was not claimed")
	}
	if env.Result.Auth.Provider != executorProviderName || strings.TrimSpace(env.Result.Auth.ID) == "" {
		t.Errorf("auth = %+v, want provider ocfree with a stable id", env.Result.Auth)
	}
}
