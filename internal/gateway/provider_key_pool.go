package gateway

import (
	"sync"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

// providerKeyPool orders credentials for one provider profile and tracks
// per-key cooldowns. A request receives one candidate sequence; the adapter
// keeps each attempted key bound for that attempt and never switches after
// downstream stream output begins.
type providerKeyPool struct {
	labels            []string
	failureClasses    []string
	failureTimes      []time.Time
	counters          []*KeyCounters
	telemetryEnabled  bool
	counterEpoch      uint64
	decisions         []KeyFailover
	keys              []string
	next              int
	cooldowns         []time.Time
	rateFailures      []uint16
	rateVersions      []uint64
	rateStreak        uint16
	rateVersion       uint64
	rateProviderUntil time.Time
	providerRetry     *providerRetryState
	lastRateLimit     time.Time
	rateProbe         *rateRecoveryLease
	jitter            func(time.Duration) time.Duration
	mu                sync.Mutex
}

type providerKeyCandidate struct {
	index int
	key   string
}

func newProviderKeyPool(keys []string) *providerKeyPool {
	if len(keys) == 0 {
		return nil
	}
	pool := &providerKeyPool{keys: append([]string(nil), keys...), cooldowns: make([]time.Time, len(keys)),
		rateFailures: make([]uint16, len(keys)), rateVersions: make([]uint64, len(keys)), jitter: recoveryJitter,
		providerRetry: &providerRetryState{}}
	pool.initializeDiagnostics()
	return pool
}

func (p *providerKeyPool) Next() string {
	candidates, _ := p.Candidates()
	if len(candidates) == 0 {
		return ""
	}
	return candidates[0].key
}

// Candidates returns healthy keys in round-robin selection order. Each
// downstream request uses one selected key; other candidates remain available
// to subsequent downstream requests.
func (p *providerKeyPool) Candidates() ([]providerKeyCandidate, time.Duration) {
	return p.CandidatesAt(time.Now())
}

func (p *providerKeyPool) CandidatesAt(now time.Time) ([]providerKeyCandidate, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.keys) == 0 {
		return nil, 0
	}
	if server := p.providerRetry.snapshot(); server.until.After(now) {
		return nil, server.until.Sub(now)
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

func (p *providerKeyPool) Cooldown(index int, delay time.Duration, reason ...string) {
	p.CooldownAt(index, delay, time.Now(), reason...)
}

func (p *providerKeyPool) CooldownAt(index int, delay time.Duration, now time.Time, reason ...string) {
	if p == nil || index < 0 || index >= len(p.keys) {
		return
	}
	if delay < time.Second {
		delay = time.Second
	}
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	until := now.Add(delay)
	p.mu.Lock()
	if until.After(p.cooldowns[index]) {
		p.cooldowns[index] = until
	}
	if len(reason) > 0 {
		p.failureClasses[index], p.failureTimes[index] = reason[0], now
	}
	p.mu.Unlock()
}

func (p *providerKeyPool) CooldownAll(delay time.Duration) {
	if p == nil || len(p.keys) == 0 {
		return
	}
	now := time.Now()
	if delay < time.Second {
		delay = time.Second
	}
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	until := now.Add(delay)
	p.mu.Lock()
	for index := range p.cooldowns {
		if until.After(p.cooldowns[index]) {
			p.cooldowns[index] = until
		}
	}
	p.mu.Unlock()
}

// CandidateReadyAt rechecks a selected key immediately before dispatch so a
// concurrent cooldown can take effect after route selection and queueing.
func (p *providerKeyPool) CandidateReadyAt(index int, now time.Time) (bool, time.Duration) {
	ready, delay, _ := p.candidateStateAt(index, now)
	return ready, delay
}

type rateLimitObservation struct{ keyVersion, providerVersion, serverVersion uint64 }

func (p *providerKeyPool) candidateStateAt(index int, now time.Time) (bool, time.Duration, rateLimitObservation) {
	if p == nil || index < 0 || index >= len(p.keys) {
		return false, 0, rateLimitObservation{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	until := p.cooldowns[index]
	server := p.providerRetry.snapshot()
	if server.until.After(until) {
		until = server.until
	}
	observation := rateLimitObservation{p.rateVersions[index], p.rateVersion, server.version}
	if until.After(now) {
		return false, until.Sub(now), observation
	}
	p.cooldowns[index] = time.Time{}
	return true, 0, observation
}

// CooldownWaitAt reports the shortest remaining cooldown across the pool, or
// zero when at least one key is currently eligible.
func (p *providerKeyPool) CooldownWaitAt(now time.Time) time.Duration {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	server := p.providerRetry.snapshot()
	if p.rateProbe != nil || server.probe != nil {
		return time.Second
	}
	delay := p.cooldownWaitLocked(now)
	for _, until := range []time.Time{p.rateProviderUntil, server.until} {
		if remaining := until.Sub(now); remaining > delay {
			delay = remaining
		}
	}
	return delay
}

// A server-supplied wait applies to the provider, independent of which key or
// model returned it. Version fencing prevents an older success from erasing it.
func (p *providerKeyPool) holdProviderUntil(until time.Time) time.Time {
	if p == nil || until.IsZero() {
		return time.Time{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	server := p.providerRetry
	server.mu.Lock()
	defer server.mu.Unlock()
	increment(&p.rateVersion)
	increment(&server.version)
	if until.After(server.until) {
		server.until = until
	}
	return server.until
}

func (p *providerKeyPool) cooldownWaitLocked(now time.Time) time.Duration {
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

// Operational rate-limit state is independent of optional telemetry. Each
// actual rejected attempt advances the streak; no local rejection advances it.
func (p *providerKeyPool) rateLimitedAt(index int, upstream time.Duration, provided bool, now time.Time) (keyDelay, providerDelay time.Duration, streak uint16) {
	if p == nil || index < 0 || index >= len(p.keys) {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rateFailures[index] < ^uint16(0) {
		p.rateFailures[index]++
	}
	if p.rateStreak < ^uint16(0) {
		p.rateStreak++
	}
	increment(&p.rateVersions[index])
	increment(&p.rateVersion)
	keyDelay = upstream
	if !provided {
		keyDelay = boundedRecoveryBackoff("rate_limited", p.rateFailures[index], upstream, p.jitter)
	}
	if keyDelay < time.Second {
		keyDelay = time.Second
	}
	if keyDelay > 24*time.Hour || p.rateFailures[index] > 10 {
		keyDelay = 24 * time.Hour
	}
	until := now.Add(keyDelay)
	if until.After(p.cooldowns[index]) {
		p.cooldowns[index] = until
	}
	keyDelay = p.cooldowns[index].Sub(now)
	p.failureClasses[index], p.failureTimes[index] = "rate_limited", now
	p.lastRateLimit = now
	providerDelay = p.cooldownWaitLocked(now)
	if providerDelay > 0 && p.rateStreak > 10 {
		until = now.Add(24 * time.Hour)
		for i := range p.cooldowns {
			if until.After(p.cooldowns[i]) {
				p.cooldowns[i] = until
			}
		}
		providerDelay = 24 * time.Hour
	}
	if providerDelay > 0 {
		p.rateProviderUntil = now.Add(providerDelay)
	}
	return keyDelay, providerDelay, p.rateStreak
}

// An older successful request cannot erase a rate limit observed after its
// dispatch, including one on another key in the same provider.
func (p *providerKeyPool) rateSucceeded(index int, observation rateLimitObservation) bool {
	if p == nil || index < 0 || index >= len(p.keys) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rateVersions[index] == observation.keyVersion {
		p.rateFailures[index] = 0
	}
	if p.rateVersion != observation.providerVersion {
		return false
	}
	server := p.providerRetry
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.version != observation.serverVersion {
		return false
	}
	p.rateStreak = 0
	p.rateProviderUntil = time.Time{}
	if !server.until.IsZero() {
		server.until = time.Time{}
		increment(&server.version)
	}
	return true
}

type rateRecoveryLease struct {
	marker byte
	server *providerRetryState
}

// Keep the single recovery claim with the bounded key pool as well as the
// availability cache. The latter may evict an idle target after a 24h silence;
// that must not admit a herd of first requests when the silence ends.
func (p *providerKeyPool) beginRateRecovery(now time.Time) (*rateRecoveryLease, *adapter.CallError) {
	if p == nil {
		return nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	server := p.providerRetry
	server.mu.Lock()
	defer server.mu.Unlock()
	if p.rateProbe != nil || server.probe != nil {
		return nil, &adapter.CallError{Status: 429, Code: "recovery_in_progress", UpstreamNotAttempted: true}
	}
	if (p.rateStreak == 0 || p.rateProviderUntil.IsZero()) && server.until.IsZero() {
		return nil, nil
	}
	until := p.rateProviderUntil
	if server.until.After(until) {
		until = server.until
	}
	if until.After(now) {
		code := "provider_rate_limited_cooling_down"
		if server.until.After(now) {
			code = "provider_retry_after_cooling_down"
		}
		return nil, &adapter.CallError{Status: 429, Code: code, Cooldown: until.Sub(now), CooldownUntil: until, UpstreamNotAttempted: true}
	}
	lease := &rateRecoveryLease{}
	p.rateProbe = lease
	if !server.until.IsZero() {
		lease.server = server
		server.probe = lease
	}
	return lease, nil
}

func (p *providerKeyPool) finishRateRecovery(lease *rateRecoveryLease) {
	if p == nil || lease == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rateProbe == lease {
		p.rateProbe = nil
		if lease.server != nil {
			lease.server.mu.Lock()
			if lease.server.probe == lease {
				lease.server.probe = nil
			}
			lease.server.mu.Unlock()
		}
	}
}
