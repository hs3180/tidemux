package ledger

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func TestBillingUsagePeriodAndUnknownCounts(t *testing.T) {
	l, _ := openReconciliationTest(t)
	ctx := context.Background()
	zero, input, output := int64(0), int64(20), int64(8)
	for _, a := range []Audit{
		{ID: "before", TimestampMS: 99, Status: "ok", InputTokens: &input, OutputTokens: &output},
		{ID: "start", TimestampMS: 100, Status: "ok", InputTokens: &input, OutputTokens: &output},
		{ID: "error", TimestampMS: 150, Status: "error", OutputTokens: &zero},
		{ID: "canceled", TimestampMS: 199, Status: "canceled", InputTokens: &zero},
		{ID: "end", TimestampMS: 200, Status: "ok", InputTokens: &input, OutputTokens: &output},
	} {
		a.Protocol, a.Upstream, a.Model = "openai", "test", "m"
		if err := l.AppendAudit(a); err != nil {
			t.Fatal(err)
		}
	}
	want := UsageSummary{RequestCount: 3, SuccessfulRequests: 1, ErrorRequests: 1, CanceledRequests: 1, InputTokens: 20, OutputTokens: 8, UnknownInputRequests: 1, UnknownOutputRequests: 1}
	got, err := l.BillingUsage(ctx, 100, 200)
	if err != nil || got != want {
		t.Fatalf("usage=%+v want=%+v err=%v", got, want, err)
	}
	for _, tc := range []struct {
		name     string
		from, to int64
		count    int64
	}{
		{"all", 0, 0, 5},
		{"from", 150, 0, 3},
		{"to", 0, 150, 2},
		{"empty", 201, 300, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary, err := l.BillingUsage(ctx, tc.from, tc.to)
			if err != nil || summary.RequestCount != tc.count {
				t.Fatalf("usage=%+v err=%v", summary, err)
			}
			if tc.count == 0 && summary != (UsageSummary{}) {
				t.Fatalf("nonzero empty usage=%+v", summary)
			}
		})
	}
}

func TestBillingRequestDetailsCompleteOrderedAndNullPreserving(t *testing.T) {
	l, _ := openReconciliationTest(t)
	ctx := context.Background()
	for i := 0; i < 105; i++ {
		a := auditForReconciliation(fmt.Sprintf("request-%03d", i), "", int64(100+i), nil)
		a.InputTokens, a.OutputTokens = nil, nil
		a.Events = []string{"queue_wait"}
		if i == 102 {
			a.TimestampMS = 201 // Same timestamp as request-101; ID breaks the tie.
		}
		if err := l.AppendAudit(a); err != nil {
			t.Fatal(err)
		}
	}
	details, err := l.RequestDetails(ctx, 100, 203)
	if err != nil {
		t.Fatal(err)
	}
	if len(details) != 103 || details[0].ID != "request-102" || details[1].ID != "request-101" || details[102].ID != "request-000" {
		t.Fatalf("unexpected complete ordered details: len=%d", len(details))
	}
	if details[0].InputTokens != nil || details[0].OutputTokens != nil || details[0].EstimatedCost != nil || !reflect.DeepEqual(details[0].Events, []string{"queue_wait"}) {
		t.Fatalf("audit fields lost: %+v", details[0])
	}
	for _, tc := range []struct {
		name     string
		from, to int64
		count    int
	}{
		{"all", 0, 0, 105},
		{"from", 203, 0, 2},
		{"to", 0, 101, 1},
		{"empty", 300, 400, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := l.RequestDetails(ctx, tc.from, tc.to)
			if err != nil || len(rows) != tc.count {
				t.Fatalf("rows=%d want=%d err=%v", len(rows), tc.count, err)
			}
			if tc.count == 0 {
				encoded, err := json.Marshal(rows)
				if err != nil || string(encoded) != "[]" {
					t.Fatalf("empty details=%s err=%v", encoded, err)
				}
			}
		})
	}
}

func TestBillingViewsReadOnlyAndInvalidPeriods(t *testing.T) {
	l, path := openReconciliationTest(t)
	if err := l.AppendAudit(auditForReconciliation("read", "", 100, nil)); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx := context.Background()
	if usage, err := reader.BillingUsage(ctx, 0, 0); err != nil || usage.RequestCount != 1 {
		t.Fatalf("read-only usage=%+v err=%v", usage, err)
	}
	if details, err := reader.RequestDetails(ctx, 0, 0); err != nil || len(details) != 1 {
		t.Fatalf("read-only details=%+v err=%v", details, err)
	}
	var writes int64
	if err = reader.QueryRow(ctx, `SELECT total_changes()`).Scan(&writes); err != nil || writes != 0 {
		t.Fatalf("read-only views performed %d writes: %v", writes, err)
	}
	for _, bounds := range [][2]int64{{-1, 0}, {0, -1}, {100, 100}, {200, 100}} {
		if _, err := reader.BillingUsage(ctx, bounds[0], bounds[1]); err == nil {
			t.Fatalf("usage accepted invalid period %v", bounds)
		}
		if _, err := reader.RequestDetails(ctx, bounds[0], bounds[1]); err == nil {
			t.Fatalf("details accepted invalid period %v", bounds)
		}
	}
}
