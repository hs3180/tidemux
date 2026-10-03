package ledger

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGatewayOwnershipPreventsLiveRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	l, err := OpenForGateway(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	p := BudgetPolicy{Currency: "USD", FiveHourLimit: 10, AlertThreshold: .8, Mode: "hard"}
	if _, err := l.CheckBudget(context.Background(), "live", "p", p, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(path), "alias.db")
	if err := os.Symlink(path, alias); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{path, alias} {
		other, err := OpenForGateway(target)
		if err == nil {
			other.Close()
			t.Fatal("second gateway claimed a live ledger")
		}
	}
	report, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := report.Close(); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := l.QueryRow(context.Background(), "SELECT state FROM budget_charges WHERE request_id='live'").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "pending" {
		t.Fatalf("live state=%s", state)
	}
	if err := l.ReleaseBudgetReservation(context.Background(), "live"); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenForGateway(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
}
