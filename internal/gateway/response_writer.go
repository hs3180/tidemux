package gateway

import (
	"net/http"
	"strings"
)

// trackedResponseWriter preserves ResponseController operations while tracking
// whether panic recovery can still send JSON or needs an SSE error event.
type trackedResponseWriter struct {
	http.ResponseWriter
	wroteHeader, eventStream, streamComplete bool
}

func (w *trackedResponseWriter) WriteHeader(status int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		w.eventStream = strings.HasPrefix(strings.ToLower(w.Header().Get("Content-Type")), "text/event-stream")
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *trackedResponseWriter) Write(data []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(data)
}

func (w *trackedResponseWriter) FlushError() error {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *trackedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
