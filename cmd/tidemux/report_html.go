package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"html/template"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
)

//go:embed report.html.tmpl
var reportHTMLTemplate string

//go:embed report.css
var reportCSS string

const (
	reportSVGWidth      = 960.0
	reportSVGBase       = 210.0
	reportSVGPlotHeight = 175.0
	reportSVGLeft       = 48.0
	reportSVGRight      = 12.0
)

type reportExportResult struct {
	Path     string `json:"path"`
	Days     int    `json:"days"`
	StartDay string `json:"start_day"`
	EndDay   string `json:"end_day"`
	Timezone string `json:"timezone"`
}

func defaultReportPath(ledgerPath string) string {
	return filepath.Join(filepath.Dir(ledgerPath), "reports", "latest.html")
}

type reportHTMLModel struct {
	GeneratedAt          string
	StartDay             string
	EndDay               string
	Timezone             string
	TotalRequests        int64
	TotalFailures        int64
	TotalInputTokens     int64
	TotalOutputTokens    int64
	UnknownCostRequests  int64
	EstimatedCost        string
	Currency             string
	HasUnknownCost       bool
	CostChartAvailable   bool
	CostChartDescription string
	RequestMaxLabel      string
	CostMaxLabel         string
	RequestBars          []reportHTMLBar
	CostBars             []reportHTMLBar
	Rows                 []reportHTMLRow
}

type reportHTMLRow struct {
	Day                 string
	RequestCount        int64
	FailureCount        int64
	InputTokens         int64
	OutputTokens        int64
	EstimatedCost       string
	TokenizerMismatches string
	UnmatchedStatements string
	BalanceNetChange    string
	BudgetRemaining     string
	CostValue           float64
	CostKnown           bool
	Currency            string
}

type reportHTMLBar struct {
	X             float64
	Y             float64
	Width         float64
	Height        float64
	FailureY      float64
	FailureHeight float64
	Label         string
	Tooltip       string
}

