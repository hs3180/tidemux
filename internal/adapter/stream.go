package adapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// StreamSink receives validated SSE frames before the terminal frame. The
// terminal frame is returned to the caller and sent only after audit commits.
type StreamSink func(id string, frame []byte) error

func readStream(protocol string, r io.Reader, emit func([]byte) error) (TokenUsage, []byte, error) {
	return readStreamWithLimits(protocol, r, Limits{}.Effective(), emit)
}
func readStreamWithLimits(protocol string, r io.Reader, limits Limits, emit func([]byte) error) (TokenUsage, []byte, error) {
	maxBytes := limits.StreamBytes
	reader := io.LimitReader(r, maxBytes+1)
	scan := bufio.NewScanner(reader)
	scan.Buffer(make([]byte, min(4096, limits.EventBytes+1)), limits.EventBytes+1)
	// Keep original line endings: limits count wire bytes, including CRLF.
	scan.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		advance, token, err := bufio.ScanLines(data, atEOF)
		if token == nil {
			return advance, nil, err
		}
		return advance, data[:advance], err
	})
	var frame bytes.Buffer
	usage := map[string]json.RawMessage{}
	total := int64(0)
	started := false
	finished := false
	fail := func(code string) (TokenUsage, []byte, error) { return TokenUsage{}, nil, &CallError{502, code} }
	for scan.Scan() {
		wireLine := scan.Bytes()
		line := strings.TrimSuffix(strings.TrimSuffix(string(wireLine), "\n"), "\r")
		total += int64(len(wireLine))
		if total > maxBytes {
			return fail("upstream_response_too_large")
		}
		frame.Write(wireLine)
		if frame.Len() > limits.EventBytes {
			return fail("upstream_event_too_large")
		}
		if line != "" {
			continue
		}
		raw := append([]byte(nil), frame.Bytes()...)
		frame.Reset()
		var lines []string
		event := ""
		for _, l := range strings.Split(string(raw), "\n") {
			l = strings.TrimSuffix(l, "\r")
			if strings.HasPrefix(l, "event:") {
				event = strings.TrimPrefix(strings.TrimPrefix(l, "event:"), " ")
			}
			if strings.HasPrefix(l, "data:") {
				lines = append(lines, strings.TrimPrefix(strings.TrimPrefix(l, "data:"), " "))
			}
		}
		if len(lines) == 0 {
			if err := emit(raw); err != nil {
				return TokenUsage{}, nil, err
			}
			continue
		}
		if event == "error" {
			return fail("upstream_stream_error")
		}
		if protocol == "openai" && event != "" && event != "message" {
			return fail("invalid_upstream_stream")
		}
		data := strings.Join(lines, "\n")
		terminal := false
		if protocol == "openai" && data == "[DONE]" {
			if !started || !finished {
				return fail("incomplete_upstream_stream")
			}
			terminal = true
		} else {
			var obj map[string]json.RawMessage
			if StrictJSON([]byte(data), &obj) != nil || obj == nil {
				return fail("invalid_upstream_stream")
			}
			var kind string
			json.Unmarshal(obj["type"], &kind)
			if protocol == "anthropic" && event != "" && event != kind {
				return fail("invalid_upstream_stream")
			}
			if obj["error"] != nil || kind == "error" {
				return fail("upstream_stream_error")
			}
			merge := func(raw json.RawMessage) error {
				if len(raw) == 0 || string(raw) == "null" {
					return nil
				}
				var u map[string]json.RawMessage
				if json.Unmarshal(raw, &u) != nil || u == nil {
					return errors.New("usage")
				}
				for k, v := range u {
					usage[k] = v
				}
				return nil
			}
			if protocol == "openai" {
				var choices []struct {
					Index        int             `json:"index"`
					Delta        json.RawMessage `json:"delta"`
					FinishReason *string         `json:"finish_reason"`
				}
				if json.Unmarshal(obj["choices"], &choices) != nil {
					return fail("invalid_upstream_stream")
				}
				for _, c := range choices {
					if !object(c.Delta) {
						return fail("invalid_upstream_stream")
					}
					if c.Index == 0 {
						started = true
					}
					if c.Index == 0 && c.FinishReason != nil && *c.FinishReason != "" {
						finished = true
					}
				}
				if merge(obj["usage"]) != nil {
					return fail("invalid_upstream_usage")
				}
			} else {
				switch kind {
				case "message_start":
					if started {
						return fail("invalid_upstream_stream")
					}
					started = true
					var msg struct {
						Usage json.RawMessage `json:"usage"`
						Role  string          `json:"role"`
					}
					if json.Unmarshal(obj["message"], &msg) != nil || msg.Role != "assistant" || merge(msg.Usage) != nil {
						return fail("invalid_upstream_stream")
					}
				case "message_delta":
					if !started {
						return fail("invalid_upstream_stream")
					}
					var delta struct {
						StopReason *string `json:"stop_reason"`
					}
					if json.Unmarshal(obj["delta"], &delta) != nil {
						return fail("invalid_upstream_stream")
					}
					if delta.StopReason != nil && *delta.StopReason != "" {
						finished = true
					}
					if merge(obj["usage"]) != nil {
						return fail("invalid_upstream_usage")
					}
				case "message_stop":
					if !started || !finished {
						return fail("incomplete_upstream_stream")
					}
					terminal = true
				case "ping":
				default:
					if !started {
						return fail("invalid_upstream_stream")
					}
				}
			}
		}
		if terminal {
			ujson, _ := json.Marshal(usage)
			u, err := ParseUsage(protocol, ujson)
			if err != nil {
				return fail("invalid_upstream_usage")
			}
			return u, raw, nil
		}
		if err := emit(raw); err != nil {
			return TokenUsage{}, nil, err
		}
	}
	if errors.Is(scan.Err(), bufio.ErrTooLong) {
		return fail("upstream_event_too_large")
	}
	if scan.Err() != nil {
		return fail("upstream_stream_read_error")
	}
	return fail("incomplete_upstream_stream")
}
