package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
)

func TestLegacyLedgerKeepsLatest100JSONFromHistoricalDatabase(t *testing.T) {
	for _, diagnostics := range []bool{false, true} {
		t.Run(fmt.Sprintf("diagnostics=%t", diagnostics), func(t *testing.T) {
			configPath, _, audits, rejections := historicalDiagnosticFixture(t, "https://example.com/v1", 105)
			args := []string{"ledger", "--config", configPath}
			var expected any = audits[:100]
			if diagnostics {
				args = append(args, "--diagnostics")
				expected = rejections[:100]
			}
			output, stderr, err := runDiagnosticTest(t, args...)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(expected)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(output, append(encoded, '\n')) || len(stderr) != 0 {
				t.Fatalf("legacy output changed: stdout=%s stderr=%s", output, stderr)
			}
		})
	}
}

func TestDoctorDiagnosticsReadsHistoricalDatabaseWithoutSideEffects(t *testing.T) {
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	configPath, ledgerPath, _, rejections := historicalDiagnosticFixture(t, upstream.URL, 105)
	securityMarker := filepath.Join(t.TempDir(), "keychain-called")
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "security"), []byte("#!/bin/sh\n: > \"$TIDEMUX_TEST_SECURITY_MARKER\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TIDEMUX_TEST_SECURITY_MARKER", securityMarker)
	t.Setenv("PATH", fakeBin)
	before := billingDirectorySnapshot(t, filepath.Dir(ledgerPath))
	output, stderr, err := runDiagnosticTest(t, "doctor", "--diagnostics", "--config", configPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Local diagnostics (latest 100)", "TIME (UTC)", "ENDPOINT", "invalid_request", rejections[0].ID, "2024-01-01T00:00:00.104Z"} {
		if !strings.Contains(string(output), text) {
			t.Fatalf("readable diagnostics omitted %q: %s", text, output)
		}
	}
	if lines := bytes.Count(output, []byte("\n")); lines != 102 || len(stderr) != 0 {
		t.Fatalf("expected header and 100 rows: lines=%d stderr=%s", lines, stderr)
	}
	output, stderr, err = runDiagnosticTest(t, "doctor", "--diagnostics", "--json", "--config", configPath)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := json.Marshal(rejections[:100])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output, append(expected, '\n')) || len(stderr) != 0 {
		t.Fatalf("unexpected diagnostic JSON: stdout=%s stderr=%s", output, stderr)
	}
	if got := billingDirectorySnapshot(t, filepath.Dir(ledgerPath)); !reflect.DeepEqual(got, before) {
		t.Fatalf("doctor diagnostics changed files: before=%v after=%v", before, got)
	}
	if _, err := os.Stat(securityMarker); !os.IsNotExist(err) {
		t.Fatalf("doctor diagnostics invoked Keychain: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("doctor diagnostics contacted upstream")
	}
	// This fixture has no reconciliation schema. Diagnostics must leave it that
	// way, while the billing-specific reader still requires initialization.
	if store, err := ledger.OpenReadOnly(ledgerPath); err == nil {
		store.Close()
		t.Fatal("reading diagnostics initialized reconciliation")
	}
}

func TestDiagnosticEmptyMissingAndInvalidModes(t *testing.T) {
	configPath, ledgerPath, _, _ := historicalDiagnosticFixture(t, "https://example.com/v1", 0)
	for _, args := range [][]string{{"ledger"}, {"ledger", "--diagnostics"}, {"doctor", "--diagnostics", "--json"}} {
		output, stderr, err := runDiagnosticTest(t, append(args, "--config", configPath)...)
		if err != nil || string(output) != "[]\n" || len(stderr) != 0 {
			t.Fatalf("empty %v: stdout=%q stderr=%q err=%v", args, output, stderr, err)
		}
	}
	output, _, err := runDiagnosticTest(t, "doctor", "--diagnostics", "--config", configPath)
	if err != nil || string(output) != "No local diagnostics recorded.\n" {
		t.Fatalf("empty readable diagnostics: %q: %v", output, err)
	}
	if _, _, err := runDiagnosticTest(t, "doctor", "--json", "--config", configPath); err == nil || !strings.Contains(err.Error(), "requires --diagnostics") {
		t.Fatalf("doctor accepted --json without --diagnostics: %v", err)
	}
	if err := os.Remove(ledgerPath); err != nil {
		t.Fatal(err)
	}
	before := billingDirectorySnapshot(t, filepath.Dir(ledgerPath))
	if _, _, err := runDiagnosticTest(t, "doctor", "--diagnostics", "--config", configPath); err == nil {
		t.Fatal("doctor diagnostics accepted a missing ledger")
	}
	if got := billingDirectorySnapshot(t, filepath.Dir(ledgerPath)); !reflect.DeepEqual(got, before) {
		t.Fatalf("doctor diagnostics created ledger files: before=%v after=%v", before, got)
	}
}

