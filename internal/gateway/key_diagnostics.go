package gateway

import (
	"crypto/rand"
	"encoding/hex"
	"math"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

const keyDecisionLimit = 32
const keyDecisionTTL = time.Hour

var fallbackKeyLabel atomic.Uint64

func assignedKeyLabel() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return "key-" + hex.EncodeToString(value[:])
	}
	// Diagnostics must not stop routing when entropy is unavailable. The
	// fallback is an assigned process-local sequence, never secret-derived.
	return "key-local-" + strconv.FormatUint(fallbackKeyLabel.Add(1), 16)
}

type KeyCounters struct {
	Requests      uint64 `json:"requests"`
	HTTPAttempts  uint64 `json:"http_attempts"`
	Successes     uint64 `json:"successes"`
	Failures      uint64 `json:"failures"`
	Cancellations uint64 `json:"cancellations"`
	Timeouts      uint64 `json:"timeouts"`
	InFlight      uint64 `json:"in_flight"`
}

func increment(counter *uint64) {
	if *counter < math.MaxUint64 {
		*counter++
	}
}

type KeyStatus struct {
	Label            string       `json:"label"`
	State            string       `json:"state"`
	LastFailureClass string       `json:"last_failure_class,omitempty"`
	LastFailureAt    time.Time    `json:"last_failure_at,omitzero"`
	CooldownUntil    time.Time    `json:"cooldown_until,omitzero"`
	Counters         *KeyCounters `json:"counters,omitempty"`
}

type KeyFailover struct {
	Timestamp time.Time `json:"timestamp"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Reason    string    `json:"reason"`
}

type KeyPoolStatus struct {
	Capacity         int           `json:"capacity"`
	Eligible         int           `json:"eligible"`
	Cooling          int           `json:"cooling"`
	TelemetryEnabled bool          `json:"telemetry_enabled"`
	CounterEpoch     uint64        `json:"counter_epoch"`
	Keys             []KeyStatus   `json:"keys"`
	RecentFailovers  []KeyFailover `json:"recent_failovers"`
	RecentLimit      int           `json:"recent_limit"`
	RecentTTLSeconds int           `json:"recent_ttl_seconds"`
}

func (c Config) HealthDiagnosticsEnabled() bool {
	return c.HealthDiagnostics == nil || *c.HealthDiagnostics
}

func (p *providerKeyPool) initializeDiagnostics() {
	p.labels = make([]string, len(p.keys))
	p.failureClasses = make([]string, len(p.keys))
	p.failureTimes = make([]time.Time, len(p.keys))
	for i := range p.labels {
		p.labels[i] = assignedKeyLabel()
	}
	p.setDiagnostics(true)
}

func (p *providerKeyPool) setDiagnostics(enabled bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.telemetryEnabled == enabled {
		return
	}
	p.telemetryEnabled = enabled
	increment(&p.counterEpoch)
	p.counters = nil
	p.decisions = nil
	if enabled {
		p.counters = make([]*KeyCounters, len(p.keys))
		for i := range p.counters {
			p.counters[i] = &KeyCounters{}
		}
	}
}

func (p *providerKeyPool) beginAttempt(index int, first bool, from int, reason string, now time.Time) func(adapter.HTTPAttemptResult) {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.telemetryEnabled || index < 0 || index >= len(p.counters) {
		return nil
	}
	counters := p.counters[index]
	increment(&counters.HTTPAttempts)
	increment(&counters.InFlight)
	if first {
		increment(&counters.Requests)
	}
	if from >= 0 && from != index && from < len(p.labels) {
		p.pruneDecisions(now)
		if len(p.decisions) == keyDecisionLimit {
			copy(p.decisions, p.decisions[1:])
			p.decisions = p.decisions[:keyDecisionLimit-1]
		}
		p.decisions = append(p.decisions, KeyFailover{now, p.labels[from], p.labels[index], reason})
	}
	return func(result adapter.HTTPAttemptResult) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if !p.telemetryEnabled || p.counters[index] != counters {
			return
		}
		if counters.InFlight > 0 {
			counters.InFlight--
		}
		switch result.Outcome {
		case "success":
			increment(&counters.Successes)
		case "canceled":
			increment(&counters.Cancellations)
		case "timeout":
			increment(&counters.Timeouts)
		default:
			increment(&counters.Failures)
		}
		if result.Outcome != "success" {
			p.failureClasses[index], p.failureTimes[index] = result.FailureClass, time.Now()
		}
	}
}

func (p *providerKeyPool) pruneDecisions(now time.Time) {
	first := 0
	for first < len(p.decisions) && now.Sub(p.decisions[first].Timestamp) >= keyDecisionTTL {
		first++
	}
	if first > 0 {
		p.decisions = append([]KeyFailover(nil), p.decisions[first:]...)
	}
}

func (p *providerKeyPool) diagnosticStatus(now time.Time) *KeyPoolStatus {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pruneDecisions(now)
	status := &KeyPoolStatus{Capacity: len(p.keys), TelemetryEnabled: p.telemetryEnabled, CounterEpoch: p.counterEpoch,
		Keys: []KeyStatus{}, RecentFailovers: append([]KeyFailover{}, p.decisions...), RecentLimit: keyDecisionLimit, RecentTTLSeconds: int(keyDecisionTTL / time.Second)}
	for i, label := range p.labels {
		key := KeyStatus{Label: label, State: "eligible", LastFailureClass: p.failureClasses[i], LastFailureAt: p.failureTimes[i]}
		if p.cooldowns[i].After(now) {
			key.State, key.CooldownUntil = "cooling", p.cooldowns[i]
			status.Cooling++
		} else {
			status.Eligible++
		}
		if p.telemetryEnabled {
			copy := *p.counters[i]
			key.Counters = &copy
		}
		status.Keys = append(status.Keys, key)
	}
	return status
}
