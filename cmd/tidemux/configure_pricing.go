package main

import (
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

func configurePrices(flags *flag.FlagSet, baseURL, model, currency, source, version string, inputCacheHit, inputCacheMiss, output float64) (map[string]adapter.Price, error) {
	prices := map[string]adapter.Price{}
	if price, ok := adapter.BuiltInPrice(baseURL, model, time.Now()); ok {
		prices[model] = price
	}
	pricingFlags := []string{"pricing-currency", "pricing-source", "pricing-version", "pricing-input-cache-hit", "pricing-input-cache-miss", "pricing-output"}
	if !flagWasSet(flags, pricingFlags...) {
		if len(prices) == 0 {
			return nil, errors.New("pricing is required; use --pricing-input-cache-hit, --pricing-input-cache-miss and --pricing-output")
		}
		return prices, nil
	}
	if !flagWasSet(flags, "pricing-input-cache-hit") || !flagWasSet(flags, "pricing-input-cache-miss") || !flagWasSet(flags, "pricing-output") {
		return nil, errors.New("custom pricing requires --pricing-input-cache-hit, --pricing-input-cache-miss and --pricing-output")
	}
	price := adapter.Price{
		Currency:       currency,
		Source:         source,
		Version:        version,
		InputCacheHit:  &inputCacheHit,
		InputCacheMiss: &inputCacheMiss,
		Output:         &output,
	}
	if err := price.Validate(); err != nil {
		return nil, fmt.Errorf("invalid custom pricing: %w", err)
	}
	prices[model] = price
	return prices, nil
}

func configureWizardPrices(flags *flag.FlagSet, baseURL, model, currency, source, version string, inputCacheHit, inputCacheMiss, output float64) (map[string]adapter.Price, error) {
	pricingFlags := []string{"pricing-currency", "pricing-source", "pricing-version", "pricing-input-cache-hit", "pricing-input-cache-miss", "pricing-output"}
	if flagWasSet(flags, pricingFlags...) {
		return configurePrices(flags, baseURL, model, currency, source, version, inputCacheHit, inputCacheMiss, output)
	}
	if price, ok := adapter.BuiltInPrice(baseURL, model, time.Now()); ok {
		return map[string]adapter.Price{model: price}, nil
	}
	return map[string]adapter.Price{}, nil
}
