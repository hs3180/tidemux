package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/hs3180/tidemux/internal/gateway"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHermesProfileTransportAndOwnership(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, "config.yaml")
	original := []byte("model: existing\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := clientLaunch("hermes", "http://127.0.0.1:8787", "m", state, ""); err == nil {
		t.Fatal("overwrote unmanaged profile")
	}
	data, _ := os.ReadFile(path)
	if string(data) != string(original) {
		t.Fatal("unmanaged profile changed")
	}
	state = t.TempDir()
	for _, model := range []string{"test/first", "test/second"} {
		if _, _, err := clientLaunch("hermes", "http://127.0.0.1:8787", model, state, ""); err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(state, "config.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var config struct {
			Model struct {
				Default string `json:"default"`
			} `json:"model"`
			Providers map[string]struct {
				API       string `json:"api"`
				KeyEnv    string `json:"key_env"`
				Transport string `json:"transport"`
			} `json:"providers"`
		}
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		provider := config.Providers["tidemux-local"]
		if config.Model.Default != model || provider.API != "http://127.0.0.1:8787/v1" || provider.KeyEnv != "TIDEMUX_GATEWAY_TOKEN" || provider.Transport != "chat_completions" {
			t.Fatal("incorrect Hermes route or credential reference")
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatal("profile permissions")
		}
	}
}

func TestClientLaunchConfiguration(t *testing.T) {
	for _, name := range []string{"claude", "kilo", "hermes"} {
		env, args, err := clientLaunch(name, "http://127.0.0.1:8787", "openrouter/stealth/union-alpha", filepath.Join(t.TempDir(), "state"), `{"permission":{"bash":"ask"},"provider":{"existing":{"name":"keep"}}}`)
		if err != nil {
			t.Fatal(err)
		}
		all, _ := json.Marshal(env)
		if strings.Contains(string(all), "provider-secret") {
			t.Fatal("unexpected secret")
		}
		switch name {
		case "claude":
			if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8787" || env["ANTHROPIC_MODEL"] != "openrouter/stealth/union-alpha" || env["CLAUDE_CODE_SIMPLE"] != "1" || len(args) != 0 {
				t.Fatal(env, args)
			}
		case "kilo":
			var config map[string]any
			json.Unmarshal([]byte(env["KILO_CONFIG_CONTENT"]), &config)
			if config["permission"] == nil || config["provider"].(map[string]any)["existing"] == nil || !strings.Contains(env["KILO_CONFIG_CONTENT"], "{env:TIDEMUX_GATEWAY_TOKEN}") {
				t.Fatal("lost existing options or credential reference")
			}
		case "hermes":
			if env["HERMES_HOME"] == "" || strings.Join(args, " ") != "chat --provider tidemux-local --model openrouter/stealth/union-alpha" {
				t.Fatal(env, args)
			}
		}
	}
}
func TestConnectEnvironmentReplacesConflictingAuth(t *testing.T) {
	env := overrideEnvironment([]string{"ANTHROPIC_API_KEY=old", "ANTHROPIC_AUTH_TOKEN=other", "PATH=/bin"}, map[string]string{"ANTHROPIC_API_KEY": "local", "ANTHROPIC_AUTH_TOKEN": ""})
	result := strings.Join(env, "\n")
	if strings.Contains(result, "old") || strings.Contains(result, "other") || !strings.Contains(result, "ANTHROPIC_API_KEY=local") || !strings.Contains(result, "PATH=/bin") {
		t.Fatal("credential override failed")
	}
}

func TestValidateClientProtocol(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		wantErr  bool
	}{
		{name: "claude", protocol: "anthropic"},
		{name: "claude", protocol: "openai"},
		{name: "kilo", protocol: "openai"},
		{name: "kilo", protocol: "anthropic"},
		{name: "kilo-ide", protocol: "anthropic"},
		{name: "hermes", protocol: "anthropic"},
		{name: "claude", protocol: ""},
		{name: "kilo", protocol: "auto"},
		{name: "hermes", protocol: "AUTO"},
		{name: "claude", protocol: "both", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name+"/"+tt.protocol, func(t *testing.T) {
			err := validateClientProtocol(tt.name, tt.protocol)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateClientProtocol(%q, %q) error = %v, wantErr %v", tt.name, tt.protocol, err, tt.wantErr)
			}
		})
	}
}

func TestConnectChecksGatewayBeforeLaunch(t *testing.T) {
	redirected := false
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected = true }))
	defer other.Close()
	for _, mode := range []string{"ok", "401", "redirect", "empty-model-list"} {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer local" {
				t.Error("bad preflight")
			}
			switch mode {
			case "401":
				w.WriteHeader(401)
			case "redirect":
				http.Redirect(w, r, other.URL, 302)
			case "empty-model-list":
				w.Write([]byte(`{"data":[]}`))
			default:
				w.Write([]byte(`{"data":[{"id":"custom-model"}]}`))
			}
		}))
		err := checkClientEndpoint(context.Background(), up.URL, "local")
		up.Close()
		if (err == nil) != (mode == "ok" || mode == "empty-model-list") {
			t.Fatalf("%s %v", mode, err)
		}
	}
	if redirected {
		t.Fatal("credential redirected")
	}
}

