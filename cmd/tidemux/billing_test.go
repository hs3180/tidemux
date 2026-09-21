package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"fmt"
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
	_, ledgerPath := billingFixture(t, upstream.URL)
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
	output, err := runBillingTest(t, "billing", "--json", "--from", "2026-09-01T00:00:00Z")
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
	if output, err := runBillingTest(t, "billing", "--from", "2026-09-01T00:00:00Z", "--download", destination); err != nil {
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
		if _, err := runBillingTest(t, "billing", "--download", existingPath); err == nil {
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
	billingFixture(t, "https://example.com/v1")
	for _, args := range [][]string{
		{"reconcile", "report"}, {"reconcile", "import"},
		{"billing", "run"}, {"billing", "import"}, {"billing", "retry"},
		{"billing", "config"}, {"billing", "--config", defaultConfigPath()},
		{"billing", "--from", "bad"},
		{"billing", "--to", "bad"},
		{"billing", "--from", "1969-12-31T23:59:59Z"},
		{"billing", "--to", "1970-01-01T00:00:00Z"},
		{"billing", "--from", "2026-09-15T00:00:00Z", "--to", "2026-09-14T00:00:00Z"},
		{"billing", "--from", "2026-09-15T00:00:00Z", "--to", "2026-09-15T00:00:00Z"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "billing.csv")
			args := append(append([]string{}, args...), "--download", destination)
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
	_, ledgerPath := billingFixture(t, "https://example.com/v1")
	if err := os.Remove(ledgerPath); err != nil {
		t.Fatal(err)
	}
	for _, options := range [][]string{nil, {"--download", filepath.Join(t.TempDir(), "billing.csv")}} {
		before := billingDirectorySnapshot(t, filepath.Dir(ledgerPath))
		args := append([]string{"billing"}, options...)
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
	start, _ := time.Parse(time.RFC3339, "2026-09-14T00:00:00Z")
	return billingFixtureAt(t, baseURL, start)
}

func billingFixtureAt(t *testing.T, baseURL string, start time.Time) (string, string) {
	t.Helper()
	dir := t.TempDir()
	ledgerPath := filepath.Join(dir, "ledger.db")
	store, err := ledger.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
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
	statement := fmt.Sprintf("period_start,period_end,currency,amount,request_id,model\n%s,%s,USD,1,billed,model\n", start.Format(time.RFC3339), start.AddDate(0, 0, 1).Format(time.RFC3339))
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
	// Keep the existing ledger outside the default config directory to verify
	// that billing discovers its configured location without a path selector.
	t.Setenv("HOME", t.TempDir())
	configPath := defaultConfigPath()
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
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

func TestBillingCalendarMonthAndOpenPeriods(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		now, from, to string
		location      *time.Location
	}{
		{"2026-12-20T10:00:00+08:00", "2026-12-01T00:00:00+08:00", "2027-01-01T00:00:00+08:00", time.FixedZone("test", 8*3600)},
		{"2028-02-29T10:00:00Z", "2028-02-01T00:00:00Z", "2028-03-01T00:00:00Z", time.UTC},
		{"2026-03-20T10:00:00-04:00", "2026-03-01T00:00:00-05:00", "2026-04-01T00:00:00-04:00", newYork},
	} {
		t.Run(test.now, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, test.now)
			if err != nil {
				t.Fatal(err)
			}
			p, err := resolveBillingPeriod("", "", now.In(test.location))
			if err != nil || p.From.Format(time.RFC3339) != test.from || p.To.Format(time.RFC3339) != test.to {
				t.Fatalf("period=%+v err=%v", p, err)
			}
		})
	}
	startOnly, err := resolveBillingPeriod("2026-09-14T00:00:00+08:00", "", time.Now())
	if err != nil || startOnly.From == nil || startOnly.To != nil {
		t.Fatalf("start-only period=%+v err=%v", startOnly, err)
	}
	endOnly, err := resolveBillingPeriod("", "2026-09-14T00:00:00+08:00", time.Now())
	if err != nil || endOnly.From != nil || endOnly.To == nil {
		t.Fatalf("end-only period=%+v err=%v", endOnly, err)
	}
}

func TestBillingDefaultMonthAndExplicitRangeAgreeAcrossViews(t *testing.T) {
	now := time.Now()
	currentStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	currentEnd := currentStart.AddDate(0, 1, 0)
	for _, test := range []struct {
		name       string
		start, end time.Time
		flags      []string
	}{
		{name: "default month", start: currentStart, end: currentEnd},
		{name: "explicit offset range", start: time.Date(2026, 9, 1, 0, 0, 0, 0, time.FixedZone("test", 8*3600)), end: time.Date(2026, 10, 1, 0, 0, 0, 0, time.FixedZone("test", 8*3600)), flags: []string{"--from", "2026-09-01T00:00:00+08:00", "--to", "2026-10-01T00:00:00+08:00"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, path := billingFixtureAt(t, "https://example.com/v1", test.start.AddDate(0, 0, 13))
			l, err := ledger.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, audit := range []ledger.Audit{{ID: "previous-period", TimestampMS: test.start.UnixMilli() - 1}, {ID: "at-start", TimestampMS: test.start.UnixMilli()}, {ID: "next-period", TimestampMS: test.end.UnixMilli()}} {
				audit.Protocol, audit.Upstream, audit.Model, audit.Status = "openai", "test", "model", "ok"
				if err := l.AppendAudit(audit); err != nil {
					t.Fatal(err)
				}
			}
			l.Close()
			args := append([]string{"billing"}, test.flags...)
			human, err := runBillingTest(t, args...)
			if err != nil || !strings.Contains(string(human), "Requests: 4") || json.Valid(human) || strings.Contains(string(human), "Request details") {
				t.Fatalf("default summary=%q error=%v", human, err)
			}
			for _, boundary := range []string{test.start.Format(time.RFC3339), test.end.Format(time.RFC3339)} {
				if !strings.Contains(string(human), boundary) {
					t.Fatalf("missing resolved boundary %s: %s", boundary, human)
				}
			}
			data, err := runBillingTest(t, append(args, "--json")...)
			if err != nil {
				t.Fatal(err)
			}
			var summary billingReport
			if err := json.Unmarshal(data, &summary); err != nil || summary.Usage.RequestCount != 4 || summary.Requests != nil {
				t.Fatalf("summary=%s error=%v", data, err)
			}
			data, err = runBillingTest(t, append(args, "--details", "--json")...)
			if err != nil {
				t.Fatal(err)
			}
			var details billingReport
			if err := json.Unmarshal(data, &details); err != nil || details.Requests == nil || len(*details.Requests) != 4 {
				t.Fatalf("details=%s error=%v", data, err)
			}
			if !reflect.DeepEqual(details.Period, summary.Period) || !reflect.DeepEqual(details.Usage, summary.Usage) {
				t.Fatal("details used a different period from summary")
			}
			ids := map[string]bool{}
			for _, a := range *details.Requests {
				ids[a.ID] = true
			}
			if ids["previous-period"] || ids["next-period"] || !ids["at-start"] {
				t.Fatalf("incorrect period requests: %v", ids)
			}
			humanDetails, err := runBillingTest(t, append(args, "--details")...)
			if err != nil || !strings.Contains(string(humanDetails), "Request details") || !strings.Contains(string(humanDetails), "at-start") || !strings.Contains(string(humanDetails), "Unknown") {
				t.Fatalf("human details=%s error=%v", humanDetails, err)
			}
			destination := filepath.Join(t.TempDir(), "bill.csv")
			receipt, err := runBillingTest(t, append(args, "--download", destination, "--json")...)
			if err != nil {
				t.Fatal(err)
			}
			var downloaded struct {
				Period   billingPeriod `json:"period"`
				Download string        `json:"download"`
			}
			if err := json.Unmarshal(receipt, &downloaded); err != nil || downloaded.Download != destination || !reflect.DeepEqual(downloaded.Period, summary.Period) {
				t.Fatalf("download receipt=%s error=%v", receipt, err)
			}
			csvData, err := os.ReadFile(destination)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := csv.NewReader(bytes.NewReader(csvData)).ReadAll()
			if err != nil {
				t.Fatal(err)
			}
			exported := map[string]bool{}
			for _, row := range rows[1:] {
				if row[0] == "request" {
					exported[row[1]] = true
				}
			}
			if !reflect.DeepEqual(exported, ids) {
				t.Fatalf("download period disagrees: exported=%v details=%v", exported, ids)
			}
		})
	}
}

func TestBillingHumanOutputExplainsMissingEvidenceAndEmptyPeriods(t *testing.T) {
	billingFixture(t, "https://example.com/v1")
	out, err := runBillingTest(t, "billing", "--from", "2026-09-14T00:00:00Z", "--to", "2026-09-14T12:00:00Z")
	if err != nil || !strings.Contains(string(out), "Partial coverage") || !strings.Contains(string(out), "Outside selected bounds") || !strings.Contains(string(out), "Unknown") {
		t.Fatalf("partial-period output=%s error=%v", out, err)
	}
	out, err = runBillingTest(t, "billing", "--details", "--from", "2030-01-01T00:00:00Z")
	if err != nil || !strings.Contains(string(out), "No requests recorded for this period.") || !strings.Contains(string(out), "Requests: 0") {
		t.Fatalf("empty-period output=%s error=%v", out, err)
	}
	out, err = runBillingTest(t, "billing", "--details", "--json", "--from", "2030-01-01T00:00:00Z")
	if err != nil || !strings.Contains(string(out), `"requests":[]`) {
		t.Fatalf("empty JSON=%s error=%v", out, err)
	}
	var buf bytes.Buffer
	cost := 0.0000001
	unknown := int64(1)
	period, _ := resolveBillingPeriod("2026-09-01T00:00:00Z", "2026-10-01T00:00:00Z", time.Now())
	report := billingReport{Period: period, Reconciliation: []ledger.Reconciliation{{Currency: "USD", Coverage: "no_statement", LocalEstimated: cost, UnknownCostRequests: unknown, MissingStatementRequests: 1}}}
	if err := renderBilling(&buf, report); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"Pending", "Waiting for statement", "0.0000001", "1 unknown", "Unknown", "no check recorded yet"} {
		if !strings.Contains(buf.String(), expected) {
			t.Fatalf("missing %q: %s", expected, buf.String())
		}
	}
}

