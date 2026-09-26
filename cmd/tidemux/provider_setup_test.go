package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParseProviderModelList(t *testing.T) {
	got, err := parseProviderModelList(" model-a,model-b ")
	if err != nil || !reflect.DeepEqual(got, []string{"model-a", "model-b"}) {
		t.Fatalf("parsed models=%v err=%v", got, err)
	}
	for _, value := range []string{"model-a,,model-b", "model-a,model-a"} {
		if _, err := parseProviderModelList(value); err == nil {
			t.Errorf("invalid model list %q was accepted", value)
		}
	}
}

func TestChooseProviderModelsSetsInteractiveAllowlist(t *testing.T) {
	t.Run("selected models become allowlist", func(t *testing.T) {
		input := providerSetupInput(t, "2,model-c\n")
		defer input.Close()
		allowed, err := chooseProviderModels(input, os.Stdout, []string{"model-a", "model-b", "model-c"}, true)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(allowed, []string{"model-b", "model-c"}) {
			t.Fatalf("allowlist = %v", allowed)
		}
	})

	t.Run("blank scope allows all discovered models", func(t *testing.T) {
		input := providerSetupInput(t, "\n")
		defer input.Close()
		allowed, err := chooseProviderModels(input, os.Stdout, []string{"model-a", "model-b"}, true)
		if err != nil {
			t.Fatal(err)
		}
		if allowed != nil {
			t.Fatalf("selection should allow all models, got %v", allowed)
		}
	})

	t.Run("manual IDs become allowlist when discovery is unavailable", func(t *testing.T) {
		input := providerSetupInput(t, "model-a,model-b\n")
		defer input.Close()
		allowed, err := chooseProviderModels(input, os.Stdout, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(allowed, []string{"model-a", "model-b"}) {
			t.Fatalf("allowlist = %v", allowed)
		}
	})

	t.Run("all with unknown catalog leaves allowlist unset", func(t *testing.T) {
		input := providerSetupInput(t, "*\n")
		defer input.Close()
		allowed, err := chooseProviderModels(input, os.Stdout, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if allowed != nil {
			t.Fatalf("selection should allow all models, got %v", allowed)
		}
	})
}

func providerSetupInput(t *testing.T, value string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return input
}