func exportHTMLReport(ctx context.Context, store *ledger.Ledger, output, timezone string, through time.Time, days int, budget ledger.ReportBudget, open bool) (reportExportResult, error) {
	reports, err := store.DailyReportSeries(ctx, through, timezone, days, budget)
	if err != nil {
		return reportExportResult{}, err
	}
	model := buildReportHTMLModel(reports, time.Now())
	var body bytes.Buffer
	if err := renderReportHTML(&body, model); err != nil {
		return reportExportResult{}, err
	}
	path, err := filepath.Abs(output)
	if err != nil {
		return reportExportResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return reportExportResult{}, fmt.Errorf("create HTML report directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return reportExportResult{}, fmt.Errorf("create HTML report: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return reportExportResult{}, fmt.Errorf("protect HTML report: %w", err)
	}
	if _, err := file.Write(body.Bytes()); err != nil {
		file.Close()
		return reportExportResult{}, fmt.Errorf("write HTML report: %w", err)
	}
	if err := file.Close(); err != nil {
		return reportExportResult{}, fmt.Errorf("close HTML report: %w", err)
	}
	if open {
		if err := openHTMLReport(path); err != nil {
			return reportExportResult{}, err
		}
	}
	return reportExportResult{Path: path, Days: len(reports), StartDay: reports[0].Day, EndDay: reports[len(reports)-1].Day, Timezone: timezone}, nil
}

func openHTMLReport(output string) error {
	path, err := filepath.Abs(output)
	if err != nil {
		return fmt.Errorf("resolve HTML report path: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("HTML report not found at %s; run `tidemux report export` first", path)
		}
		return fmt.Errorf("read HTML report: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("HTML report path is not a regular file: %s", path)
	}
	if err := exec.Command("open", path).Run(); err != nil {
		return fmt.Errorf("open HTML report: %w", err)
	}
	return nil
}

func renderReportHTML(w io.Writer, model reportHTMLModel) error {
	tmpl, err := template.New("report").Parse(reportHTMLTemplate)
	if err != nil {
		return err
	}
	data := struct {
		reportHTMLModel
		CSS template.CSS
	}{
		reportHTMLModel: model,
		CSS:             template.CSS(reportCSS),
	}
	return tmpl.Execute(w, data)
}

func buildReportHTMLModel(reports []ledger.DailyReport, generatedAt time.Time) reportHTMLModel {
	model := reportHTMLModel{GeneratedAt: generatedAt.Format(time.RFC3339), Rows: make([]reportHTMLRow, 0, len(reports))}
	if len(reports) == 0 {
		return model
	}
	model.StartDay = reports[0].Day
	model.EndDay = reports[len(reports)-1].Day
	model.Timezone = reports[0].Timezone

	primaryCurrency := ""
	multipleCurrencies := false
	var totalCost float64
	knownCostRows := 0
	var maxRequests int64
	var maxCost float64
	for _, report := range reports {
		model.TotalRequests += report.RequestCount
		model.TotalFailures += report.FailureCount
		model.TotalInputTokens += report.InputTokens
		model.TotalOutputTokens += report.OutputTokens
		model.UnknownCostRequests += report.UnknownCostRequests
		if report.RequestCount > maxRequests {
			maxRequests = report.RequestCount
		}
		if report.EstimatedCost != nil && report.Currency != "" {
			if primaryCurrency == "" {
				primaryCurrency = report.Currency
			} else if primaryCurrency != report.Currency {
				multipleCurrencies = true
			}
			totalCost += *report.EstimatedCost
			knownCostRows++
			if *report.EstimatedCost > maxCost {
				maxCost = *report.EstimatedCost
			}
		}
	}
	model.Currency = primaryCurrency
	model.HasUnknownCost = model.UnknownCostRequests > 0
	if knownCostRows == 0 && model.TotalRequests == 0 {
		model.EstimatedCost = "0"
	} else if knownCostRows == 0 {
		model.EstimatedCost = "unknown"
	} else if multipleCurrencies {
		model.EstimatedCost = "multiple currencies"
		model.Currency = "mixed currencies"
	} else {
		model.EstimatedCost = fmt.Sprintf("%.6f %s", totalCost, primaryCurrency)
	}
	model.RequestMaxLabel = strconv.FormatInt(maxRequests, 10)
	model.CostChartAvailable = knownCostRows > 0 && !multipleCurrencies
	if model.CostChartAvailable {
		model.CostMaxLabel = fmt.Sprintf("%.6f %s", maxCost, primaryCurrency)
		if maxCost == 0 {
			model.CostMaxLabel = "0"
		}
	} else if multipleCurrencies {
		model.CostChartDescription = "Cost chart is hidden because the period contains multiple currencies."
	} else if model.TotalRequests == 0 {
		model.CostChartDescription = "No requests were recorded in this period."
	} else {
		model.CostChartDescription = "Cost chart is hidden because all costs are unknown."
	}

	for _, report := range reports {
		row := reportHTMLRow{
			Day:                 report.Day,
			RequestCount:        report.RequestCount,
			FailureCount:        report.FailureCount,
			InputTokens:         report.InputTokens,
			OutputTokens:        report.OutputTokens,
			TokenizerMismatches: optionalInt(report.TokenizerMismatches),
			UnmatchedStatements: optionalInt(report.UnmatchedStatements),
			BalanceNetChange:    optionalFloat(report.BalanceNetChange),
			BudgetRemaining:     optionalFloat(report.BudgetRemaining),
			Currency:            report.Currency,
		}
		if report.RequestCount == 0 {
			row.EstimatedCost = "0"
			row.CostKnown = true
		} else if report.EstimatedCost == nil || report.Currency == "" {
			row.EstimatedCost = "unknown"
		} else {
			row.EstimatedCost = fmt.Sprintf("%.6f %s", *report.EstimatedCost, report.Currency)
			row.CostValue = *report.EstimatedCost
			row.CostKnown = !multipleCurrencies && report.Currency == primaryCurrency
		}
		model.Rows = append(model.Rows, row)
	}
	model.RequestBars = buildRequestBars(reports, maxRequests)
	if model.CostChartAvailable {
		model.CostBars = buildCostBars(model.Rows, maxCost)
	}
	return model
}

func buildRequestBars(reports []ledger.DailyReport, max int64) []reportHTMLBar {
	if max < 1 {
		max = 1
	}
	bars := make([]reportHTMLBar, 0, len(reports))
	for i, report := range reports {
		x, width := reportBarPosition(i, len(reports))
		totalHeight := float64(report.RequestCount) / float64(max) * reportSVGPlotHeight
		failureHeight := float64(report.FailureCount) / float64(max) * reportSVGPlotHeight
		bars = append(bars, reportHTMLBar{
			X:             x,
			Y:             reportSVGBase - totalHeight,
			Width:         width,
			Height:        totalHeight,
			FailureY:      reportSVGBase - failureHeight,
			FailureHeight: failureHeight,
			Label:         report.Day,
			Tooltip:       fmt.Sprintf("%s: %d requests, %d failures", report.Day, report.RequestCount, report.FailureCount),
		})
	}
	return bars
}

func buildCostBars(rows []reportHTMLRow, max float64) []reportHTMLBar {
	if max <= 0 {
		max = 1
	}
	bars := make([]reportHTMLBar, 0, len(rows))
	for i, row := range rows {
		x, width := reportBarPosition(i, len(rows))
		height := 0.0
		if row.CostKnown {
			height = row.CostValue / max * reportSVGPlotHeight
		}
		bars = append(bars, reportHTMLBar{
			X:       x,
			Y:       reportSVGBase - height,
			Width:   width,
			Height:  height,
			Label:   row.Day,
			Tooltip: fmt.Sprintf("%s: %s", row.Day, row.EstimatedCost),
		})
	}
	return bars
}

func reportBarPosition(index, count int) (float64, float64) {
	if count < 1 {
		return reportSVGLeft, 0
	}
	slot := (reportSVGWidth - reportSVGLeft - reportSVGRight) / float64(count)
	width := slot * 0.72
	if width > 72 {
		width = 72
	}
	return reportSVGLeft + float64(index)*slot + (slot-width)/2, width
}

func optionalInt(value *int64) string {
	if value == nil {
		return "unknown"
	}
	return strconv.FormatInt(*value, 10)
}

func optionalFloat(value *float64) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprintf("%.6f", *value)
}
