package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// This exercises the built CLI, Keychain lookup boundary, real loopback listener,
// both upstream wire protocols, persisted records, and process shutdown.
func TestServeProcessBothProtocols(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Keychain runtime is macOS-only")
	}
	dir := t.TempDir()
	binary := os.Getenv("TIDEMUX_TEST_BINARY")
	if binary == "" {
		binary = filepath.Join(dir, "tidemux")
		build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", binary, ".")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build %v %s", err, out)
		}
	}
	binDir := filepath.Join(dir, "bin")
	os.Mkdir(binDir, 0700)
	os.WriteFile(filepath.Join(binDir, "security"), []byte("#!/bin/sh\ncase \"$*\" in *test.gateway*) printf 'local-secret';; *) printf 'upstream-secret';; esac\n"), 0700)
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			userDirectory := t.TempDir()
			configDirectory := filepath.Join(userDirectory, "Library", "Application Support", "TideMux")
			if err := os.MkdirAll(configDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if protocol == "anthropic" {
					if r.Header.Get("x-api-key") != "upstream-secret" {
						t.Error("credential")
					}
					io.WriteString(w, `{"id":"a","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":3,"output_tokens":2,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`)
				} else {
					if r.Header.Get("Authorization") != "Bearer upstream-secret" {
						t.Error("credential")
					}
					io.WriteString(w, `{"id":"o","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"prompt_tokens_details":{"cached_tokens":0}}}`)
				}
			}))
			defer upstream.Close()
			config := map[string]any{"listen_addr": "127.0.0.1:0", "protocol": protocol, "base_url": upstream.URL + "/v1", "model": "test-model", "upstream_id": "mock", "anthropic_version": "2023-06-01", "upstream_keychain": map[string]string{"service": "test.provider", "account": "default"}, "access_token_keychain": map[string]string{"service": "test.gateway", "account": "default"}, "max_in_flight": 1, "ledger_path": filepath.Join(dir, protocol+".db"), "prices": map[string]any{"test-model": map[string]any{"currency": "USD", "source": "test-fixture", "version": "1", "input_cache_hit_per_million": 2, "input_cache_miss_per_million": 2, "output_per_million": 4}}}
			data, _ := json.Marshal(config)
			path := filepath.Join(configDirectory, "config.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(binary, "serve", "--config", path)
			cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"), "HOME="+userDirectory)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			ready := make(chan string, 1)
			go func() {
				scan := bufio.NewScanner(stdout)
				if scan.Scan() {
					ready <- scan.Text()
				} else {
					ready <- ""
				}
			}()
			var address string
			select {
			case line := <-ready:
				address = strings.TrimPrefix(line, "TideMux listening on ")
			case <-time.After(10 * time.Second):
				t.Fatal("startup timeout")
			}
			route := "/v1/chat/completions"
			body := `{"messages":[{"role":"user","content":"test"}]}`
			if protocol == "anthropic" {
				route = "/v1/messages"
				body = `{"max_tokens":8,"messages":[{"role":"user","content":"test"}]}`
			}
			req, _ := http.NewRequest("POST", address+route, strings.NewReader(body))
			req.Header.Set("Authorization", "Bearer local-secret")
			client := http.Client{Timeout: 10 * time.Second}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("status %d", resp.StatusCode)
			}
			cmd.Process.Signal(os.Interrupt)
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(12 * time.Second):
				t.Fatal("shutdown timeout")
			}
			query := exec.Command(binary, "billing", "--details", "--json", "--from", "1970-01-01T00:00:00.001Z")
			query.Env = append(os.Environ(), "HOME="+userDirectory)
			out, err := query.CombinedOutput()
			if err != nil {
				t.Fatalf("billing %s %v", out, err)
			}
			var report struct {
				Requests []struct {
					Status        string   `json:"status"`
					EstimatedCost *float64 `json:"estimated_cost"`
					Protocol      string   `json:"protocol"`
				} `json:"requests"`
			}
			if err := json.Unmarshal(out, &report); err != nil {
				t.Fatalf("billing JSON: %s: %v", out, err)
			}
			rows := report.Requests
			if len(rows) != 1 || rows[0].Status != "ok" || rows[0].EstimatedCost == nil || rows[0].Protocol != protocol {
				t.Fatalf("records %s", out)
			}
			if *rows[0].EstimatedCost < 13.999e-6 || *rows[0].EstimatedCost > 14.001e-6 {
				t.Fatal("pricing mismatch")
			}
		})
	}
}