func TestBillingDisplaysExactMillisecondBoundaries(t *testing.T) {
	p, err := resolveBillingPeriod("2026-09-14T00:00:00.123Z", "2026-09-14T00:00:00.456Z", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(p.String(), ".123Z") || !strings.Contains(p.String(), ".456Z") {
		t.Fatalf("display lost precision: %s", p)
	}
	from, to := p.bounds()
	if to-from != 333 {
		t.Fatalf("range differs from display: %d..%d", from, to)
	}
	for _, args := range [][2]string{{"2026-09-14T00:00:00.1234Z", ""}, {"", "2026-09-14T00:00:00.1234Z"}} {
		if _, err := resolveBillingPeriod(args[0], args[1], time.Now()); err == nil {
			t.Fatal("submillisecond precision was silently rounded")
		}
	}
}

func TestBillingCurrencyTableKeepsColumnsAligned(t *testing.T) {
	period, _ := resolveBillingPeriod("2026-09-01T00:00:00Z", "2026-10-01T00:00:00Z", time.Now())
	r := billingReport{Period: period, Reconciliation: []ledger.Reconciliation{
		{Currency: "CNY", Coverage: "no_statement", MissingStatementRequests: 1},
		{Currency: "EUR", Coverage: "complete", StatementLines: 1, SupplierStatement: 20},
		{Currency: "USD", Coverage: "partial", StatementLines: 1, SupplierStatement: 30, LocalEstimated: .0000001, UnknownCostRequests: 1},
	}}
	var out bytes.Buffer
	if err := renderBilling(&out, r); err != nil {
		t.Fatal(err)
	}
	var column int
	for _, line := range strings.Split(out.String(), "\n") {
		switch {
		case strings.HasPrefix(line, "Currency"):
			column = strings.Index(line, "Local estimate")
		case strings.HasPrefix(line, "CNY"), strings.HasPrefix(line, "EUR"), strings.HasPrefix(line, "USD"):
			if got := 3 + len(line[3:]) - len(strings.TrimLeft(line[3:], " ")); got != column {
				t.Fatalf("currency columns do not align with header: %s", out.String())
			}
		}
	}
	if strings.Index(out.String(), "CNY: requests awaiting") < strings.Index(out.String(), "USD ") {
		t.Fatalf("notes interrupt table: %s", out.String())
	}
}
