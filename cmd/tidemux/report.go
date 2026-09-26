package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"github.com/hs3180/tidemux/internal/gateway"
	"github.com/hs3180/tidemux/internal/ledger"
)

const macOSNotificationSettingsURL = "x-apple.systempreferences:com.apple.Notifications-Settings.extension"

func report(args []string, stdout, stderr *os.File) error {
	return reportWithKeychain(args, stdout, stderr, gateway.MacOSKeychain{})
}

func reportWithKeychain(args []string, stdout, stderr *os.File, keychain gateway.SecretLookup) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	command := args[0]
	if command == "schedule" {
		return scheduleCommand(args[1:], stdout, stderr)
	}
	if command == "webhook" {
		return configureReportWebhook(args[1:], stdout, stderr)
	}
	if command != "generate" && command != "list" && command != "export" && command != "open" && command != "deliver" && command != "retry" && command != "deliveries" && command != "notify" {
		return errors.New(usage)
	}
	flags := flag.NewFlagSet("report "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", defaultConfigPath(), "path to JSON config")
	var day, timezone, output *string
	var id *int64
	var channel, budgetCurrency *string
	var dailyBudget *float64
	var limit, days *int
	var openReport *bool
	switch command {
	case "generate":
		day = flags.String("date", "", "report date (YYYY-MM-DD)")
		timezone = flags.String("timezone", "UTC", "IANA report timezone")
		budgetCurrency = flags.String("budget-currency", "", "currency of optional daily reporting limit")
		dailyBudget = flags.Float64("daily-budget", 0, "optional daily reporting limit; does not enforce requests")
	case "list":
		limit = flags.Int("limit", 30, "history rows")
	case "export":
		day = flags.String("date", "", "report date (YYYY-MM-DD)")
		timezone = flags.String("timezone", "UTC", "IANA report timezone")
		budgetCurrency = flags.String("budget-currency", "", "currency of optional daily reporting limit")
		dailyBudget = flags.Float64("daily-budget", 0, "optional daily reporting limit; does not enforce requests")
		days = flags.Int("days", 30, "number of days in the HTML export")
		output = flags.String("output", "", "HTML export path (default: ledger directory/reports/latest.html)")
		openReport = flags.Bool("open", false, "open an HTML export in the default browser")
	case "open":
		output = flags.String("output", "", "HTML report path (default: ledger directory/reports/latest.html)")
	case "deliver", "retry":
		id = flags.Int64("id", 0, "report ID")
		channel = flags.String("channel", "macos", "delivery channel: macos or webhook")
		budgetCurrency = flags.String("budget-currency", "", "currency of optional daily reporting limit")
		dailyBudget = flags.Float64("daily-budget", 0, "optional daily reporting limit; does not enforce requests")
		days = flags.Int("days", 30, "number of days in a refreshed HTML report")
	case "deliveries":
		id = flags.Int64("id", 0, "report ID")
	case "notify":
		channel = flags.String("channel", "macos", "delivery channel: macos or webhook")
		timezone = flags.String("timezone", "UTC", "IANA report timezone")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *configPath == "" {
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
	if command == "deliveries" {
		if *id < 1 {
			return errors.New("report deliveries requires --id N")
		}
		deliveries, err := l.ReportDeliveries(context.Background(), *id)
		if err != nil {
			return err
		}
		if len(deliveries) == 0 {
			return errors.New("no delivery records found for report")
		}
		return json.NewEncoder(stdout).Encode(deliveries)
	}
	if command == "generate" {
		loc, err := reportLocation(*timezone)
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
		loc, err := reportLocation(*timezone)
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
	if command == "notify" {
		selectedChannel := *channel
		if !flagWasSet(flags, "channel") {
			if c.ReportSchedule == (gateway.ReportSchedule{}) {
				return errors.New("scheduled notifications are not configured")
			}
			selectedChannel = c.ReportSchedule.EffectiveChannel()
		}
		if selectedChannel != "macos" && selectedChannel != "webhook" {
			return errors.New("notification channel must be macos or webhook")
		}
		notifyTimezone := "Local"
		if flagWasSet(flags, "timezone") {
			notifyTimezone = *timezone
		}
		return notifyReportWithKeychain(stdout, l, c, selectedChannel, notifyTimezone, keychain)
	}
	if *id < 1 || (*channel != "macos" && *channel != "webhook") {
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
	var deliveryErr error
	if *channel == "webhook" {
		c, deliveryErr = resolveReportWebhook(c, keychain)
	}
	var reportPath string
	if deliveryErr == nil && *channel == "macos" {
		loc, e := reportLocation(selected.Timezone)
		if e != nil {
			deliveryErr = errors.New("invalid persisted report timezone")
		} else {
			through, parseErr := time.ParseInLocation("2006-01-02", selected.Day, loc)
			if parseErr != nil {
				deliveryErr = errors.New("invalid persisted report date")
			} else {
				result, exportErr := exportHTMLReport(context.Background(), l, defaultReportPath(c.LedgerPath), selected.Timezone, through, *days, ledger.ReportBudget{Currency: *budgetCurrency, DailyLimit: *dailyBudget}, false)
				if exportErr != nil {
					deliveryErr = exportErr
				} else {
					reportPath = result.Path
				}
			}
		}
	}
	if deliveryErr == nil {
		deliveryErr = deliverReport(*channel, *selected, reportPath, c.ReportWebhookURL, c.ReportWebhook.Provider)
	}
	status, code := "sent", ""
	if deliveryErr != nil {
		status, code = "failed", "delivery_failed"
	}
	if e := l.RecordDelivery(context.Background(), selected.ID, *channel, status, code); e != nil {
		return e
	}
	if deliveryErr != nil {
		return fmt.Errorf("%s: %w", code, deliveryErr)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"report_id": selected.ID, "channel": *channel, "status": status})
}

func reportLocation(timezone string) (*time.Location, error) {
	if timezone == "Local" {
		return time.Local, nil
	}
	return time.LoadLocation(timezone)
}

func notifyReport(stdout *os.File, l *ledger.Ledger, c gateway.Config, channel, timezone string) error {
	return notifyReportWithKeychain(stdout, l, c, channel, timezone, gateway.MacOSKeychain{})
}

func notifyReportWithKeychain(stdout *os.File, l *ledger.Ledger, c gateway.Config, channel, timezone string, keychain gateway.SecretLookup) error {
	through := time.Now()
	report, err := l.GenerateDailyReport(context.Background(), through, timezone, ledger.ReportBudget{})
	if err != nil {
		return err
	}
	var deliveryErr error
	if channel == "webhook" {
		c, deliveryErr = resolveReportWebhook(c, keychain)
	}
	reportPath := ""
	if deliveryErr == nil && channel == "macos" {
		export, err := exportHTMLReport(context.Background(), l, defaultReportPath(c.LedgerPath), timezone, through, 30, ledger.ReportBudget{}, false)
		if err != nil {
			deliveryErr = err
		} else {
			reportPath = export.Path
		}
	}
	status, code := "sent", ""
	if deliveryErr == nil {
		deliveryErr = deliverReport(channel, report, reportPath, c.ReportWebhookURL, c.ReportWebhook.Provider)
	}
	if deliveryErr != nil {
		status, code = "failed", "delivery_failed"
		if recordErr := l.RecordDelivery(context.Background(), report.ID, channel, status, code); recordErr != nil {
			return recordErr
		}
		return fmt.Errorf("%s: %w", code, deliveryErr)
	}
	if err := l.RecordDelivery(context.Background(), report.ID, channel, status, code); err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"report_id": report.ID, "channel": channel, "status": status})
}

func resolveReportWebhook(c gateway.Config, lookup gateway.SecretLookup) (gateway.Config, error) {
	if c.ReportWebhook == (gateway.ReportWebhookConfig{}) {
		return c, errors.New("report webhook is not configured")
	}
	if lookup == nil {
		return gateway.Config{}, errors.New("report webhook Keychain lookup unavailable")
	}
	endpoint, err := lookup.Lookup(context.Background(), c.ReportWebhook.Keychain)
	if err != nil {
		return gateway.Config{}, errors.New("report webhook Keychain item unavailable")
	}
	if err := validateReportWebhookEndpoint(endpoint); err != nil {
		return gateway.Config{}, err
	}
	c.ReportWebhookURL = endpoint
	return c, nil
}

func deliverReport(channel string, r ledger.DailyReport, reportPath, webhookURL, webhookProvider string) error {
	return deliverReportWithLocale(channel, r, reportPath, webhookURL, webhookProvider, detectReportLocale())
}

func deliverReportWithLocale(channel string, r ledger.DailyReport, reportPath, webhookURL, webhookProvider string, locale reportLocale) error {
	messages := reportMessagesFor(locale)
	subject, body := fmt.Sprintf(messages.subjectFormat, r.Day), reportTextForLocale(r, locale)
	if channel == "macos" {
		return notifyMacOS(subject, body, reportPath)
	}
	if channel == "webhook" {
		return postReportWebhook(webhookURL, webhookProvider, body)
	}
	return errors.New("notification channel must be macos or webhook")
}

const maxReportWebhookBytes = 10000

func postReportWebhook(endpoint, provider, message string) error {
	if err := validateReportWebhookEndpoint(endpoint); err != nil {
		return err
	}
	parsed, err := url.Parse(endpoint)
	if len([]byte(message)) > maxReportWebhookBytes {
		return errors.New("report webhook message is too large")
	}
	var payload any
	switch provider {
	case "generic", "telegram":
		payload = map[string]string{"text": message}
	case "discord":
		payload = map[string]string{"content": message}
	case "lark":
		payload = map[string]any{"msg_type": "text", "content": map[string]string{"text": message}}
	default:
		return errors.New("unsupported report webhook provider")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return errors.New("cannot encode report webhook")
	}
	req, err := http.NewRequest(http.MethodPost, parsed.String(), strings.NewReader(string(data)))
	if err != nil {
		return errors.New("cannot prepare report webhook")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "tidemux/"+version)
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("report webhook request failed: %s", safeWebhookDiagnostic(err.Error(), endpoint))
	}
	defer response.Body.Close()
	if provider != "lark" {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return fmt.Errorf("report webhook returned HTTP %d", response.StatusCode)
		}
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxReportWebhookResponseBytes+1))
	if err != nil {
		return errors.New("could not read Feishu webhook response")
	}
	if len(body) > maxReportWebhookResponseBytes {
		return errors.New("Feishu webhook response exceeded the diagnostic size limit")
	}
	return validateLarkWebhookResponse(response.StatusCode, body, endpoint)
}

