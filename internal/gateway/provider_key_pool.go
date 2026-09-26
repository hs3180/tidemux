package gateway

import (
	"sync"
	"time"
)

// providerKeyPool orders credentials for one provider profile and tracks
// per-key cooldowns. A request receives one candidate sequence; the adapter
// keeps each attempted key bound for that attempt and never switches after
// downstream stream output begins.
type providerKeyPool struct {
	keys      []string
	next      int
	cooldowns []time.Time
	mu        sync.Mutex
}

type providerKeyCandidate struct {
	index int
	key   string
}

func newProviderKeyPool(keys []string) *providerKeyPool {
	if len(keys) == 0 {
		return nil
	}
	return &providerKeyPool{keys: append([]string(nil), keys...), cooldowns: make([]time.Time, len(keys))}
}

func (p *providerKeyPool) hasMultipleKeys() bool {
	return p != nil && len(p.keys) > 1
}

func (p *providerKeyPool) Next() string {
	candidates, _ := p.Candidates()
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0].key
}

// Candidates returns healthy keys in round-robin attempt order. The first key
// is the normal selection; later keys are available only for safe pre-stream
// failover within the same provider profile.
func (p *providerKeyPool) Candidates() ([]providerKeyCandidate, time.Duration) {
	return p.CandidatesAt(time.Now())
}

func (p *providerKeyPool) CandidatesAt(now time.Time) ([]providerKeyCandidate, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.keys) == 0 {
		return nil, 0
	}
	start := p.next % len(p.keys)
	p.next = (start + 1) % len(p.keys)
	result := make([]providerKeyCandidate, 0, len(p.keys))
	var earliest time.Time
	for offset := 0; offset < len(p.keys); offset++ {
		index := (start + offset) % len(p.keys)
		until := p.cooldowns[index]
		if until.After(now) {
			if earliest.IsZero() || until.Before(earliest) {
				earliest = until
			}
			continue
		}
		p.cooldowns[index] = time.Time{}
		result = append(result, providerKeyCandidate{index: index, key: p.keys[index]})
	}
	if len(result) > 0 || earliest.IsZero() {
		return result, 0
	}
	return nil, earliest.Sub(now)
}

func (p *providerKeyPool) Cooldown(index int, delay time.Duration) {
	p.CooldownAt(index, delay, time.Now())
}

func (p *providerKeyPool) CooldownAt(index int, delay time.Duration, now time.Time) {
	if p == nil || index < 0 || index >= len(p.keys) {
		return
	}
	if delay < time.Second {
		delay = time.Second
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	until := now.Add(delay)
	p.mu.Lock()
	if until.After(p.cooldowns[index]) {
		p.cooldowns[index] = until
	}
	p.mu.Unlock()
}

// CandidateReadyAt rechecks a request's failover snapshot immediately before
// trying a later key, so concurrent failures can take effect without waiting
// for a new request to rebuild its candidate list.
func (p *providerKeyPool) CandidateReadyAt(index int, now time.Time) (bool, time.Duration) {
	if p == nil || index < 0 || index >= len(p.keys) {
		return false, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	until := p.cooldowns[index]
	if until.After(now) {
		return false, until.Sub(now)
	}
	p.cooldowns[index] = time.Time{}
	return true, 0
}

// CooldownWaitAt reports the shortest remaining cooldown across the pool, or
// zero when at least one key is currently eligible.
func (p *providerKeyPool) CooldownWaitAt(now time.Time) time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var earliest time.Time
	for _, until := range p.cooldowns {
		if !until.After(now) {
			return 0
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	if earliest.IsZero() {
		return 0
	}
	return earliest.Sub(now)
}
