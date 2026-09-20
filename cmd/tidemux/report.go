package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/smtp"
	"net/url"
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
	if command != "generate" && command != "list" && command != "export" && command != "open" && command != "deliver" && command != "retry" {
		return errors.New(usage)
	}
	flags := flag.NewFlagSet("report "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "path to JSON config")
	day := flags.String("date", "", "report date (YYYY-MM-DD)")
	id := flags.Int64("id", 0, "report ID")
	channel := flags.String("channel", "macos", "macos or smtp")
	timezone := flags.String("timezone", "UTC", "IANA report timezone")
	budgetCurrency := flags.String("budget-currency", "", "currency of optional daily reporting limit")
	dailyBudget := flags.Float64("daily-budget", 0, "optional daily reporting limit; does not enforce requests")
	limit := flags.Int("limit", 30, "history rows")
	days := flags.Int("days", 30, "number of days in an HTML export")
	output := flags.String("output", "", "HTML export path (default: ledger directory/reports/latest.html)")
	openReport := flags.Bool("open", false, "open an HTML export in the default browser")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *configPath == "" {
		return errors.New(usage)
	}
	c, err := gateway.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if command == "open" {
		path := strings.TrimSpace(*output)
		if path == "" {
			path = defaultReportPath(c.LedgerPath)
		}
		return openHTMLReport(path)
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
		loc, err := time.LoadLocation(*timezone)
		if err != nil {
			return errors.New("invalid report timezone")
		}
		when := time.Now()
		if *day != "" {
			when, err = time.ParseInLocation("2006-01-02", *day, loc)
			if err != nil {
				return errors.New("invalid --date")
			}
		}
		r, e := l.GenerateDailyReport(context.Background(), when, *timezone, ledger.ReportBudget{Currency: *budgetCurrency, DailyLimit: *dailyBudget})
		if e != nil {
			return e
		}
		return json.NewEncoder(stdout).Encode(r)
	}
	if command == "export" {
		path := strings.TrimSpace(*output)
		if path == "" {
			path = defaultReportPath(c.LedgerPath)
		}
		if *days < 1 || *days > 3660 {
			return errors.New("report export requires --days 1..3660")
		}
		loc, err := time.LoadLocation(*timezone)
		if err != nil {
			return errors.New("invalid report timezone")
		}
		through := time.Now()
		if *day != "" {
			through, err = time.ParseInLocation("2006-01-02", *day, loc)
			if err != nil {
				return errors.New("invalid --date")
			}
		}
		result, err := exportHTMLReport(context.Background(), l, path, *timezone, through, *days, ledger.ReportBudget{Currency: *budgetCurrency, DailyLimit: *dailyBudget}, *openReport)
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(result)
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
	var reportPath string
	if *channel == "macos" {
		loc, e := time.LoadLocation(selected.Timezone)
		if e != nil {
			err = errors.New("invalid persisted report timezone")
		} else {
			through, parseErr := time.ParseInLocation("2006-01-02", selected.Day, loc)
			if parseErr != nil {
				err = errors.New("invalid persisted report date")
			} else {
				result, exportErr := exportHTMLReport(context.Background(), l, defaultReportPath(c.LedgerPath), selected.Timezone, through, *days, ledger.ReportBudget{Currency: *budgetCurrency, DailyLimit: *dailyBudget}, false)
				if exportErr != nil {
					err = exportErr
				} else {
					reportPath = result.Path
				}
			}
		}
	}
	if err == nil {
		err = deliverReport(*channel, c, *selected, reportPath)
	}
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

func deliverReport(channel string, c gateway.Config, r ledger.DailyReport, reportPath string) error {
	subject, body := fmt.Sprintf("TideMux %s usage report", r.Day), reportText(r)
	if channel == "macos" {
		return notifyMacOS(subject, body, reportPath)
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

func notifyMacOS(subject, body, reportPath string) error {
	if strings.TrimSpace(reportPath) == "" {
		return errors.New("macOS report notification requires an HTML report")
	}
	if notifier, ok := findTerminalNotifier(); ok {
		fileURL := (&url.URL{Scheme: "file", Path: reportPath}).String()
		if err := exec.Command(notifier, "-title", subject, "-message", body, "-open", fileURL, "-group", "tidemux-daily-report").Run(); err != nil {
			return fmt.Errorf("send clickable macOS notification: %w", err)
		}
		return nil
	}
	// AppleScript is available on every supported macOS installation, but its
	// notification API cannot attach a click target. Keep the existing
	// notification path and make the one-command fallback visible to users.
	fallback := body + " Open with: tidemux report open."
	if err := exec.Command("osascript", "-e", fmt.Sprintf("display notification %s with title %s", appleQuote(fallback), appleQuote(subject))).Run(); err != nil {
		return fmt.Errorf("send macOS notification: %w", err)
	}
	return nil
}

func findTerminalNotifier() (string, bool) {
	if path, err := exec.LookPath("terminal-notifier"); err == nil {
		return path, true
	}
	for _, path := range []string{"/opt/homebrew/bin/terminal-notifier", "/usr/local/bin/terminal-notifier"} {
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return path, true
		}
	}
	return "", false
}

func reportText(r ledger.DailyReport) string {
	cost := "unknown"
	if r.EstimatedCost != nil {
		cost = fmt.Sprintf("%.6f %s", *r.EstimatedCost, r.Currency)
	}
	mismatches, unmatched := "unknown", "unknown"
	if r.TokenizerMismatches != nil {
		mismatches = strconv.FormatInt(*r.TokenizerMismatches, 10)
	}
	if r.UnmatchedStatements != nil {
		unmatched = strconv.FormatInt(*r.UnmatchedStatements, 10)
	}
	return fmt.Sprintf("Requests: %d; failures: %d; input tokens: %d; output tokens: %d; local estimated cost: %s; unknown local costs: %d; tokenizer mismatches: %s; unmatched statement lines: %s.", r.RequestCount, r.FailureCount, r.InputTokens, r.OutputTokens, cost, r.UnknownCostRequests, mismatches, unmatched)
}
func appleQuote(s string) string { return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\"" }
