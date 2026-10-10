package gateway

import (
	"errors"
	"log/slog"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/observability"
)

const (
	availabilityLimit      = 4096
	availabilityTTL        = 24 * time.Hour
	availabilityMaxBackoff = 30 * time.Minute
)

type availabilityTarget struct {
	provider, model string // an empty model denotes provider scope
	generation      uint64
}

type availabilityEntry struct {
	reason                                     string
	failures                                   uint16
	lastSuccess, lastFailure, retryAt, touched time.Time
	probing                                    bool
	owner                                      *availabilityLease
}

func (e *availabilityEntry) eligible(now time.Time) bool {
	return e == nil || e.failures == 0 || !e.probing && !now.Before(e.retryAt)
}

func (e *availabilityEntry) state() string {
	if e == nil {
		return "unknown"
	}
	if e.probing {
		return "probing"
	}
	if e.failures > 0 {
		return "cooling"
	}
	return "available"
}

// Pointer identity fences earlier completions; epoch fences old config views.
// Only an actual request holds a recovery claim. No background traffic exists.
type availabilityLease struct {
	epoch   uint64
	targets []availabilityTarget
	entries []*availabilityEntry
}

type availabilityState struct {
	nextSweep   time.Time
	mu          sync.Mutex
	epoch       uint64
	generations map[string]uint64
	entries     map[availabilityTarget]*availabilityEntry
	now         func() time.Time
	jitter      func(time.Duration) time.Duration
	logger      *slog.Logger
}

func newAvailabilityState(logger *slog.Logger) *availabilityState {
	return &availabilityState{entries: map[availabilityTarget]*availabilityEntry{},
		now: time.Now, jitter: func(base time.Duration) time.Duration { return time.Duration(rand.Int63n(int64(base/5) + 1)) },
		logger: observability.LoggerOrDiscard(logger)}
}

func (s *availabilityState) activate(epoch uint64, generations map[string]uint64, valid ...func(string, string) bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.epoch = epoch
	s.generations = make(map[string]uint64, len(generations))
	for provider, generation := range generations {
		s.generations[provider] = generation
	}
	for target, entry := range s.entries {
		if generation, ok := generations[target.provider]; !ok || generation != target.generation || target.model != "" && len(valid) > 0 && !valid[0](target.provider, target.model) {
			delete(s.entries, target)
		} else {
			copy := *entry
			s.entries[target] = &copy
		}
	}
}

func safeAvailabilityModel(model string) bool {
	if model == "" || len(model) > 128 {
		return false
	}
	for _, c := range model {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._/-:@", c)) {
			return false
		}
	}
	return true
}

func (s *availabilityState) prune(now time.Time) {
	if now.Before(s.nextSweep) {
		return
	}
	s.nextSweep = now.Add(time.Minute)
	for target, entry := range s.entries {
		if !entry.probing && now.Sub(entry.touched) >= availabilityTTL {
			delete(s.entries, target)
		}
	}
}

func (s *availabilityState) put(target availabilityTarget, entry *availabilityEntry) bool {
	if _, exists := s.entries[target]; !exists && len(s.entries) >= availabilityLimit {
		var oldest availabilityTarget
		var candidate *availabilityEntry
		for key, value := range s.entries {
			if !value.probing && (candidate == nil || value.touched.Before(candidate.touched)) {
				oldest, candidate = key, value
			}
		}
		if candidate == nil {
			return false
		}
		delete(s.entries, oldest)
	}
	s.entries[target] = entry
	return true
}

