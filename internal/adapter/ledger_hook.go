// Package adapter contains the provider-boundary hooks used by TideMux.
package adapter

import (
	"context"
	"errors"
	"fmt"

	"github.com/hs3180/tidemux/internal/ledger"
)

// Usage is the normalized usage payload returned by an OpenAI-compatible
// provider. Cache fields are optional because providers may omit them.
type Usage struct {
	PromptTokens        int64
	CompletionTokens    int64
	CacheHitTokens      *int64
	CacheMissTokens     *int64
	FirstTokenLatencyMS *int64
	TotalLatencyMS      *int64
}

// Record is the adapter-boundary data needed to append an immutable ledger
// row. Provider protocol parsing stays outside this package.
type Record struct {
	TimestampMS      int64
	Model            string
	Status           string
	Usage            Usage
	PriceInPerMTok   float64
	PriceOutPerMTok  float64
	PriceHitPerMTok  float64
	PriceMissPerMTok float64
	Shaping          *string
	ErrorCode        *string
	ErrorReason      *string
	Events           []ledger.Event
}

// AppendLedger writes the request row first, then its explainable control
// events. It is intentionally a thin hook: it records decisions but does not
// change routing, limits, or provider behavior.
func AppendLedger(ctx context.Context, l *ledger.Ledger, r Record) (int64, error) {
	if l == nil {
		return 0, errors.New("ledger hook requires a ledger")
	}
	if r.Usage.PromptTokens < 0 || r.Usage.CompletionTokens < 0 {
		return 0, errors.New("adapter usage tokens must be non-negative")
	}
	if err := validateCache(r.Usage); err != nil {
		return 0, err
	}

	cacheHit, cacheMiss := r.Usage.CacheHitTokens, r.Usage.CacheMissTokens
	inputCost := float64(r.Usage.PromptTokens) * r.PriceInPerMTok
	if cacheHit != nil || cacheMiss != nil {
		inputCost = 0
		if cacheHit != nil {
			inputCost += float64(*cacheHit) * r.PriceHitPerMTok
		}
		if cacheMiss != nil {
			inputCost += float64(*cacheMiss) * r.PriceMissPerMTok
		}
	}
	cost := (inputCost + float64(r.Usage.CompletionTokens)*r.PriceOutPerMTok) / 1_000_000

	id, err := l.AppendRequest(ctx, ledger.Request{
		TimestampMS: r.TimestampMS, Model: r.Model, Status: r.Status,
		PromptTokens: r.Usage.PromptTokens, CompletionTokens: r.Usage.CompletionTokens,
		TotalTokens:    r.Usage.PromptTokens + r.Usage.CompletionTokens,
		CacheHitTokens: cacheHit, CacheMissTokens: cacheMiss,
		PriceInPerMTok: r.PriceInPerMTok, PriceOutPerMTok: r.PriceOutPerMTok,
		PriceHitPerMTok: r.PriceHitPerMTok, PriceMissPerMTok: r.PriceMissPerMTok,
		EstimatedCostCNY: cost, Shaping: r.Shaping, ErrorCode: r.ErrorCode,
		ErrorReason: r.ErrorReason, FirstTokenLatencyMS: r.Usage.FirstTokenLatencyMS,
		TotalLatencyMS: r.Usage.TotalLatencyMS,
	})
	if err != nil {
		return 0, fmt.Errorf("record adapter request: %w", err)
	}
	for _, event := range r.Events {
		event.RequestID = &id
		if _, err := l.AppendEvent(ctx, event); err != nil {
			return id, fmt.Errorf("record adapter event: %w", err)
		}
	}
	return id, nil
}

func validateCache(u Usage) error {
	if u.CacheHitTokens == nil && u.CacheMissTokens == nil {
		return nil
	}
	hit, miss := int64(0), int64(0)
	if u.CacheHitTokens != nil {
		hit = *u.CacheHitTokens
	}
	if u.CacheMissTokens != nil {
		miss = *u.CacheMissTokens
	}
	if hit < 0 || miss < 0 || hit+miss > u.PromptTokens {
		return errors.New("adapter cache tokens are inconsistent with prompt tokens")
	}
	return nil
}
