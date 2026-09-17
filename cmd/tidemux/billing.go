package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/hs3180/tidemux/internal/gateway"
	"github.com/hs3180/tidemux/internal/ledger"
)

type billingPeriod struct {
	From *time.Time `json:"from"`
	To   *time.Time `json:"to"`
}

func (p billingPeriod) bounds() (int64, int64) {
	var from, to int64
	if p.From != nil {
		from = p.From.UnixMilli()
	}
	if p.To != nil {
		to = p.To.UnixMilli()
	}
	return from, to
}

func (p billingPeriod) String() string {
	from, to := "beginning", "no upper bound"
	if p.From != nil {
		from = p.From.Format(time.RFC3339Nano)
	}
	if p.To != nil {
		to = p.To.Format(time.RFC3339Nano)
	}
	return from + " to " + to + " (end exclusive)"
}

type billingReport struct {
	Period         billingPeriod           `json:"period"`
	Usage          ledger.UsageSummary     `json:"usage"`
	Reconciliation []ledger.Reconciliation `json:"reconciliation"`
	StatementSync  ledger.StatementSync    `json:"statement_sync"`
	Requests       *[]ledger.Audit         `json:"requests,omitempty"`
}

// billing reads data already recorded by the gateway. Every view uses the same
// resolved period; queries never start reconciliation or contact the provider.
func billing(args []string, stdout, stderr *os.File) error {
	flags := flag.NewFlagSet("billing", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "path to JSON config")
	from := flags.String("from", "", "RFC3339 inclusive start (default period: current local calendar month)")
	to := flags.String("to", "", "RFC3339 exclusive end (an omitted bound is open when the other is supplied)")
	details := flags.Bool("details", false, "show all request details in the selected period")
	asJSON := flags.Bool("json", false, "output structured data or a download receipt as JSON")
	download := flags.String("download", "", "save stored billing details to a new CSV file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || flags.NArg() != 0 {
		return errors.New(usage)
	}
	period, err := resolveBillingPeriod(*from, *to, time.Now())
	if err != nil {
		return err
	}
	fromMS, toMS := period.bounds()
	config, err := gateway.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	store, err := ledger.OpenReadOnly(config.LedgerPath)
	if err != nil {
		return errors.New("cannot read existing ledger; start the gateway to initialize it")
	}
	defer store.Close()
	ctx := context.Background()
	if *download != "" {
		if err := downloadBilling(ctx, store, *download, fromMS, toMS); err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(stdout).Encode(struct {
				Period   billingPeriod `json:"period"`
				Download string        `json:"download"`
			}{period, *download})
		}
		_, err := fmt.Fprintf(stdout, "Saved billing details to %s\nPeriod: %s\n", billingCell(*download), period)
		return err
	}
	result := billingReport{Period: period}
	result.Usage, err = store.BillingUsage(ctx, fromMS, toMS)
	if err != nil {
		return errors.New("cannot read billing usage")
	}
	result.Reconciliation, err = store.Reconcile(ctx, fromMS, toMS)
	if err != nil {
		return errors.New("cannot read billing statistics")
	}
	result.StatementSync, err = store.StatementSyncStatus(ctx)
	if err != nil {
		return errors.New("cannot read statement synchronization status")
	}
	if *details {
		requests, err := store.RequestDetails(ctx, fromMS, toMS)
		if err != nil {
			return errors.New("cannot read request details")
		}
		result.Requests = &requests
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(result)
	}
	return renderBilling(stdout, result)
}

func resolveBillingPeriod(from, to string, now time.Time) (billingPeriod, error) {
	if from == "" && to == "" {
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
		end := start.AddDate(0, 1, 0)
		return billingPeriod{From: &start, To: &end}, nil
	}
	var p billingPeriod
	for _, boundary := range []struct {
		name  string
		value string
		dest  **time.Time
	}{{"from", from, &p.From}, {"to", to, &p.To}} {
		if boundary.value == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, boundary.value)
		if err != nil || t.UnixMilli() <= 0 {
			return p, fmt.Errorf("invalid --%s: use RFC3339 after the Unix epoch", boundary.name)
		}
		if t.Nanosecond()%int(time.Millisecond) != 0 {
			return p, fmt.Errorf("invalid --%s: timestamps support millisecond precision", boundary.name)
		}
		*boundary.dest = &t
	}
	fromMS, toMS := p.bounds()
	if toMS > 0 && fromMS >= toMS {
		return p, errors.New("invalid billing period: --to must follow --from")
	}
	return p, nil
}

