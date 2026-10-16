package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/diagnostics"
)

const defaultProviderSwitchThreshold = int64(60)

func validSwitchThreshold(value *int64) error {
	if value != nil && (*value < 0 || *value > 86400) {
		return errors.New("provider_switch_threshold must be an integer number of seconds in 0..86400")
	}
	return nil
}

func (p Provider) validateSwitchThresholds() error {
	if err := validSwitchThreshold(p.ProviderSwitchThreshold); err != nil {
		return err
	}
	if len(p.ModelSwitchThresholds) > 256 {
		return errors.New("model_switch_thresholds may contain at most 256 entries")
	}
	for model, seconds := range p.ModelSwitchThresholds {
		if model == "" || strings.TrimSpace(model) != model || len(model) > 255 {
			return errors.New("model_switch_thresholds requires nonempty model IDs up to 255 bytes")
		}
		if err := validSwitchThreshold(&seconds); err != nil {
			return err
		}
	}
	return nil
}

func (h *handler) switchThreshold(provider, model string) int64 {
	p := h.providers[provider]
	if seconds, ok := p.ModelSwitchThresholds[model]; ok {
		return seconds
	}
	if p.ProviderSwitchThreshold != nil {
		return *p.ProviderSwitchThreshold
	}
	if seconds := h.config.EffectiveRouting().ProviderSwitchThreshold; seconds != nil {
		return *seconds
	}
	return defaultProviderSwitchThreshold
}

// A snapshot uses the same maximum of independent gates as retry guidance.
// The threshold compares absolute deadlines, without duration saturation or
// rounding an over-threshold remainder down to the threshold boundary.
func (h *handler) cooldownChoice(route routeStep, alternatives []routeStep) (*diagnostics.RoutingDecision, bool) {
	now := time.Now()
	if h.availability != nil {
		h.availability.mu.Lock()
		defer h.availability.mu.Unlock()
		if h.availability.epoch != h.routingEpoch {
			return nil, false
		}
		now = h.availability.now()
	}
	return h.cooldownChoiceLocked(route, alternatives, now)
}

func (h *handler) cooldownChoiceLocked(route routeStep, alternatives []routeStep, now time.Time) (*diagnostics.RoutingDecision, bool) {
	if !h.providerBindingPresent(route.provider) {
		return nil, false
	}
	wait := h.routeWaitLocked(route, now)
	if !wait.blocked {
		return nil, false
	}
	threshold := h.switchThreshold(route.provider, route.model)
	d := &diagnostics.RoutingDecision{FromProvider: route.provider, ToProvider: route.provider, Model: route.model, Action: "retain", Reason: "within_threshold", ThresholdMS: threshold * 1000, WaitUnknown: wait.unknown}
	if wait.retryAt.After(now) {
		remaining := wait.retryAt.UnixMilli() - now.UnixMilli()
		if wait.retryAt.Nanosecond()%1000000 > now.Nanosecond()%1000000 {
			remaining++
		}
		if remaining < 1 {
			remaining = 1
		}
		d.RemainingMS = &remaining
		d.CooldownUntilMS = wait.retryAt.UnixMilli()
	}
	long := wait.retryAt.After(now.Add(time.Duration(threshold) * time.Second))
	if wait.unknown && !long && threshold != 0 {
		d.Reason = "unknown_wait"
		return d, true
	}
	if !long && !(wait.unknown && threshold == 0) {
		return d, true
	}
	for _, alternative := range alternatives {
		if alternative.provider != route.provider && !h.routeWaitLocked(alternative, now).blocked {
			d.Action, d.Reason = "switch", "exceeds_threshold"
			if wait.unknown && !long {
				d.Reason = "affinity_disabled"
			}
			return d, false
		}
	}
	if threshold == 0 {
		return nil, false
	}
	d.Reason = "no_eligible_alternative"
	return d, true
}

func (h *handler) providerBindingPresent(provider string) bool {
	_, unavailable := h.unavailableProviders[provider]
	return !unavailable && h.clients[provider] != nil
}

type routingChoiceContextKey struct{}

func withRoutingChoice(r *http.Request, route routeStep) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), routingChoiceContextKey{}, route))
}

func routingDecision(r *http.Request) *diagnostics.RoutingDecision {
	choice, _ := r.Context().Value(routingChoiceContextKey{}).(routeStep)
	return choice.decision.Normalize()
}

func (h *handler) retainedRoute(route routeStep, alternatives []routeStep) (routeStep, bool) {
	d, retain := h.cooldownChoice(route, alternatives)
	route.decision = d
	if retain {
		route.holdAffinity = true
	}
	return route, retain
}

// A late extension while queued can exceed the threshold; re-resolve before
// dispatch rather than freezing a short wait observed before the queue.
func (h *handler) retainQueuedAffinity(route routeStep, protocol string, body []byte) bool {
	var alternatives []routeStep
	for provider := range h.providers {
		if h.supportScopeAllows(provider, route.model) && h.routeRequestCompatible(provider, route.model, protocol, body) {
			alternatives = append(alternatives, routeStep{provider: provider, model: route.model})
		}
	}
	_, retain := h.cooldownChoice(route, alternatives)
	return retain
}
