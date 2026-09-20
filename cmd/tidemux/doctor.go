package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
)

// Diagnostics are recorded local rejections. Reading them does not require
// credentials, a running gateway, or an initialized reconciliation schema.
func doctorDiagnostics(ledgerPath string, asJSON bool, stdout io.Writer) error {
	store, err := ledger.OpenAuditReadOnly(ledgerPath)
	if err != nil {
		return errors.New("cannot read existing ledger diagnostics; start the gateway to initialize the ledger")
	}
	defer store.Close()
	rows, err := store.RecentDiagnostics(context.Background(), 100)
	if err != nil {
		return errors.New("cannot read local diagnostics")
	}
	if asJSON {
		return json.NewEncoder(stdout).Encode(rows)
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintln(stdout, "No local diagnostics recorded.")
		return err
	}
	if _, err := fmt.Fprintln(stdout, "Local diagnostics (latest 100)"); err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "TIME (UTC)\tID\tPROTOCOL\tMETHOD\tENDPOINT\tSTATUS\tERROR"); err != nil {
		return err
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\n", time.UnixMilli(row.TimestampMS).UTC().Format(time.RFC3339Nano), row.ID, row.Protocol, row.Method, row.Endpoint, row.Status, row.ErrorCode); err != nil {
			return err
		}
	}
	return w.Flush()
}
