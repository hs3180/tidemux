package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDoctorSmokeFromCleanDirectory(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the MVP Keychain implementation is macOS-only")
	}
	temp := t.TempDir()
	binary := filepath.Join(temp, "tidemux")
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	configPath := filepath.Join(temp, "config.json")
	config := map[string]any{
		"listen_addr":           "127.0.0.1:8787",
		"deepseek_keychain":     map[string]string{"service": "test.deepseek", "account": "default"},
		"access_token_keychain": map[string]string{"service": "test.gateway", "account": "default"},
		"max_in_flight":         1, "ledger_path": filepath.Join(temp, "ledger.db"),
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(temp, "bin")
	if err := os.Mkdir(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	security := filepath.Join(fakeBin, "security")
	if err := os.WriteFile(security, []byte("#!/bin/sh\nprintf 'test-secret\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "doctor", "--config", configPath)
	command.Dir = temp
	command.Env = append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH"))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "doctor: configuration, Keychain references, and ledger directory are ready") {
		t.Fatalf("unexpected doctor output: %s", output)
	}
}
