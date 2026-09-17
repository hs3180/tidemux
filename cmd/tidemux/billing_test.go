package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"io/fs"
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

func TestBillingReadsStoredStatisticsWithoutSideEffects(t *testing.T) {
	var requests atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	configPath, ledgerPath := billingFixture(t, upstream.URL)
	statementDir := filepath.Join(filepath.Dir(ledgerPath), "statements")
	if err := os.Mkdir(statementDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(statementDir, "pending.csv"), []byte("period_start,period_end,currency,amount\n2026-09-14T00:00:00Z,2026-09-15T00:00:00Z,USD,99\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	securityMarker := filepath.Join(t.TempDir(), "keychain-called")
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "security"), []byte("#!/bin/sh\n: > \"$TIDEMUX_TEST_SECURITY_MARKER\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TIDEMUX_TEST_SECURITY_MARKER", securityMarker)
	t.Setenv("PATH", fakeBin)
	before := billingDirectorySnapshot(t, filepath.Dir(ledgerPath))
	output, err := runBillingTest(t, "billing", "--config", configPath)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Reconciliation []map[string]any `json:"reconciliation"`
		StatementSync  map[string]any   `json:"statement_sync"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("billing JSON %q: %v", output, err)
	}
	if len(result.Reconciliation) != 1 || result.StatementSync == nil {
		t.Fatalf("unexpected statistics: %s", output)
	}
	row := result.Reconciliation[0]
	if row["unknown_cost_requests"] != float64(1) || row["supplier_statement"] != float64(1) || row["statement_lines"] != float64(1) {
		t.Fatalf("statistics lost unknown costs or imported pending file: %s", output)
	}
	if got := billingDirectorySnapshot(t, filepath.Dir(ledgerPath)); !reflect.DeepEqual(got, before) {
		t.Fatalf("billing changed existing files: before=%v after=%v", before, got)
	}
	if _, err := os.Stat(securityMarker); !os.IsNotExist(err) {
		t.Fatalf("billing invoked Keychain: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("billing contacted upstream")
	}
}

func TestBillingDownloadPreservesUnknownAndZeroAndExistingFiles(t *testing.T) {
	configPath, ledgerPath := billingFixture(t, "https://example.com/v1")
	destination := filepath.Join(t.TempDir(), "billing.csv")
	before := billingDirectorySnapshot(t, filepath.Dir(ledgerPath))
	if output, err := runBillingTest(t, "billing", "--config", configPath, "--download", destination); err != nil {
		t.Fatalf("download %q: %v", output, err)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("billing CSV is not private: %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil || len(rows) != 5 {
		t.Fatalf("unexpected billing CSV %q: %v", data, err)
	}
	header := map[string]int{}
	for i, name := range rows[0] {
		header[name] = i
	}
	for _, name := range []string{"row_type", "request_id", "local_estimated", "supplier_amount", "input_tokens"} {
		if _, exists := header[name]; !exists {
			t.Fatalf("missing %s column: %q", name, data)
		}
	}
	var requestRows, statementRows int
	for _, row := range rows[1:] {
		if row[header["supplier_amount"]] != "" {
			statementRows++
			if row[header["local_estimated"]] != "" || row[header["row_type"]] == "" {
				t.Fatalf("supplier charge conflated with estimate: %v", row)
			}
			continue
		}
		requestRows++
		switch row[header["request_id"]] {
		case "unknown":
			if row[header["local_estimated"]] != "" || row[header["input_tokens"]] != "" {
				t.Fatalf("unknown values became zero: %v", row)
			}
		case "zero":
			if row[header["local_estimated"]] != "0" || row[header["input_tokens"]] != "0" {
				t.Fatalf("known zero values became unknown: %v", row)
			}
		case "billed":
		default:
			t.Fatalf("unexpected request row: %v", row)
		}
	}
	if requestRows != 3 || statementRows != 1 {
		t.Fatalf("requests and supplier statements were not separate: %q", data)
	}
	for _, existingPath := range []string{destination, ledgerPath, configPath} {
		existing, err := os.ReadFile(existingPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runBillingTest(t, "billing", "--config", configPath, "--download", existingPath); err == nil {
			t.Fatalf("download overwrote %s", existingPath)
		}
		unchanged, err := os.ReadFile(existingPath)
		if err != nil || !bytes.Equal(unchanged, existing) {
			t.Fatalf("existing file changed: %s: %v", existingPath, err)
		}
	}
	if got := billingDirectorySnapshot(t, filepath.Dir(ledgerPath)); !reflect.DeepEqual(got, before) {
		t.Fatalf("download changed ledger files: before=%v after=%v", before, got)
	}
}

func TestBillingRejectsMutationCommandsAndInvalidPeriodsBeforeCreatingOutput(t *testing.T) {
	configPath, _ := billingFixture(t, "https://example.com/v1")
	for _, args := range [][]string{
		{"reconcile", "report"}, {"reconcile", "import"},
		{"billing", "run"}, {"billing", "import"}, {"billing", "retry"},
		{"billing", "--from", "bad"},
		{"billing", "--to", "bad"},
		{"billing", "--from", "1969-12-31T23:59:59Z"},
		{"billing", "--to", "1970-01-01T00:00:00Z"},
		{"billing", "--from", "2026-09-15T00:00:00Z", "--to", "2026-09-14T00:00:00Z"},
		{"billing", "--from", "2026-09-15T00:00:00Z", "--to", "2026-09-15T00:00:00Z"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "billing.csv")
			args := append(append([]string{}, args...), "--config", configPath, "--download", destination)
			if _, err := runBillingTest(t, args...); err == nil {
				t.Fatalf("accepted invalid command: %v", args)
			}
			if _, err := os.Stat(destination); !os.IsNotExist(err) {
				t.Fatalf("invalid arguments created a download: %v", err)
			}
		})
	}
}

func TestBillingDoesNotInitializeMissingLedger(t *testing.T) {
	configPath, ledgerPath := billingFixture(t, "https://example.com/v1")
	if err := os.Remove(ledgerPath); err != nil {
		t.Fatal(err)
	}
	for _, options := range [][]string{nil, {"--download", filepath.Join(t.TempDir(), "billing.csv")}} {
		before := billingDirectorySnapshot(t, filepath.Dir(ledgerPath))
		args := append([]string{"billing", "--config", configPath}, options...)
		if _, err := runBillingTest(t, args...); err == nil {
			t.Fatal("billing accepted a missing ledger")
		}
		if got := billingDirectorySnapshot(t, filepath.Dir(ledgerPath)); !reflect.DeepEqual(got, before) {
			t.Fatalf("billing created ledger files: before=%v after=%v", before, got)
		}
		if len(options) > 0 {
			if _, err := os.Stat(options[1]); !os.IsNotExist(err) {
				t.Fatalf("missing ledger created a download: %v", err)
			}
		}
	}
}

func TestBillingRemovesFailedDownload(t *testing.T) {
	_, ledgerPath := billingFixture(t, "https://example.com/v1")
	store, err := ledger.OpenReadOnly(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "partial.csv")
	if err := downloadBilling(context.Background(), store, destination, 0, 0); err == nil {
		t.Fatal("export from a closed ledger succeeded")
	}
	if _, err := os.Stat(destination); !os.IsNotExist(err) {
		t.Fatalf("failed download left a partial file: %v", err)
	}
}

func billingFixture(t *testing.T, baseURL string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.db")
	store, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	start, _ := time.Parse(time.RFC3339, "2026-09-14T00:00:00Z")
	zeroCost, billedCost := 0.0, 1.0
	zeroTokens := int64(0)
	for _, audit := range []ledger.Audit{
		{ID: "unknown", EstimatedCost: nil},
		{ID: "zero", EstimatedCost: &zeroCost, InputTokens: &zeroTokens, OutputTokens: &zeroTokens},
		{ID: "billed", EstimatedCost: &billedCost},
	} {
		audit.TimestampMS = start.UnixMilli()
		audit.Protocol, audit.Upstream, audit.Model, audit.Status, audit.Currency = "openai", "test", "model", "ok", "USD"
		if err := store.AppendAudit(audit); err != nil {
			store.Close()
			t.Fatal(err)
		}
	}
	statement := "period_start,period_end,currency,amount,request_id,model\n2026-09-14T00:00:00Z,2026-09-15T00:00:00Z,USD,1,billed,model\n"
	if _, err := store.ImportStatementCSV(context.Background(), strings.NewReader(statement), start.Add(24*time.Hour).UnixMilli()); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{
		"protocol": "openai", "base_url": baseURL, "model": "model", "upstream_id": "test",
		"listen_addr": "127.0.0.1:8787", "max_in_flight": 1, "ledger_path": ledgerPath,
		"upstream_keychain":     map[string]string{"service": "test.provider", "account": "default"},
		"access_token_keychain": map[string]string{"service": "test.gateway", "account": "default"},
		"reconciliation":        map[string]any{"statement_dir": filepath.Join(dir, "statements")},
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, ledgerPath
}

func runBillingTest(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()
	output, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	stderr, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	runErr := run(args, output, stderr)
	data, err := os.ReadFile(output.Name())
	if err != nil {
		t.Fatal(err)
	}
	return data, runErr
}

type billingFileSnapshot struct {
	Mode    fs.FileMode
	ModTime time.Time
	Digest  [32]byte
}

func billingDirectorySnapshot(t *testing.T, directory string) map[string]billingFileSnapshot {
	t.Helper()
	result := map[string]billingFileSnapshot{}
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// SQLite readers may create or update WAL coordination files even in
		// mode=ro. The database, configuration and statement files must remain
		// unchanged; immutable=1 would risk hiding a running gateway's writes.
		if path == filepath.Join(directory, "ledger.db-wal") || path == filepath.Join(directory, "ledger.db-shm") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		item := billingFileSnapshot{Mode: info.Mode(), ModTime: info.ModTime()}
		if entry.IsDir() {
			item.ModTime = time.Time{}
		} else {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			item.Digest = sha256.Sum256(data)
		}
		result[path] = item
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
