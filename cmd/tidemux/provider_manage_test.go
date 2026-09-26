package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

func TestProviderManagerListsSortedRedactedGroups(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	config := providerManageTestConfig(dir)
	if err := writeCommandConfig(configPath, config, nil); err != nil {
		t.Fatal(err)
	}
	tty := terminalInput(t, "q\n")
	defer tty.Close()
	stdout := terminalOutput(t)
	defer stdout.Close()
	stderr := terminalOutput(t)
	defer stderr.Close()
	if err := runProviderManage(tty, stdout, stderr, configPath); err != nil {
		t.Fatal(err)
	}
	text := readOutput(t, stdout)
	if strings.Index(text, "alpha") < 0 || strings.Index(text, "zeta") < 0 || strings.Index(text, "alpha") > strings.Index(text, "zeta") {
		t.Fatalf("provider groups were not listed in stable sorted order: %s", text)
	}
	for _, want := range []string{"openai", "anthropic", "2 keys", "all models", "1 allowed models"} {
		if !strings.Contains(text, want) {
			t.Fatalf("manager output missing %q: %s", want, text)
		}
	}
	for _, secretDetail := range []string{"private-service", "private-account", "provider-secret", "gateway-secret"} {
		if strings.Contains(text, secretDetail) {
			t.Fatalf("manager disclosed credential detail %q: %s", secretDetail, text)
		}
	}
}

func TestProviderManagerGroupMenuReturnsToList(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := writeCommandConfig(configPath, providerManageTestConfig(dir), nil); err != nil {
		t.Fatal(err)
	}
	tty := terminalInput(t, "0\n")
	defer tty.Close()
	stdout := terminalOutput(t)
	defer stdout.Close()
	stderr := terminalOutput(t)
	defer stderr.Close()
	if err := runProviderGroupManage(tty, stdout, stderr, configPath, "alpha"); err != nil {
		t.Fatal(err)
	}
	text := readOutput(t, stdout)
	if !strings.Contains(text, "Provider alpha (openai") || !strings.Contains(text, "6. Edit model scope") || !strings.Contains(text, "7. Validate") {
		t.Fatalf("unexpected provider action menu: %s", text)
	}
}

func TestPromptProviderUpdateBuildsOnlyChangedFields(t *testing.T) {
	current := gateway.Provider{BaseURL: "https://old.example/v1", Protocol: "openai", SupportedModels: []string{"model-a"}}
	tests := []struct {
		name       string
		input      string
		wantArgs   []string
		wantChange bool
		wantError  bool
	}{
		{
			name:       "endpoint auto-detects protocol",
			input:      "https://new.example/v1\n\n",
			wantArgs:   []string{"alpha", "--endpoint", "https://new.example/v1", "--config", "/tmp/config.json"},
			wantChange: true,
		},
		{
			name:       "protocol override",
			input:      "\n Anthropic \n",
			wantArgs:   []string{"alpha", "--protocol", "anthropic", "--config", "/tmp/config.json"},
			wantChange: true,
		},
		{
			name:     "no changes",
			input:    "\n\n",
			wantArgs: []string{"alpha", "--config", "/tmp/config.json"},
		},
		{
			name:      "reject unsafe endpoint",
			input:     "http://provider.example/v1\n\n",
			wantError: true,
		},
		{
			name:      "reject invalid protocol",
			input:     "\ninvalid\n",
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := terminalInput(t, test.input)
			defer in.Close()
			out := terminalOutput(t)
			defer out.Close()
			args, changed, err := promptProviderUpdateArgs(in, out, "alpha", "/tmp/config.json", current)
			if test.wantError {
				if err == nil {
					t.Fatalf("input %q was accepted", test.input)
				}
				return
			}
			if err != nil || changed != test.wantChange || strings.Join(args, "\x00") != strings.Join(test.wantArgs, "\x00") {
				t.Fatalf("args=%q changed=%t err=%v", args, changed, err)
			}
			if strings.Contains(readOutput(t, out), "default model") {
				t.Fatal("provider edit prompt reintroduced a default model")
			}
		})
	}
}

func providerManageTestConfig(dir string) gateway.Config {
	return gateway.Config{
		ListenAddr:          defaultListenAddr,
		MaxInFlight:         1,
		LedgerPath:          filepath.Join(dir, "ledger.db"),
		AccessTokenKeychain: gateway.KeychainReference{Service: "private-gateway-service", Account: "private-gateway-account"},
		Providers: map[string]gateway.Provider{
			"zeta": {
				Protocol: "anthropic", BaseURL: "https://anthropic.example/v1", UpstreamID: "zeta",
				UpstreamKeychains: []gateway.KeychainReference{
					{Service: "private-service", Account: "private-account-one"},
					{Service: "private-service", Account: "private-account-two"},
				},
			},
			"alpha": {
				Protocol: "openai", BaseURL: "https://openai.example/v1", UpstreamID: "alpha",
				UpstreamKeychain: gateway.KeychainReference{Service: "private-service", Account: "private-account-three"},
				SupportedModels:  []string{"model-a"},
			},
		},
	}
}

func terminalInput(t *testing.T, value string) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "input-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(value); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		file.Close()
		t.Fatal(err)
	}
	return file
}

func terminalOutput(t *testing.T) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "output-*")
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func readOutput(t *testing.T, file *os.File) string {
	t.Helper()
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