func TestQualifiedModelID(t *testing.T) {
	for _, model := range []string{"openrouter/stealth/union-alpha", "deepseek/flash"} {
		if !isQualifiedModelID(model) {
			t.Fatalf("expected qualified model %q", model)
		}
	}
	for _, model := range []string{"", "model", "/model", "provider/", " provider/model", "provider/model "} {
		if isQualifiedModelID(model) {
			t.Fatalf("accepted invalid model %q", model)
		}
	}
}

func TestClientReceivesDeclaredContext(t *testing.T) {
	for _, name := range []string{"kilo", "hermes"} {
		state := t.TempDir()
		env, _, err := clientLaunch(name, "http://127.0.0.1:8787", "test/m", state, "", gateway.ModelCapabilities{ContextTokens: 65536, MaxOutputTokens: 8192})
		if err != nil {
			t.Fatal(err)
		}
		var data []byte
		if name == "kilo" {
			data = []byte(env["KILO_CONFIG_CONTENT"])
		} else {
			data, err = os.ReadFile(filepath.Join(state, "config.yaml"))
			if err != nil {
				t.Fatal(err)
			}
		}
		var config map[string]any
		if json.Unmarshal(data, &config) != nil {
			t.Fatal("invalid client configuration")
		}
		if name == "kilo" {
			limits := config["provider"].(map[string]any)["tidemux-local"].(map[string]any)["models"].(map[string]any)["test/m"].(map[string]any)["limit"].(map[string]any)
			if limits["context"] != float64(65536) || limits["output"] != float64(8192) {
				t.Fatal("Kilo limits lost")
			}
		} else if config["model"].(map[string]any)["context_length"] != float64(65536) {
			t.Fatal("Hermes context lost")
		}
	}
}

func TestIDEProfilePreventsStaleEnvironmentReuse(t *testing.T) {
	state := t.TempDir()
	release, err := lockIDEProfile(state)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := lockIDEProfile(state); err == nil {
		other()
		t.Fatal("allowed concurrent launch")
	}
	release()
	os.MkdirAll(filepath.Join(state, "vscode"), 0700)
	os.WriteFile(filepath.Join(state, "vscode", "code.lock"), []byte(fmt.Sprint(os.Getpid())), 0600)
	if other, err := lockIDEProfile(state); err == nil {
		other()
		t.Fatal("reused active process environment")
	}
}
func TestIDEConfigurationAndWorkspaceIsolation(t *testing.T) {
	state := t.TempDir()
	env, args, err := clientLaunch("kilo-ide", "http://127.0.0.1:8787", "test/m", state, `{"permission":{"bash":"ask"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(args, " ") != "--new-window --wait --user-data-dir "+filepath.Join(state, "vscode") {
		t.Fatal(args)
	}
	if !strings.Contains(env["KILO_CONFIG_CONTENT"], "{env:TIDEMUX_GATEWAY_TOKEN}") || !strings.Contains(env["KILO_CONFIG_CONTENT"], `"bash":"ask"`) {
		t.Fatal("credential reference or permissions lost")
	}
	for _, bad := range [][]string{{"--reuse-window"}, {"a", "b"}, {filepath.Join(state, "missing")}} {
		if _, err := ideWorkspaceArgs(bad, ""); err == nil {
			t.Fatal("accepted invalid workspace", bad)
		}
	}
	got, err := ideWorkspaceArgs([]string{state}, state)
	if err != nil || strings.Join(got, " ") != "--extensions-dir "+state+" "+state {
		t.Fatal(got, err)
	}
}

func TestDirectClientHelp(t *testing.T) {
	for _, client := range []string{"claude", "kilo", "hermes"} {
		t.Run(client, func(t *testing.T) {
			output, err := os.CreateTemp(t.TempDir(), "help")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			if err := run([]string{client, "--help"}, output, output); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(output.Name())
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "Usage of tidemux "+client+":") || !strings.Contains(string(data), "-executable") || !strings.Contains(string(data), "-model") {
				t.Fatalf("missing client help: %s", data)
			}
		})
	}
}

func TestRemovedClientCommands(t *testing.T) {
	for _, command := range []string{"connect", "launch"} {
		if err := run([]string{command, "claude", "--help"}, os.Stdout, os.Stderr); err == nil || err.Error() != usage {
			t.Fatalf("removed command %q should return usage, got %v", command, err)
		}
	}
}
