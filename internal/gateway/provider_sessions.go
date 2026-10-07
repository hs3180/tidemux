package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/hs3180/tidemux/internal/limiter"
)

type requestSessionAdmissionKey struct{}

// Request leases outlive all adapter/key attempts and downstream delivery.
// Failed routes release only their own reference; successful routes remain
// leased until the gateway knows whether the client received the response.
type requestSessionAdmission struct {
	handler   *handler
	id        string
	global    *limiter.SessionLease
	providers map[string]*limiter.SessionLease
}

func (h *handler) sessionCapacityEnabled(routes []routeStep) bool {
	if h.config.MaxActiveSessions > 0 {
		return true
	}
	for _, route := range routes {
		if h.providers[route.provider].MaxActiveSessions > 0 {
			return true
		}
	}
	return false
}

func (a *requestSessionAdmission) acquireProvider(ctx context.Context, ref string, limit int) error {
	if a.providers[ref] != nil {
		return nil
	}
	h := a.handler
	h.providerSessionMu.Lock()
	defer h.providerSessionMu.Unlock()
	if h.providerSessions == nil {
		h.providerSessions = map[string]*limiter.SessionLimiter{}
	}
	// Retired refs may be reused, but never drop retained or in-flight entries.
	// Acquire holds the map lock so pruning cannot split a provider's state.
	for name, sessions := range h.providerSessions {
		_, configured := h.providers[name]
		if _, current, _ := sessions.Stats(); !configured && current == 0 {
			delete(h.providerSessions, name)
		}
	}
	sessions := h.providerSessions[ref]
	if sessions == nil {
		var err error
		sessions, err = limiter.NewSessionLimiter(0, time.Duration(h.config.ActiveSessionIdleTimeoutSeconds)*time.Second)
		if err != nil {
			return err
		}
		h.providerSessions[ref] = sessions
	}
	lease, err := sessions.AcquireWithLimit(ctx, a.id, limit)
	if err != nil && limit == 0 && ctx.Err() != nil {
		// An already canceled request has no active slot to track. Preserve the
		// unlimited route's adapter cancellation/audit contract.
		return nil
	}
	if err == nil {
		a.providers[ref] = lease
	}
	return err
}

func (a *requestSessionAdmission) releaseProvider(ref string) {
	if lease := a.providers[ref]; lease != nil {
		lease.Release(false)
		delete(a.providers, ref)
	}
}

func (a *requestSessionAdmission) touchOutput() {
	a.global.TouchOutput()
	for _, lease := range a.providers {
		lease.TouchOutput()
	}
}

func (a *requestSessionAdmission) release(retain bool) {
	for _, lease := range a.providers {
		lease.Release(retain)
	}
	a.global.Release(retain)
}

func (h *handler) closeProviderSessions() {
	h.providerSessionMu.Lock()
	defer h.providerSessionMu.Unlock()
	for _, sessions := range h.providerSessions {
		sessions.Close()
	}
	h.providerSessions = nil
}

type sessionCapacityStatus struct {
	Limit    int    `json:"limit"`
	Current  int    `json:"current"`
	Rejected uint64 `json:"rejected"`
}

func (h *handler) sessionStatus(w http.ResponseWriter) {
	limit, current, rejected := h.sessions.Stats()
	providers := map[string]sessionCapacityStatus{}
	h.providerSessionMu.Lock()
	for ref, provider := range h.providers {
		var current int
		var rejected uint64
		if sessions := h.providerSessions[ref]; sessions != nil {
			_, current, rejected = sessions.Stats()
		}
		providers[ref] = sessionCapacityStatus{Limit: provider.MaxActiveSessions, Current: current, Rejected: rejected}
	}
	h.providerSessionMu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"gateway": sessionCapacityStatus{limit, current, rejected}, "providers": providers})
}
