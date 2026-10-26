package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/hs3180/tidemux/internal/contentlog"
)

// trackedResponseWriter preserves ResponseController operations while tracking
// whether panic recovery can still send JSON or needs an SSE error event.
type trackedResponseWriter struct {
	http.ResponseWriter
	wroteHeader, eventStream, streamComplete bool
	content                                  *requestContent
	contentSuccess, contentWriteFailed       bool
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
	w.startContent()
	n, err := w.ResponseWriter.Write(data)
	if w.content != nil {
		w.content.capture.Write(data[:n])
	}
	if err != nil || n != len(data) {
		w.contentWriteFailed = true
	}
	return n, err
}

func (w *trackedResponseWriter) FlushError() error {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		w.contentWriteFailed = true
	}
	return err
}

func (w *trackedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type requestContent struct {
	log      *contentlog.Log
	metadata contentlog.Metadata
	input    []byte // Existing admitted body; no second full-body buffer.
	capture  *contentlog.Capture
}

func (w *trackedResponseWriter) startContent() {
	if c := w.content; c != nil && c.capture == nil {
		if c.metadata.RequestID == "" {
			c.metadata.RequestID = w.Header().Get("X-TideMux-Request-ID")
		}
		if c.metadata.RequestID != "" {
			c.capture = c.log.Start(c.metadata, c.input)
			c.input = nil
		}
	}
}

func (w *trackedResponseWriter) finishContent(ctx context.Context) {
	w.startContent()
	if w.content == nil || w.content.capture == nil {
		return
	}
	outcome := "error"
	if w.contentSuccess && !w.contentWriteFailed {
		outcome = "success"
	}
	if ctx.Err() != nil {
		outcome = "canceled"
	}
	w.content.capture.Finish(outcome, w.contentSuccess && !w.contentWriteFailed)
}
