package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/hs3180/tidemux/internal/ledger"
)

type reportLocale string

const (
	reportLocaleEnglish            reportLocale = "en"
	reportLocaleSimplifiedChinese  reportLocale = "zh-Hans"
	reportLocaleTraditionalChinese reportLocale = "zh-Hant"
)

type reportMessages struct {
	subjectFormat        string
	title                string
	dateFormat           string
	dateTimezoneFormat   string
	requestsFormat       string
	tokensFormat         string
	costFormat           string
	unknownCostsFormat   string
	dataChecksTitle      string
	tokenMismatchFormat  string
	unmatchedLinesFormat string
	unknownValue         string
}

var reportCatalog = map[reportLocale]reportMessages{
	reportLocaleEnglish: {
		subjectFormat:        "TideMux %s usage report",
		title:                "📊 TideMux Daily Usage",
		dateFormat:           "Date: %s",
		dateTimezoneFormat:   "Date: %s · Timezone: %s",
		requestsFormat:       "Requests: %s total · Failures: %s",
		tokensFormat:         "Tokens: %s input · %s output",
		costFormat:           "Local estimated cost: %s",
		unknownCostsFormat:   "Requests with unknown cost: %s",
		dataChecksTitle:      "Data checks",
		tokenMismatchFormat:  "Token count mismatches: %s",
		unmatchedLinesFormat: "Unmatched statement lines: %s",
		unknownValue:         "unknown",
	},
	reportLocaleSimplifiedChinese: {
		subjectFormat:        "TideMux %s 用量日报",
		title:                "📊 TideMux 每日用量",
		dateFormat:           "日期：%s",
		dateTimezoneFormat:   "日期：%s · 时区：%s",
		requestsFormat:       "请求：%s 次（失败 %s 次）",
		tokensFormat:         "Token：输入 %s · 输出 %s",
		costFormat:           "本地估算费用：%s",
		unknownCostsFormat:   "未知费用请求：%s 次",
		dataChecksTitle:      "数据校验",
		tokenMismatchFormat:  "Token 统计差异：%s",
		unmatchedLinesFormat: "未匹配账单行：%s",
		unknownValue:         "未知",
	},
	reportLocaleTraditionalChinese: {
		subjectFormat:        "TideMux %s 用量日報",
		title:                "📊 TideMux 每日用量",
		dateFormat:           "日期：%s",
		dateTimezoneFormat:   "日期：%s · 時區：%s",
		requestsFormat:       "請求：%s 次（失敗 %s 次）",
		tokensFormat:         "Token：輸入 %s · 輸出 %s",
		costFormat:           "本地估算費用：%s",
		unknownCostsFormat:   "未知費用請求：%s 次",
		dataChecksTitle:      "資料校驗",
		tokenMismatchFormat:  "Token 統計差異：%s",
		unmatchedLinesFormat: "未匹配帳單行：%s",
		unknownValue:         "未知",
	},
}

func detectReportLocale() reportLocale {
	if runtime.GOOS == "darwin" {
		if output, err := exec.Command("/usr/bin/defaults", "read", "-g", "AppleLanguages").Output(); err == nil {
			if locale, ok := reportLocaleFromAppleLanguages(string(output)); ok {
				return locale
			}
		}
	}
	if locale, ok := reportLocaleFromEnvironment(os.Getenv); ok {
		return locale
	}
	return reportLocaleEnglish
}

func reportLocaleFromAppleLanguages(output string) (reportLocale, bool) {
	locales := make([]string, 0, 2)
	for _, line := range strings.Split(output, "\n") {
		start := strings.IndexByte(line, '"')
		if start < 0 {
			continue
		}
		end := strings.IndexByte(line[start+1:], '"')
		if end < 0 {
			continue
		}
		locales = append(locales, line[start+1:start+1+end])
	}
	return preferredReportLocale(locales)
}

func reportLocaleFromEnvironment(getenv func(string) string) (reportLocale, bool) {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES"} {
		if value := strings.TrimSpace(getenv(key)); value != "" {
			if locale, ok := reportLocaleForTag(value); ok {
				return locale, true
			}
			return reportLocaleEnglish, true
		}
	}
	if value := strings.TrimSpace(getenv("LANGUAGE")); value != "" {
		locales := strings.Split(value, ":")
		if locale, ok := preferredReportLocale(locales); ok {
			return locale, true
		}
		return reportLocaleEnglish, true
	}
	if value := strings.TrimSpace(getenv("LANG")); value != "" {
		if locale, ok := reportLocaleForTag(value); ok {
			return locale, true
		}
		return reportLocaleEnglish, true
	}
	return "", false
}

