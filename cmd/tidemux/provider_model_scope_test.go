package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

func TestParseModelScopeSelectionSupportsCatalogAndManualIDs(t *testing.T) {
	models := []string{"catalog-a", "catalog-b", "catalog-c"}
	selected, err := parseModelScopeSelection("#2,manual-model,#1,#2", models, true)
	if err != nil || !reflect.DeepEqual(selected, []string{"catalog-b", "manual-model", "catalog-a"}) {
		t.Fatalf("selection=%v err=%v", selected, err)
	}
	manual, err := parseModelScopeSelection("private-model,another-model", nil, false)
	if err != nil || !reflect.DeepEqual(manual, []string{"private-model", "another-model"}) {
		t.Fatalf("manual selection=%v err=%v", manual, err)
	}
	for _, test := range []struct {
		value string
		known bool
	}{
		{value: "#0", known: true},
		{value: "#1", known: false},
		{value: "model-a,,model-b", known: false},
		{value: "bad\nmodel", known: false},
	} {
		if _, err := parseModelScopeSelection(test.value, models, test.known); err == nil {
			t.Fatalf("invalid selection %q was accepted", test.value)
		}
	}
	if _, err := parseModelScopeSelection("#1", []string{" model-with-space "}, true); err == nil {
		t.Fatal("catalog model ID with surrounding whitespace was accepted")
	}
	if _, err := parseModelScopeSelection("#1", nil, true); err == nil || !strings.Contains(err.Error(), "catalog is empty") {
		t.Fatalf("empty catalog selector error=%v", err)
	}
}

func TestProviderModelsRejectsConflictingScopeModes(t *testing.T) {
	stdout := terminalOutput(t)
	defer stdout.Close()
	stderr := terminalOutput(t)
	defer stderr.Close()
	if err := providerModels([]string{"alpha", "--select", "--all"}, stdout, stderr); err == nil || !strings.Contains(err.Error(), "use at most one") {
		t.Fatalf("conflicting selection modes error=%v", err)
	}
}

func TestPromptModelScopeCanCancelNarrowOrRestoreAll(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		current   []string
		want      []string
		changed   bool
		wantError bool
	}{
		{name: "cancel", input: "\n", current: []string{"model-a"}, want: []string{"model-a"}},
		{name: "select catalog and manual", input: "#2,manual-model\n", want: []string{"model-b", "manual-model"}, changed: true},
		{name: "restore all", input: "all\n", current: []string{"model-a"}, changed: true},
		{name: "read failure", input: "", wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := terminalInput(t, test.input)
			defer in.Close()
			out := terminalOutput(t)
			defer out.Close()
			selected, changed, err := promptModelScope(in, out, []string{"model-a", "model-b"}, true, test.current)
			if test.wantError {
				if err == nil {
					t.Fatal("expected an input error")
				}
				return
			}
			if err != nil || changed != test.changed || !reflect.DeepEqual(selected, test.want) {
				t.Fatalf("selected=%v changed=%t err=%v", selected, changed, err)
			}
			if strings.Contains(readOutput(t, out), "default model") {
				t.Fatal("model scope prompt mentioned a default model")
			}
		})
	}
}

func TestSaveProviderModelScopePersistsAllowlistAndAllModels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	config := gateway.Config{
		ListenAddr:          defaultListenAddr,
		MaxInFlight:         1,
		LedgerPath:          filepath.Join(dir, "ledger.db"),
		AccessTokenKeychain: gateway.KeychainReference{Service: "gateway", Account: "local"},
		Providers: map[string]gateway.Provider{
			"alpha": {
				Protocol: "openai", BaseURL: "https://provider.example/v1", UpstreamID: "alpha",
				UpstreamKeychain: gateway.KeychainReference{Service: "provider", Account: "alpha"},
			},
		},
	}
	if err := writeCommandConfig(path, config, nil); err != nil {
		t.Fatal(err)
	}
	before := mustReadConfig(t, path)
	if err := saveProviderModelScope(path, config, before, "alpha", []string{"model-a", "model-b"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := gateway.LoadConfig(path)
	if err != nil || !reflect.DeepEqual(loaded.Providers["alpha"].SupportedModels, []string{"model-a", "model-b"}) {
		t.Fatalf("saved allowlist=%v err=%v", loaded.Providers["alpha"].SupportedModels, err)
	}
	updated := mustReadConfig(t, path)
	if err := saveProviderModelScope(path, loaded, updated, "alpha", nil); err != nil {
		t.Fatal(err)
	}
	loaded, err = gateway.LoadConfig(path)
	if err != nil || len(loaded.Providers["alpha"].SupportedModels) != 0 {
		t.Fatalf("all-model scope=%v err=%v", loaded.Providers["alpha"].SupportedModels, err)
	}
	if err := saveProviderModelScope(path, loaded, mustReadConfig(t, path), "missing", []string{"model-a"}); err == nil {
		t.Fatal("unknown provider scope was accepted")
	}
}
