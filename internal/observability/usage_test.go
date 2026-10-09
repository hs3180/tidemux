package observability

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestUsageEnvelopeCacheCountsUnknownsAndTimestamp(t *testing.T) {
	input, output, read, write := int64(7), int64(2), int64(3), int64(1)
	for _, name := range []string{"known", "missing-input", "missing-output", "invalid-cache"} {
		t.Run(name, func(t *testing.T) {
			summary := RequestSummary{TimestampMS: 1791549600047, InputTokens: &input, OutputTokens: &output, CacheReadTokens: &read, CacheWriteTokens: &write}
			switch name {
			case "missing-input":
				summary.InputTokens = nil
			case "missing-output":
				summary.OutputTokens = nil
			case "invalid-cache":
				tooMany := int64(8)
				summary.CacheReadTokens = &tooMany
			}
			record := summary.usageRecord(strings.Repeat("a", 32), "model")
			data, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if record.Timestamp != "2026-10-09T12:40:00.047Z" || record.Message.ID != record.RequestID {
				t.Fatalf("incompatible identity/timestamp: %s", data)
			}
			if name == "known" {
				usage := record.Message.Usage
				if usage == nil || usage.Input != 3 || usage.Output != 2 || *usage.CacheRead != 3 || *usage.CacheWrite != 1 {
					t.Fatalf("cache tokens counted twice: %s", data)
				}
			} else if record.Message.Usage != nil || bytes.Contains(data, []byte(`"usage"`)) {
				t.Fatalf("unknown usage represented as zero: %s", data)
			}
		})
	}
}

func TestUsageLogsConcurrentRequestsAndRestartSessionIdentity(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "logs")
	l := NewUsageLog(directory, "private-caller")
	group := l.SessionID("openai", "private-session", false)
	if group != NewUsageLog(directory, "private-caller").SessionID("openai", "private-session", false) ||
		group == l.SessionID("anthropic", "private-session", false) ||
		group == NewUsageLog(directory, "other-caller").SessionID("openai", "private-session", false) ||
		l.SessionID("openai", "generated-admission", true) != "" {
		t.Fatal("session grouping leaked or lost its scope")
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in, out := int64(3), int64(2)
			RequestSummary{Event: "request_terminal", RequestID: fmt.Sprintf("%032x", i+1), Model: "model",
				TimestampMS: 1791549600047, SessionID: group, InputTokens: &in, OutputTokens: &out, UsageLog: l}.Log(nil)
		}(i)
	}
	wg.Wait()
	path := filepath.Join(directory, "projects", "tidemux", group+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte{'\n'})
	seen := map[string]bool{}
	for _, line := range lines {
		var r usageRecord
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatalf("concurrent writes interleaved: %v", err)
		}
		if r.SessionID != group || seen[r.RequestID] || r.Message.Usage == nil {
			t.Fatalf("invalid or duplicate request: %s", line)
		}
		seen[r.RequestID] = true
	}
	if len(seen) != 32 || bytes.Contains(data, []byte("private")) {
		t.Fatalf("lost requests or leaked identity: %s", data)
	}
	for _, dir := range []string{directory, filepath.Join(directory, "projects"), filepath.Dir(path)} {
		if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("non-private directory %s: %v", dir, err)
		}
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("non-private log: %v", err)
	}
}

func TestUsageLogFailuresDoNotOverwriteOtherFilesOrSuppressRuntimeSummary(t *testing.T) {
	for _, scenario := range []string{"blocked-directory", "symlink", "public-file"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			l := NewUsageLog(filepath.Join(root, "logs"), "caller")
			var logs bytes.Buffer
			logger := JSONLogger(&logs)
			summary := RequestSummary{Event: "request_terminal", RequestID: strings.Repeat("a", 32), Model: "model", TimestampMS: 1791549600047, UsageLog: l}
			if scenario == "blocked-directory" {
				if err := os.WriteFile(l.directory, []byte("private-path-sentinel"), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				summary.Log(nil)
				path := filepath.Join(l.directory, "projects", "tidemux", "ungrouped.jsonl")
				if scenario == "symlink" {
					target := filepath.Join(root, "unrelated")
					if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
					defer func() {
						if data, _ := os.ReadFile(target); string(data) != "keep" {
							t.Error("usage logging overwrote an unrelated file")
						}
					}()
				} else if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			}
			summary.Log(logger)
			summary.Log(logger)
			if strings.Count(logs.String(), `"event":"request_terminal"`) != 2 ||
				strings.Count(logs.String(), `"event":"usage_log_write_failure"`) != 1 ||
				strings.Contains(logs.String(), root) || strings.Contains(logs.String(), "private-path-sentinel") {
				t.Fatalf("log failure suppressed summaries or leaked data: %s", logs.String())
			}
		})
	}
}