func preferredReportLocale(locales []string) (reportLocale, bool) {
	for _, value := range locales {
		if locale, ok := reportLocaleForTag(value); ok {
			return locale, true
		}
	}
	return "", false
}

func reportLocaleForTag(value string) (reportLocale, bool) {
	value = strings.TrimSpace(value)
	value = strings.SplitN(value, ".", 2)[0]
	value = strings.SplitN(value, "@", 2)[0]
	value = strings.ReplaceAll(value, "_", "-")
	parts := strings.Split(value, "-")
	if len(parts) == 0 {
		return "", false
	}
	switch strings.ToLower(parts[0]) {
	case "en", "c", "posix":
		return reportLocaleEnglish, true
	case "zh":
		for _, part := range parts[1:] {
			if strings.EqualFold(part, "Hant") {
				return reportLocaleTraditionalChinese, true
			}
			if strings.EqualFold(part, "Hans") {
				return reportLocaleSimplifiedChinese, true
			}
		}
		for _, part := range parts[1:] {
			switch strings.ToUpper(part) {
			case "TW", "HK", "MO":
				return reportLocaleTraditionalChinese, true
			case "CN", "SG":
				return reportLocaleSimplifiedChinese, true
			}
		}
		return reportLocaleSimplifiedChinese, true
	default:
		return "", false
	}
}

func reportMessagesFor(locale reportLocale) reportMessages {
	if messages, ok := reportCatalog[locale]; ok {
		return messages
	}
	return reportCatalog[reportLocaleEnglish]
}

func reportTextForLocale(r ledger.DailyReport, locale reportLocale) string {
	messages := reportMessagesFor(locale)
	cost := reportCostText(r)
	if cost == "unknown" {
		cost = messages.unknownValue
	}
	date := fmt.Sprintf(messages.dateFormat, r.Day)
	if timezone := strings.TrimSpace(r.Timezone); timezone != "" {
		date = fmt.Sprintf(messages.dateTimezoneFormat, r.Day, timezone)
	}
	lines := []string{
		messages.title,
		date,
		"",
		fmt.Sprintf(messages.requestsFormat, reportCountText(r.RequestCount), reportCountText(r.FailureCount)),
		fmt.Sprintf(messages.tokensFormat, reportCountText(r.InputTokens), reportCountText(r.OutputTokens)),
		fmt.Sprintf(messages.costFormat, cost),
		fmt.Sprintf(messages.unknownCostsFormat, reportCountText(r.UnknownCostRequests)),
	}
	checks := make([]string, 0, 2)
	if r.TokenizerMismatches != nil {
		checks = append(checks, fmt.Sprintf(messages.tokenMismatchFormat, reportCountText(*r.TokenizerMismatches)))
	}
	if r.UnmatchedStatements != nil {
		checks = append(checks, fmt.Sprintf(messages.unmatchedLinesFormat, reportCountText(*r.UnmatchedStatements)))
	}
	if len(checks) > 0 {
		lines = append(lines, "", messages.dataChecksTitle)
		lines = append(lines, checks...)
	}
	return strings.Join(lines, "\n")
}

func reportCountText(value int64) string {
	digits := strconv.FormatInt(value, 10)
	sign := 0
	if strings.HasPrefix(digits, "-") {
		sign = 1
	}
	var formatted strings.Builder
	formatted.Grow(len(digits) + (len(digits)-sign-1)/3)
	for i := range len(digits) {
		if i > sign && (len(digits)-i)%3 == 0 {
			formatted.WriteByte(',')
		}
		formatted.WriteByte(digits[i])
	}
	return formatted.String()
}

func reportCostText(r ledger.DailyReport) string {
	if r.RequestCount == 0 {
		return "0"
	}
	if r.EstimatedCost == nil {
		return "unknown"
	}
	cost := fmt.Sprintf("%.6f", *r.EstimatedCost)
	if r.Currency != "" {
		cost += " " + r.Currency
	}
	return cost
}
