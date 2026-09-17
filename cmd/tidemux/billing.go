package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"time"

	"github.com/hs3180/tidemux/internal/gateway"
	"github.com/hs3180/tidemux/internal/ledger"
)

// billing only reads data already recorded by the running gateway. It never
// starts reconciliation, resolves credentials or contacts the provider.
func billing(args []string, stdout, stderr *os.File) error {
	flags := flag.NewFlagSet("billing", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "path to JSON config")
	from := flags.String("from", "", "RFC3339 inclusive period start")
	to := flags.String("to", "", "RFC3339 exclusive period end")
	download := flags.String("download", "", "save stored billing data to a new CSV file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || flags.NArg() != 0 {
		return errors.New(usage)
	}
	fromMS, err := parseBillingTime(*from)
	if err != nil {
		return errors.New("invalid --from: use RFC3339 after the Unix epoch")
	}
	toMS, err := parseBillingTime(*to)
	if err != nil {
		return errors.New("invalid --to: use RFC3339 after the Unix epoch")
	}
	if toMS > 0 && fromMS >= toMS {
		return errors.New("invalid billing period: --to must follow --from")
	}
	config, err := gateway.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	store, err := ledger.OpenReadOnly(config.LedgerPath)
	if err != nil {
		return errors.New("cannot read existing ledger; start the gateway to initialize it")
	}
	defer store.Close()
	if *download != "" {
		return downloadBilling(context.Background(), store, *download, fromMS, toMS)
	}
	rows, err := store.Reconcile(context.Background(), fromMS, toMS)
	if err != nil {
		return errors.New("cannot read billing statistics")
	}
	sync, err := store.StatementSyncStatus(context.Background())
	if err != nil {
		return errors.New("cannot read statement synchronization status")
	}
	return json.NewEncoder(stdout).Encode(struct {
		Reconciliation []ledger.Reconciliation `json:"reconciliation"`
		StatementSync  ledger.StatementSync    `json:"statement_sync"`
	}{Reconciliation: rows, StatementSync: sync})
}

func downloadBilling(ctx context.Context, store *ledger.Ledger, path string, fromMS, toMS int64) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("cannot create billing CSV; choose a new writable path")
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if err := store.ExportBillingCSV(ctx, file, fromMS, toMS); err != nil {
		return errors.New("cannot export billing data")
	}
	if err := file.Close(); err != nil {
		return errors.New("cannot finish billing CSV")
	}
	complete = true
	return nil
}

func parseBillingTime(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	timestamp, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, err
	}
	if timestamp.UnixMilli() <= 0 {
		return 0, errors.New("time must follow the Unix epoch")
	}
	return timestamp.UnixMilli(), nil
}