func (s *availabilityState) available(epoch uint64, provider, model string, generation uint64) bool {
	if s == nil {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if epoch != s.epoch {
		return true
	}
	now := s.now()
	s.prune(now)
	return s.entries[availabilityTarget{provider, "", generation}].eligible(now) &&
		s.entries[availabilityTarget{provider, model, generation}].eligible(now)
}

func availabilityError(entry *availabilityEntry, target availabilityTarget, now time.Time) *adapter.CallError {
	code := "model_" + entry.reason + "_cooling_down"
	if target.model == "" {
		code = "provider_" + entry.reason + "_cooling_down"
	}
	if target.model != "" && entry.reason == "model_not_found" {
		code = "model_not_found_cooling_down"
	}
	if entry.probing {
		code = "recovery_in_progress"
	}
	delay := entry.retryAt.Sub(now)
	if delay < time.Second {
		delay = time.Second
	}
	return &adapter.CallError{Status: http.StatusServiceUnavailable, Code: code, Cooldown: delay, UpstreamNotAttempted: true}
}

func isAvailabilityError(code string) bool {
	return code == "provider_keys_cooling_down" || code == "recovery_in_progress" || strings.HasPrefix(code, "provider_") && strings.HasSuffix(code, "_cooling_down") ||
		strings.HasPrefix(code, "model_") && strings.HasSuffix(code, "_cooling_down")
}

func (s *availabilityState) begin(epoch uint64, provider, model string, generation uint64) (*availabilityLease, *adapter.CallError) {
	lease := &availabilityLease{epoch: epoch, targets: []availabilityTarget{{provider, "", generation}}}
	if safeAvailabilityModel(model) {
		lease.targets = append(lease.targets, availabilityTarget{provider, model, generation})
	}
	lease.entries = make([]*availabilityEntry, len(lease.targets))
	return lease, s.dispatch(lease)
}

// Recheck at the adapter's post-queue, pre-HTTP boundary. A claim held by this
// lease is accepted; a concurrent failure or a different probe is not.
func (s *availabilityState) dispatch(lease *availabilityLease) *adapter.CallError {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	var transitions []availabilityTransition
	defer func() { s.mu.Unlock(); s.logTransitions(transitions) }()
	if lease.epoch != s.epoch {
		return nil
	}
	now := s.now()
	s.prune(now)
	for i, target := range lease.targets {
		entry := s.entries[target]
		if entry != nil && entry.failures > 0 && !(entry.probing && entry == lease.entries[i]) && !entry.eligible(now) {
			return availabilityError(entry, target, now)
		}
	}
	for i, target := range lease.targets {
		entry := s.entries[target]
		lease.entries[i] = entry
		if entry != nil && entry.failures > 0 && !entry.probing {
			transitions = append(transitions, availabilityTransition{target, "cooling", "probing", entry.reason, entry.retryAt})
			entry.probing = true
			entry.owner = lease
		}
	}
	return nil
}

func availabilityFailure(err error, ctxErr error, handled bool) (modelScope bool, reason string, cooldown time.Duration) {
	if handled || ctxErr != nil {
		return
	}
	var callErr *adapter.CallError
	if !errors.As(err, &callErr) {
		return
	}
	if callErr.Code == "upstream_transport_error" && callErr.UpstreamNotAttempted && callErr.FailoverSafe {
		return false, "transport_error", callErr.Cooldown
	}
	if callErr.UpstreamNotAttempted {
		return
	}
	switch callErr.Category {
	case adapter.ProviderErrorModelNotFound:
		return true, "model_not_found", callErr.Cooldown
	case adapter.ProviderErrorTemporarilyUnavailable:
		return true, "temporarily_unavailable", callErr.Cooldown
	case adapter.ProviderErrorInsufficientBalance:
		return false, "insufficient_balance", callErr.Cooldown
	}
	return
}

func (s *availabilityState) backoff(reason string, failures uint16, upstream time.Duration) time.Duration {
	base := routeRecoveryCooldown
	if reason == "insufficient_balance" {
		base = billingExhaustionCooldown
	}
	for n := uint16(1); n < failures && base < availabilityMaxBackoff; n++ {
		base *= 2
	}
	if base > availabilityMaxBackoff {
		base = availabilityMaxBackoff
	}
	delay := base + s.jitter(base)
	if upstream > delay {
		delay = upstream
	}
	if delay > 24*time.Hour {
		delay = 24 * time.Hour
	}
	return delay
}

type availabilityTransition struct {
	target           availabilityTarget
	from, to, reason string
	retryAt          time.Time
}

func (s *availabilityState) complete(lease *availabilityLease, handled bool, err, ctxErr error) bool {
	if s == nil {
		return false
	}
	modelScope, reason, cooldown := availabilityFailure(err, ctxErr, handled)
	s.mu.Lock()
	if lease.epoch != s.epoch {
		// A compatible reload retains the old claim until its real attempt
		// ends. Releasing that resource does not confirm or renew health.
		var released []availabilityTransition
		for _, target := range lease.targets {
			if current := s.entries[target]; current != nil && current.owner == lease {
				next := *current
				next.probing, next.owner = false, nil
				s.entries[target] = &next
				released = append(released, availabilityTransition{target, "probing", "cooling", next.reason, next.retryAt})
			}
		}
		s.mu.Unlock()
		s.logTransitions(released)
		return false
	}
	for _, target := range lease.targets {
		if generation, exists := s.generations[target.provider]; !exists || generation != target.generation {
			s.mu.Unlock()
			return false
		}
	}
	now := s.now()
	s.prune(now)
	var transitions []availabilityTransition
	success := !handled && err == nil && ctxErr == nil
	for i, target := range lease.targets {
		previous := lease.entries[i]
		if s.entries[target] != previous {
			continue
		}
		fail := reason != "" && modelScope == (target.model != "")
		// An unclassified attempted probe cannot confirm recovery. Keep the
		// prior classification and extend its backoff, without broadening scope.
		if !fail && previous != nil && previous.probing && !success && !handled && ctxErr == nil {
			var callErr *adapter.CallError
			if errors.As(err, &callErr) && !callErr.UpstreamNotAttempted {
				fail = true
			}
		}
		if !success && !fail {
			if previous != nil && previous.probing {
				transitions = append(transitions, availabilityTransition{target, "probing", "cooling", previous.reason, previous.retryAt})
				previous.probing, previous.owner = false, nil
			}
			continue
		}
		next := availabilityEntry{touched: now}
		if previous != nil {
			next = *previous
			next.touched = now
			next.probing, next.owner = false, nil
		}
		if success {
			next.lastSuccess, next.reason, next.failures, next.retryAt = now, "", 0, time.Time{}
		} else {
			if reason != "" && modelScope == (target.model != "") {
				next.reason = reason
			}
			if next.failures < 16 {
				next.failures++
			}
			next.lastFailure = now
			next.retryAt = now.Add(s.backoff(next.reason, next.failures, cooldown))
		}
		if s.put(target, &next) && (previous.state() != next.state() || previous != nil && previous.reason != next.reason) {
			transitions = append(transitions, availabilityTransition{target, previous.state(), next.state(), next.reason, next.retryAt})
		}
	}
	s.mu.Unlock()
	s.logTransitions(transitions)
	return success
}

func (s *availabilityState) logTransitions(transitions []availabilityTransition) {
	for _, transition := range transitions {
		attrs := []any{slog.Int("schema_version", observability.SchemaVersion), slog.String("event", "availability_transition"),
			slog.String("provider_ref", transition.target.provider), slog.Uint64("generation", transition.target.generation),
			slog.String("from_state", transition.from), slog.String("state", transition.to)}
		if transition.target.model != "" {
			attrs = append(attrs, slog.String("model", transition.target.model))
		}
		if transition.reason != "" {
			attrs = append(attrs, slog.String("reason", transition.reason), slog.Time("retry_at", transition.retryAt))
		}
		s.logger.Info("upstream availability changed", attrs...)
	}
}

type AvailabilityStatus struct {
	State               string    `json:"state"`
	Reason              string    `json:"reason,omitempty"`
	LastSuccess         time.Time `json:"last_success_at,omitzero"`
	LastFailure         time.Time `json:"last_failure_at,omitzero"`
	RetryAt             time.Time `json:"retry_at,omitzero"`
	ProbeDue            bool      `json:"probe_due"`
	ConsecutiveFailures uint16    `json:"consecutive_failures"`
}

type ModelAvailability struct {
	Model string `json:"model"`
	AvailabilityStatus
}

type ProviderAvailability struct {
	Ref        string `json:"ref"`
	Generation uint64 `json:"generation"`
	AvailabilityStatus
	Models  []ModelAvailability `json:"models"`
	KeyPool *KeyPoolStatus      `json:"key_pool,omitempty"`
}

type AvailabilityReport struct {
	Timestamp      time.Time              `json:"timestamp"`
	Providers      []ProviderAvailability `json:"providers"`
	RecoveryMode   string                 `json:"recovery_mode"`
	StateLimit     int                    `json:"state_limit"`
	IdleTTLSeconds int                    `json:"idle_ttl_seconds"`
}

func availabilityStatus(entry *availabilityEntry, now time.Time) AvailabilityStatus {
	status := AvailabilityStatus{State: entry.state()}
	if entry != nil {
		status.Reason, status.LastSuccess, status.LastFailure, status.RetryAt = entry.reason, entry.lastSuccess, entry.lastFailure, entry.retryAt
		status.ProbeDue, status.ConsecutiveFailures = entry.failures > 0 && !entry.probing && !now.Before(entry.retryAt), entry.failures
	}
	return status
}

func (h *handler) availabilityReport() AvailabilityReport {
	s := h.availability
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.prune(now)
	report := AvailabilityReport{Timestamp: now, Providers: []ProviderAvailability{}, RecoveryMode: "request_driven", StateLimit: availabilityLimit, IdleTTLSeconds: int(availabilityTTL / time.Second)}
	for ref := range h.providers {
		generation := h.providerGenerations[ref]
		provider := ProviderAvailability{Ref: ref, Generation: generation,
			AvailabilityStatus: availabilityStatus(s.entries[availabilityTarget{ref, "", generation}], now), Models: []ModelAvailability{}}
		provider.KeyPool = h.keyPools[ref].diagnosticStatus(now)
		if _, unavailable := h.unavailableProviders[ref]; unavailable {
			provider.State, provider.Reason = "unavailable", "protocol_unresolved"
		}
		for target, entry := range s.entries {
			if target.provider == ref && target.generation == generation && target.model != "" {
				provider.Models = append(provider.Models, ModelAvailability{target.model, availabilityStatus(entry, now)})
			}
		}
		sort.Slice(provider.Models, func(i, j int) bool { return provider.Models[i].Model < provider.Models[j].Model })
		report.Providers = append(report.Providers, provider)
	}
	sort.Slice(report.Providers, func(i, j int) bool { return report.Providers[i].Ref < report.Providers[j].Ref })
	return report
}

func (h *handler) modelRouteAvailable(provider, model string) bool {
	return h.providerRouteAvailable(provider) && h.availability.available(h.routingEpoch, provider, model, h.providerGenerations[provider])
}
