package gateway

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/hs3180/tidemux/internal/adapter"
)

func (h *handler) beginCall(parent context.Context) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancelCause(parent)
	h.activeMu.Lock()
	if h.draining {
		h.activeMu.Unlock()
		cancel(adapter.ErrServerShuttingDown)
		return nil, func() {}, false
	}
	h.nextCallID++
	id := h.nextCallID
	if h.activeCalls == nil {
		h.activeCalls = make(map[uint64]context.CancelCauseFunc)
	}
	h.activeCalls[id] = cancel
	h.activeMu.Unlock()
	return ctx, func() {
		h.activeMu.Lock()
		delete(h.activeCalls, id)
		h.activeMu.Unlock()
		cancel(context.Canceled)
	}, true
}

func (h *handler) beginShutdown() {
	h.activeMu.Lock()
	if h.draining {
		h.activeMu.Unlock()
		return
	}
	h.draining = true
	cancels := make([]context.CancelCauseFunc, 0, len(h.activeCalls))
	for _, cancel := range h.activeCalls {
		cancels = append(cancels, cancel)
	}
	h.activeMu.Unlock()
	for _, cancel := range cancels {
		cancel(adapter.ErrServerShuttingDown)
	}
}

// BeginShutdown synchronously stops model admission and cancels upstream work.
// Call Server.Shutdown afterwards to await responses and accounting cleanup.
func BeginShutdown(server *http.Server) {
	if h, ok := server.Handler.(*handler); ok {
		h.beginShutdown()
	}
}

func writeStreamFailure(w http.ResponseWriter, protocol, code string) {
	// A disconnected or panicking downstream writer cannot receive an error.
	defer func() { _ = recover() }()
	payload := map[string]any{"error": map[string]any{"type": "api_error", "code": code, "message": code}}
	if protocol == "anthropic" {
		payload["type"] = "error"
	}
	encoded, _ := json.Marshal(payload)
	_, _ = w.Write(append(append([]byte("event: error\ndata: "), encoded...), []byte("\n\n")...))
	_ = http.NewResponseController(w).Flush()
}