const maxReportWebhookResponseBytes = 4096

func validateLarkWebhookResponse(status int, body []byte, endpoint string) error {
	var result struct {
		Code          *int64 `json:"code"`
		Message       string `json:"msg"`
		StatusCode    *int64 `json:"StatusCode"`
		StatusMessage string `json:"StatusMessage"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("Feishu returned HTTP %d with an invalid JSON response", status)
	}
	code, message := result.Code, result.Message
	if code == nil {
		code, message = result.StatusCode, result.StatusMessage
	}
	if code == nil {
		return fmt.Errorf("Feishu returned HTTP %d without a response code", status)
	}
	if status >= 200 && status < 300 && *code == 0 {
		return nil
	}
	detail := fmt.Sprintf("Feishu returned HTTP %d, code %d", status, *code)
	if safeMessage := safeWebhookDiagnostic(message, endpoint); safeMessage != "" {
		detail += ": " + safeMessage
	}
	return errors.New(detail)
}

// safeWebhookDiagnostic includes useful transport/provider details without
// allowing a URL, path token, or query credential to escape into CLI output.
func safeWebhookDiagnostic(message, endpoint string) string {
	secrets := []string{endpoint}
	if parsed, err := url.Parse(endpoint); err == nil {
		for _, segment := range strings.Split(parsed.EscapedPath(), "/") {
			decoded, decodeErr := url.PathUnescape(segment)
			if decodeErr == nil && len(decoded) >= 16 {
				secrets = append(secrets, decoded, segment)
			}
		}
		for _, values := range parsed.Query() {
			for _, value := range values {
				if len(value) >= 8 {
					secrets = append(secrets, value)
				}
			}
		}
	}
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, message)
	message = strings.Join(strings.Fields(message), " ")
	runes := []rune(message)
	if len(runes) > 256 {
		message = string(runes[:256]) + "…"
	}
	return message
}

func validateReportWebhookEndpoint(endpoint string) error {
	if strings.TrimSpace(endpoint) == "" {
		return errors.New("report webhook endpoint is not configured")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && net.ParseIP(parsed.Hostname()) != nil && net.ParseIP(parsed.Hostname()).IsLoopback())) {
		return errors.New("report webhook endpoint must use HTTPS, except loopback HTTP")
	}
	return nil
}

func notifyMacOS(subject, body, reportPath string) error {
	if strings.TrimSpace(reportPath) == "" {
		return errors.New("macOS report notification requires an HTML report")
	}
	if notifier, ok := findTerminalNotifier(); ok {
		fileURL := (&url.URL{Scheme: "file", Path: reportPath}).String()
		output, err := exec.Command(notifier, "-title", subject, "-message", body, "-open", fileURL, "-group", "tidemux-daily-report").CombinedOutput()
		if err != nil {
			if notificationPermissionError(output) {
				if settingsErr := openMacOSNotificationSettings(); settingsErr != nil {
					return fmt.Errorf("send clickable macOS notification: %w (could not open notification settings: %v)", err, settingsErr)
				}
				return fmt.Errorf("send clickable macOS notification: %w (notification settings opened)", err)
			}
			return fmt.Errorf("send clickable macOS notification: %w", err)
		}
		return nil
	}
	// AppleScript is available on every supported macOS installation, but its
	// notification API cannot attach a click target. Keep the existing
	// notification path and make the one-command fallback visible to users.
	fallback := body + " Open with: tidemux report open."
	output, err := exec.Command("osascript", "-e", fmt.Sprintf("display notification %s with title %s", appleQuote(fallback), appleQuote(subject))).CombinedOutput()
	if err != nil {
		if notificationPermissionError(output) {
			if settingsErr := openMacOSNotificationSettings(); settingsErr != nil {
				return fmt.Errorf("send macOS notification: %w (could not open notification settings: %v)", err, settingsErr)
			}
			return fmt.Errorf("send macOS notification: %w (notification settings opened)", err)
		}
		return fmt.Errorf("send macOS notification: %w", err)
	}
	return nil
}

func notificationPermissionError(output []byte) bool {
	message := strings.ToLower(string(output))
	return strings.Contains(message, "not allowed") || strings.Contains(message, "not authorized") || strings.Contains(message, "permission")
}

func openMacOSNotificationSettings() error {
	return exec.Command("open", macOSNotificationSettingsURL).Run()
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

func appleQuote(s string) string { return "\"" + strings.ReplaceAll(s, "\"", "\\\"") + "\"" }
