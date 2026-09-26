package main

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/hs3180/tidemux/internal/gateway"
)

func TestPromptProviderUpdateIsLinearAndEditsOnlyEnteredFields(t *testing.T) {
	current := gateway.Provider{BaseURL: "https://old.example/v1", Protocol: "openai", SupportedModels: []string{"model-a"}}
	tests := []struct {
		name       string
		input      string
		want       providerUpdateChanges
		wantError  bool
		wantPrompt string
	}{
		{
			name:       "endpoint auto-detects protocol and clears scope by default",
			input:      "https://new.example/v1\n\n\n",
			want:       providerUpdateChanges{Endpoint: "https://new.example/v1", Changed: true},
			wantPrompt: "Enter auto-detects",
		},
		{
			name:  "protocol override",
			input: "\nanthropic\n\n",
			want:  providerUpdateChanges{Protocol: "anthropic", Changed: true},
		},
		{
			name:  "model allowlist",
			input: "\n\nmodel-b, model-c\n",
			want:  providerUpdateChanges{Model: "model-b,model-c", ModelRequested: true, Changed: true},
		},
		{
			name:  "restore all models",
			input: "\n\nall\n",
			want:  providerUpdateChanges{Model: "all", ModelRequested: true, Changed: true},
		},
		{
			name:  "blank form leaves provider unchanged",
			input: "\n\n\n",
			want:  providerUpdateChanges{},
		},
		{
			name:      "reject unsafe endpoint",
			input:     "http://provider.example/v1\n\n\n",
			wantError: true,
		},
		{
			name:      "reject invalid protocol",
			input:     "\ninvalid\n\n",
			wantError: true,
		},
		{
			name:      "reject malformed model list",
			input:     "\n\nmodel-a,,model-b\n",
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			in := terminalInput(t, test.input)
			defer in.Close()
			out := terminalOutput(t)
			defer out.Close()
			got, err := promptProviderUpdate(in, out, current)
			if test.wantError {
				if err == nil {
					t.Fatalf("input %q was accepted", test.input)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("changes=%+v err=%v", got, err)
			}
			if test.wantPrompt != "" && !strings.Contains(readOutput(t, out), test.wantPrompt) {
				t.Fatalf("output omitted %q: %s", test.wantPrompt, readOutput(t, out))
			}
		})
	}
}

func TestProviderUpdateScopeAcceptsAllAndRejectsEmptyList(t *testing.T) {
	if !isAllProviderModels(" ALL ") || !isAllProviderModels("*") || isAllProviderModels("all,model-a") {
		t.Fatal("unexpected all-model scope parsing")
	}
	if models, err := parseProviderModelList("model-a,model-b"); err != nil || !reflect.DeepEqual(models, []string{"model-a", "model-b"}) {
		t.Fatalf("model list=%v err=%v", models, err)
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

func TestProviderUpdatePromptDoesNotDependOnProviderNameOrDefaults(t *testing.T) {
	current := gateway.Provider{BaseURL: "https://provider.example/v1", Protocol: "auto"}
	in := terminalInput(t, "\n\n\n")
	defer in.Close()
	out := terminalOutput(t)
	defer out.Close()
	if _, err := promptProviderUpdate(in, out, current); err != nil {
		t.Fatal(err)
	}
	if got := readOutput(t, out); strings.Contains(got, "default provider") || strings.Contains(got, "default model") {
		t.Fatalf("prompt reintroduced a default provider/model: %s", got)
	}
}
