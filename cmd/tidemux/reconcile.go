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

func reconcile(args []string, stdout, stderr *os.File) error {
	if len(args) == 0 || (args[0] != "report" && args[0] != "import") {
		return errors.New(usage)
	}
	command := args[0]
	flags := flag.NewFlagSet("reconcile "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "path to JSON config")
	from := flags.String("from", "", "RFC3339 inclusive period start")
	to := flags.String("to", "", "RFC3339 exclusive period end")
	file := flags.String("file", "", "supplier CSV statement")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *configPath == "" || flags.NArg() != 0 || (command == "import" && *file == "") {
		return errors.New(usage)
	}
	c, err := gateway.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	store, err := ledger.Open(c.LedgerPath)
	if err != nil {
		return errors.New("cannot open ledger")
	}
	defer store.Close()
	if command == "import" {
		f, err := os.Open(*file)
		if err != nil {
			return errors.New("cannot read statement CSV")
		}
		defer f.Close()
		n, err := store.ImportStatementCSV(context.Background(), f, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(map[string]int{"imported_lines": n})
	}
	fromMS, err := parseReconcileTime(*from)
	if err != nil {
		return errors.New("invalid --from")
	}
	toMS, err := parseReconcileTime(*to)
	if err != nil {
		return errors.New("invalid --to")
	}
	rows, err := store.Reconcile(context.Background(), fromMS, toMS)
	if err != nil {
		return errors.New("cannot reconcile ledger")
	}
	return json.NewEncoder(stdout).Encode(rows)
}
func parseReconcileTime(v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return 0, err
	}
	return t.UnixMilli(), nil
}
