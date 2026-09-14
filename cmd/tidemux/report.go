package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/smtp"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/gateway"
	"github.com/hs3180/tidemux/internal/ledger"
)

func report(args []string, stdout, stderr *os.File) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	command := args[0]
	if command != "generate" && command != "list" && command != "deliver" && command != "retry" {
		return errors.New(usage)
	}
	flags := flag.NewFlagSet("report "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "path to JSON config")
	day := flags.String("date", "", "report date (YYYY-MM-DD)")
	id := flags.Int64("id", 0, "report ID")
	channel := flags.String("channel", "macos", "macos or smtp")
	limit := flags.Int("limit", 30, "history rows")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *configPath == "" {
		return errors.New(usage)
	}
	c, err := gateway.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	l, err := ledger.Open(c.LedgerPath)
	if err != nil {
		return errors.New("cannot open ledger")
	}
	defer l.Close()
	if command == "list" {
		rows, e := l.ListDailyReports(context.Background(), *limit)
		if e != nil {
			return e
		}
		return json.NewEncoder(stdout).Encode(rows)
	}
	if command == "generate" {
		when := time.Now()
		if *day != "" {
			when, err = time.Parse("2006-01-02", *day)
			if err != nil {
				return errors.New("invalid --date")
			}
		}
		timezone := "UTC"
		if c.Budget.Timezone != "" {
			timezone = c.Budget.Timezone
		}
		r, e := l.GenerateDailyReport(context.Background(), when, timezone, c.Budget)
		if e != nil {
			return e
		}
		return json.NewEncoder(stdout).Encode(r)
	}
	if *id < 1 || (*channel != "macos" && *channel != "smtp") {
		return errors.New(usage)
	}
	reports, err := l.ListDailyReports(context.Background(), 3660)
	if err != nil {
		return err
	}
	var selected *ledger.DailyReport
	for i := range reports {
		if reports[i].ID == *id {
			selected = &reports[i]
			break
		}
	}
	if selected == nil {
		return errors.New("report not found")
	}
	if command == "retry" {
		deliveries, e := l.ReportDeliveries(context.Background(), *id)
		if e != nil {
			return e
		}
		failed := false
		for _, d := range deliveries {
			failed = failed || (d.Channel == *channel && d.Status == "failed")
		}
		if !failed {
			return errors.New("no failed delivery for channel")
		}
	}
	err = deliverReport(*channel, c, *selected)
	status, code := "sent", ""
	if err != nil {
		status, code = "failed", "delivery_failed"
	}
	if e := l.RecordDelivery(context.Background(), selected.ID, *channel, status, code); e != nil {
		return e
	}
	if err != nil {
		return errors.New(code)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"report_id": selected.ID, "channel": *channel, "status": status})
}

func deliverReport(channel string, c gateway.Config, r ledger.DailyReport) error {
	subject, body := fmt.Sprintf("TideMux %s usage report", r.Day), reportText(r)
	if channel == "macos" {
		return exec.Command("osascript", "-e", fmt.Sprintf("display notification %s with title %s", appleQuote(body), appleQuote(subject))).Run()
	}
	if c.SMTP == (gateway.SMTPConfig{}) {
		return errors.New("smtp is not configured")
	}
	secret, err := gateway.MacOSKeychain{}.Lookup(context.Background(), c.SMTP.Keychain)
	if err != nil {
		return errors.New("smtp Keychain item unavailable")
	}
	parts := strings.SplitN(secret, ":", 2)
	if len(parts) != 2 {
		return errors.New("smtp Keychain value must be username:password")
	}
	auth := smtp.PlainAuth("", parts[0], parts[1], c.SMTP.Host)
	message := "To: " + c.SMTP.To + "\r\nFrom: " + c.SMTP.From + "\r\nSubject: " + subject + "\r\n\r\n" + body
	return smtp.SendMail(c.SMTP.Host+":"+strconv.Itoa(c.SMTP.Port), auth, c.SMTP.From, []string{c.SMTP.To}, []byte(message))
}

func reportText(r ledger.DailyReport) string {
	cost := "unknown"
	if r.EstimatedCost != nil {
		cost = fmt.Sprintf("%.6f %s", *r.EstimatedCost, r.Currency)
	}
	return fmt.Sprintf("Requests: %d; failures: %d; input tokens: %d; output tokens: %d; local estimated cost: %s; unknown local costs: %d; tokenizer mismatches: %d; unmatched statement lines: %d.", r.RequestCount, r.FailureCount, r.InputTokens, r.OutputTokens, cost, r.UnknownCostRequests, r.TokenizerMismatches, r.UnmatchedStatements)
}
func appleQuote(s string) string { return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\"" }
