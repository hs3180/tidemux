package adapter

import (
	"net/url"
	"strings"
	"time"
)

const deepSeekPricingSource = "https://api-docs.deepseek.com/quick_start/pricing/"

// BuiltInPrice returns the verified DeepSeek API price for the request start
// time. It is deliberately limited to DeepSeek's official hostname; compatible
// third-party endpoints must provide an explicit price in configuration.
func BuiltInPrice(baseURL, model string, at time.Time) (Price, bool) {
	u, err := url.Parse(baseURL)
	if err != nil || strings.ToLower(u.Hostname()) != "api.deepseek.com" {
		return Price{}, false
	}
	switch model {
	case "deepseek-flash", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp":
	default:
		return Price{}, false
	}
	peak := deepSeekPeak(at.UTC())
	version := "deepseek-v4-pricing-2026-08-16-off-peak"
	input, output, cacheRead := 0.15, 0.60, 0.003
	if peak {
		version = "deepseek-v4-pricing-2026-08-16-peak"
		input, output, cacheRead = 0.30, 1.20, 0.006
	}
	return Price{
		Currency:  "USD",
		Source:    deepSeekPricingSource,
		Version:   version,
		Input:     floatPtr(input),
		Output:    floatPtr(output),
		CacheRead: floatPtr(cacheRead),
	}, true
}

func deepSeekPeak(t time.Time) bool {
	weekday := t.Weekday() >= time.Monday && t.Weekday() <= time.Friday
	if !weekday {
		return false
	}
	hour := t.Hour()
	return hour >= 1 && hour < 4 || hour >= 6 && hour < 10
}

func floatPtr(v float64) *float64 { return &v }