func renderBilling(w io.Writer, report billingReport) error {
	out := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(out, "Billing\nPeriod: %s\n\n", report.Period)
	u := report.Usage
	fmt.Fprintf(out, "Requests: %d (succeeded: %d, errors: %d, canceled: %d)\n", u.RequestCount, u.SuccessfulRequests, u.ErrorRequests, u.CanceledRequests)
	fmt.Fprintf(out, "Input tokens: %s\nOutput tokens: %s\n\n", billingKnownTotal(u.InputTokens, u.UnknownInputRequests), billingKnownTotal(u.OutputTokens, u.UnknownOutputRequests))
	if len(report.Reconciliation) == 0 {
		fmt.Fprintln(out, "No usage or supplier statements recorded for this period.")
	} else {
		var notes []string
		fmt.Fprintln(out, "Currency\tLocal estimate\tSupplier statement\tDifference\tReconciliation")
		for _, r := range report.Reconciliation {
			currency := r.Currency
			if currency == "" {
				currency = "Unknown"
			}
			local := strconv.FormatFloat(r.LocalEstimated, 'f', -1, 64)
			if r.UnknownCostRequests > 0 {
				local += fmt.Sprintf(" known; %d unknown", r.UnknownCostRequests)
			}
			supplier := "Pending"
			if r.StatementLines > 0 {
				supplier = strconv.FormatFloat(r.SupplierStatement, 'f', -1, 64)
			} else if r.PartialStatementLines > 0 {
				supplier = "Outside selected bounds"
			}
			status := "Partial coverage"
			switch r.Coverage {
			case "no_statement":
				status = "Waiting for statement"
			case "complete":
				status = "Complete coverage"
			}
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", billingCell(currency), local, supplier, billingAmount(r.Difference), status)
			if r.MissingStatementRequests > 0 || r.UnmatchedLines > 0 || r.PartialStatementLines > 0 {
				notes = append(notes, fmt.Sprintf("  %s: requests awaiting a match: %d; unmatched statement lines: %d; partial-period lines: %d", billingCell(currency), r.MissingStatementRequests, r.UnmatchedLines, r.PartialStatementLines))
			}
		}
		if len(notes) > 0 {
			fmt.Fprintln(out)
			for _, note := range notes {
				fmt.Fprintln(out, note)
			}
		}
	}
	s := report.StatementSync
	fmt.Fprintln(out)
	if s.LastAttemptMS == 0 {
		fmt.Fprintln(out, "Statement sync: no check recorded yet.")
	} else {
		fmt.Fprintf(out, "Statement sync: last checked %s; %d failures.\n", billingTimestamp(s.LastAttemptMS), s.Failures)
		if s.LastSuccessMS == nil {
			fmt.Fprintln(out, "Last successful check: none.")
		} else {
			fmt.Fprintf(out, "Last successful check: %s.\n", billingTimestamp(*s.LastSuccessMS))
		}
	}
	if report.Requests != nil {
		fmt.Fprintln(out, "\nRequest details")
		if len(*report.Requests) == 0 {
			fmt.Fprintln(out, "No requests recorded for this period.")
		} else {
			fmt.Fprintln(out, "Time\tRequest ID\tModel\tStatus\tInput\tOutput\tEstimated cost")
			for _, a := range *report.Requests {
				cost := billingAmount(a.EstimatedCost)
				if a.EstimatedCost != nil {
					cost += " " + billingCell(a.Currency)
				}
				status := a.Status
				if a.ErrorCode != "" {
					status += ": " + a.ErrorCode
				}
				fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", billingTimestamp(a.TimestampMS), billingCell(a.ID), billingCell(a.Model), billingCell(status), billingTokens(a.InputTokens), billingTokens(a.OutputTokens), cost)
			}
		}
	}
	return out.Flush()
}

func billingTimestamp(ms int64) string {
	return time.UnixMilli(ms).In(time.Local).Format(time.RFC3339Nano)
}
func billingKnownTotal(total, unknown int64) string {
	if unknown > 0 {
		return fmt.Sprintf("%d known; requests with unknown usage: %d", total, unknown)
	}
	return strconv.FormatInt(total, 10)
}
func billingTokens(n *int64) string {
	if n == nil {
		return "Unknown"
	}
	return strconv.FormatInt(*n, 10)
}
func billingAmount(n *float64) string {
	if n == nil {
		return "Unknown"
	}
	return strconv.FormatFloat(*n, 'f', -1, 64)
}
func billingCell(value string) string {
	if strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) }) >= 0 {
		return strconv.Quote(value)
	}
	return value
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
