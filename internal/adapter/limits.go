package adapter

import "errors"

// Zero values preserve the limits of existing 0.1.0 configurations.
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
		l.RequestBytes = 1 << 20
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