func TestUsagePromotesBillingAndDoctor(t *testing.T) {
	if strings.Contains(usage, "ledger") || !strings.Contains(usage, "billing") || !strings.Contains(usage, "--details") || !strings.Contains(usage, "--diagnostics [--json]") {
		t.Fatalf("unexpected primary command help: %s", usage)
	}
}

// Create the audit and diagnostic schema used before automatic reconciliation,
// with old timestamps to verify that legacy queries never acquire date filters.
func historicalDiagnosticFixture(t *testing.T, baseURL string, count int) (string, string, []ledger.Audit, []ledger.Diagnostic) {
	t.Helper()
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.db")
	db, err := sql.Open("sqlite", ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE request_audit (
 id TEXT PRIMARY KEY, timestamp_ms INTEGER NOT NULL, protocol TEXT NOT NULL,
 upstream TEXT NOT NULL, model TEXT NOT NULL, status TEXT NOT NULL,
 input_tokens INTEGER, output_tokens INTEGER, estimated_cost REAL, currency TEXT,
 record_json TEXT NOT NULL);
 CREATE TABLE local_diagnostics (
 id TEXT PRIMARY KEY, timestamp_ms INTEGER NOT NULL, protocol TEXT NOT NULL,
 method TEXT NOT NULL, endpoint TEXT NOT NULL, status INTEGER NOT NULL, error_code TEXT NOT NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	audits := []ledger.Audit{}
	diagnostics := []ledger.Diagnostic{}
	start := time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	for i := count - 1; i >= 0; i-- {
		audit := ledger.Audit{ID: fmt.Sprintf("historical-%03d", i), TimestampMS: start + int64(i), Protocol: "openai", Upstream: "test", Model: "old-model", Status: "ok", Currency: "USD", PriceSnapshot: json.RawMessage(`{"version":"historical"}`), Events: []string{}}
		record, err := json.Marshal(audit)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO request_audit VALUES (?,?,?,?,?,?,?,?,?,?,?)`, audit.ID, audit.TimestampMS, audit.Protocol, audit.Upstream, audit.Model, audit.Status, nil, nil, nil, audit.Currency, string(record)); err != nil {
			t.Fatal(err)
		}
		diagnostic := ledger.Diagnostic{ID: fmt.Sprintf("%032x", i), TimestampMS: start + int64(i), Protocol: "openai", Method: "POST", Endpoint: "chat_completions", Status: 400, ErrorCode: "invalid_request"}
		if _, err := db.Exec(`INSERT INTO local_diagnostics VALUES (?,?,?,?,?,?,?)`, diagnostic.ID, diagnostic.TimestampMS, diagnostic.Protocol, diagnostic.Method, diagnostic.Endpoint, diagnostic.Status, diagnostic.ErrorCode); err != nil {
			t.Fatal(err)
		}
		audits = append(audits, audit)
		diagnostics = append(diagnostics, diagnostic)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{
		"protocol": "openai", "base_url": baseURL, "model": "model", "upstream_id": "test",
		"listen_addr": "127.0.0.1:8787", "max_in_flight": 1, "ledger_path": ledgerPath,
		"upstream_keychain":     map[string]string{"service": "test.provider", "account": "default"},
		"access_token_keychain": map[string]string{"service": "test.gateway", "account": "default"},
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, ledgerPath, audits, diagnostics
}

func runDiagnosticTest(t *testing.T, args ...string) ([]byte, []byte, error) {
	t.Helper()
	stdout, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.CreateTemp(t.TempDir(), "stderr-*")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	runErr := run(args, stdout, stderr)
	output, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	errorOutput, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	return output, errorOutput, runErr
}
