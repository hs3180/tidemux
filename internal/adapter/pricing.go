package adapter

import (
	"net/url"
	"strings"
	"time"
)

const deepSeekPricingSource = "https://api-docs.deepseek.com/quick_start/pricing/"

// BuiltInPrice returns TideMux's verified peak DeepSeek price. It is deliberately
// limited to DeepSeek's official hostname; compatible third-party endpoints must
// provide an explicit price in configuration.
func BuiltInPrice(baseURL, model string, _ time.Time) (Price, bool) {
	u, err := url.Parse(baseURL)
	if err != nil || strings.ToLower(u.Hostname()) != "api.deepseek.com" {
		return Price{}, false
	}
	switch model {
	case "deepseek-flash", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp":
	default:
		return Price{}, false
	}
	version := "deepseek-v4-pricing-2026-08-16-peak"
	inputCacheHit, inputCacheMiss, output := 0.006, 0.30, 1.20
	return Price{
		Currency:       "USD",
		Source:         deepSeekPricingSource,
		Version:        version,
		InputCacheHit:  floatPtr(inputCacheHit),
		InputCacheMiss: floatPtr(inputCacheMiss),
		Output:         floatPtr(output),
	}, true
}

func floatPtr(v float64) *float64 { return &v }
