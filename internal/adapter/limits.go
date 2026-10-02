package adapter

import (
	"errors"
	"time"
)

// Zero values select gateway defaults; explicit limits are preserved.
type Limits struct {
	UpstreamTimeoutSeconds int   `json:"upstream_timeout_seconds,omitempty"`
	RequestBytes           int64 `json:"request_bytes,omitempty"`
	ResponseBytes          int64 `json:"response_bytes,omitempty"`
	StreamBytes            int64 `json:"stream_bytes,omitempty"`
	EventBytes             int   `json:"event_bytes,omitempty"`
}

func (l Limits) Effective() Limits {
	if l.UpstreamTimeoutSeconds == 0 {
		l.UpstreamTimeoutSeconds = 60
	}
	if l.RequestBytes == 0 {
		l.RequestBytes = 32 << 20
	}
	if l.ResponseBytes == 0 {
		l.ResponseBytes = 8 << 20
	}
	if l.StreamBytes == 0 {
		l.StreamBytes = 64 << 20
	}
	if l.EventBytes == 0 {
		l.EventBytes = 1 << 20
	}
	return l
}

// UpstreamTimeout gives long-running agent streams ten minutes by default.
// An explicitly configured timeout continues to bound both response modes.
func (l Limits) UpstreamTimeout(stream bool) time.Duration {
	if stream && l.UpstreamTimeoutSeconds == 0 {
		return 10 * time.Minute
	}
	return time.Duration(l.Effective().UpstreamTimeoutSeconds) * time.Second
}

func (l Limits) Validate() error {
	if l.UpstreamTimeoutSeconds < 0 || l.UpstreamTimeoutSeconds > 3600 {
		return errors.New("limits.upstream_timeout_seconds must be 0..3600")
	}
	for _, n := range []int64{l.RequestBytes, l.ResponseBytes, l.StreamBytes, int64(l.EventBytes)} {
		if n < 0 || n > 1<<30 {
			return errors.New("byte limits must be 0..1073741824")
		}
	}
	if int64(l.Effective().EventBytes) > l.Effective().StreamBytes {
		return errors.New("limits.event_bytes must not exceed stream_bytes")
	}
	return nil
}
