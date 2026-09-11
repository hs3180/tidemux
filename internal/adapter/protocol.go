package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
)

func ptr[T any](v T) *T { return &v }

// StrictJSON rejects trailing documents, duplicate keys, unknown fields and null roots.
func StrictJSON(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		t, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return e
					}
					key, ok := k.(string)
					if !ok || seen[strings.ToLower(key)] {
						return errors.New("duplicate JSON key")
					}
					seen[strings.ToLower(key)] = true
					if e = walk(); e != nil {
						return e
					}
				}
				_, err = d.Token()
			case '[':
				for d.More() {
					if e := walk(); e != nil {
						return e
					}
				}
				_, err = d.Token()
			default:
				return errors.New("invalid JSON")
			}
		}
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return errors.New("null JSON")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}

type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}
type Input struct {
	Model               string          `json:"model"`
	Messages            []Message       `json:"messages"`
	Stream              bool            `json:"stream,omitempty"`
	MaxTokens           *int64          `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int64          `json:"max_completion_tokens,omitempty"`
	System              json.RawMessage `json:"system,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	StopSequences       []string        `json:"stop_sequences,omitempty"`
}

func textContent(raw json.RawMessage) bool {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s != ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if StrictJSON(raw, &blocks) != nil || len(blocks) == 0 {
		return false
	}
	for _, b := range blocks {
		if b.Type != "text" || b.Text == "" {
			return false
		}
	}
	return true
}
func Request(protocol string, data []byte, defaultModel string) ([]byte, string, error) {
	var in Input
	if StrictJSON(data, &in) != nil {
		return nil, "", errors.New("invalid_request")
	}
	if in.Model == "" {
		in.Model = defaultModel
	}
	if strings.TrimSpace(in.Model) == "" {
		return nil, "", errors.New("model")
	}
	if in.Stream {
		return nil, "", errors.New("stream")
	}
	if len(in.Messages) == 0 {
		return nil, "", errors.New("messages")
	}
	for _, m := range in.Messages {
		allowed := m.Role == "user" || m.Role == "assistant"
		if protocol == "openai" {
			allowed = allowed || m.Role == "system" || m.Role == "developer"
		}
		if !allowed || !textContent(m.Content) {
			return nil, "", errors.New("messages")
		}
	}
	for _, n := range []*int64{in.MaxTokens, in.MaxCompletionTokens} {
		if n != nil && (*n < 1 || *n > 1_000_000) {
			return nil, "", errors.New("max_tokens")
		}
	}
	if in.Temperature != nil && (*in.Temperature < 0 || *in.Temperature > 2) {
		return nil, "", errors.New("temperature")
	}
	if in.TopP != nil && (*in.TopP < 0 || *in.TopP > 1) {
		return nil, "", errors.New("top_p")
	}
	if protocol == "anthropic" {
		if in.MaxTokens == nil || in.MaxCompletionTokens != nil || len(in.Stop) > 0 {
			return nil, "", errors.New("invalid_request")
		}
		if len(in.System) > 0 && !textContent(in.System) {
			return nil, "", errors.New("system")
		}
		if in.Temperature != nil && *in.Temperature > 1 {
			return nil, "", errors.New("temperature")
		}
	}
	if protocol == "openai" {
		if len(in.System) > 0 || in.StopSequences != nil || (in.MaxTokens != nil && in.MaxCompletionTokens != nil) {
			return nil, "", errors.New("invalid_request")
		}
		if len(in.Stop) > 0 {
			var s string
			var ss []string
			if json.Unmarshal(in.Stop, &s) != nil && (json.Unmarshal(in.Stop, &ss) != nil || len(ss) < 1 || len(ss) > 4) {
				return nil, "", errors.New("stop")
			}
		}
	}
	encoded, err := json.Marshal(in)
	return encoded, in.Model, err
}

type TokenUsage struct{ Input, Output, CacheRead, CacheWrite *int64 }
type Price struct {
	Currency   string   `json:"currency"`
	Source     string   `json:"source"`
	Version    string   `json:"version"`
	Input      *float64 `json:"input_per_million"`
	Output     *float64 `json:"output_per_million"`
	CacheRead  *float64 `json:"cache_read_per_million,omitempty"`
	CacheWrite *float64 `json:"cache_write_per_million,omitempty"`
}

func (p Price) Validate() error {
	if len(p.Currency) != 3 || strings.ToUpper(p.Currency) != p.Currency || strings.TrimSpace(p.Source) == "" || strings.TrimSpace(p.Version) == "" || p.Input == nil || p.Output == nil {
		return errors.New("pricing requires currency, source, version, input and output rates")
	}
	for _, x := range []*float64{p.Input, p.Output, p.CacheRead, p.CacheWrite} {
		if x != nil && (*x < 0 || math.IsNaN(*x) || math.IsInf(*x, 0)) {
			return errors.New("invalid price")
		}
	}
	return nil
}
func ParseUsage(protocol string, data []byte) (TokenUsage, error) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil || raw == nil {
		return TokenUsage{}, nil
	}
	get := func(key string) (*int64, error) {
		v, ok := raw[key]
		if !ok || string(v) == "null" {
			return nil, nil
		}
		var n int64
		if json.Unmarshal(v, &n) != nil || n < 0 || n > 1_000_000_000_000 {
			return nil, errors.New("invalid usage")
		}
		return &n, nil
	}
	u := TokenUsage{}
	var err error
	if protocol == "openai" {
		u.Input, err = get("prompt_tokens")
		if err != nil {
			return TokenUsage{}, err
		}
		u.Output, err = get("completion_tokens")
		if err != nil {
			return TokenUsage{}, err
		}
		u.CacheRead, err = get("prompt_cache_hit_tokens")
		if err != nil {
			return TokenUsage{}, err
		}
		if details, ok := raw["prompt_tokens_details"]; ok {
			var d map[string]json.RawMessage
			if json.Unmarshal(details, &d) != nil {
				return TokenUsage{}, errors.New("invalid usage")
			}
			for key, target := range map[string]**int64{"cached_tokens": &u.CacheRead, "cache_write_tokens": &u.CacheWrite} {
				if b, ok := d[key]; ok && string(b) != "null" {
					var n int64
					if json.Unmarshal(b, &n) != nil || n < 0 || n > 1_000_000_000_000 {
						return TokenUsage{}, errors.New("invalid usage")
					}
					*target = &n
				}
			}
		}
		miss, e := get("prompt_cache_miss_tokens")
		if e != nil {
			return TokenUsage{}, e
		}
		if miss != nil && (u.Input == nil || u.CacheRead == nil || *miss+*u.CacheRead != *u.Input) {
			return TokenUsage{}, errors.New("incomplete cache usage")
		}
	} else {
		u.Input, err = get("input_tokens")
		if err != nil {
			return TokenUsage{}, err
		}
		u.Output, err = get("output_tokens")
		if err != nil {
			return TokenUsage{}, err
		}
		u.CacheRead, err = get("cache_read_input_tokens")
		if err != nil {
			return TokenUsage{}, err
		}
		u.CacheWrite, err = get("cache_creation_input_tokens")
		if err != nil {
			return TokenUsage{}, err
		}
		if u.Input != nil {
			v := *u.Input
			for _, n := range []*int64{u.CacheRead, u.CacheWrite} {
				if n != nil {
					v += *n
				}
			}
			u.Input = &v
		}
	}
	if u.Input != nil {
		sum := int64(0)
		for _, n := range []*int64{u.CacheRead, u.CacheWrite} {
			if n != nil {
				sum += *n
			}
		}
		if sum > *u.Input {
			return TokenUsage{}, errors.New("invalid cache usage")
		}
	}
	return u, nil
}
func (p Price) Estimate(protocol string, u TokenUsage) *float64 {
	if u.Input == nil || u.Output == nil || p.Input == nil || p.Output == nil {
		return nil
	}
	input := *u.Input
	cost := float64(*u.Output) * *p.Output
	for _, pair := range []struct {
		n    *int64
		rate *float64
	}{{u.CacheRead, p.CacheRead}, {u.CacheWrite, p.CacheWrite}} {
		// Unknown breakdown with differential rates cannot produce a trustworthy cost.
		if pair.n == nil {
			if pair.rate != nil && *pair.rate != *p.Input {
				return nil
			}
			continue
		}
		if *pair.n > 0 {
			if pair.rate == nil {
				return nil
			}
			cost += float64(*pair.n) * *pair.rate
			input -= *pair.n
		}
	}
	if input < 0 {
		return nil
	}
	cost = (cost + float64(input)**p.Input) / 1e6
	if math.IsInf(cost, 0) || math.IsNaN(cost) {
		return nil
	}
	return &cost
}

// ValidateResponse preserves the provider's compatible JSON instead of rebuilding it.
func ValidateResponse(protocol string, data []byte) (TokenUsage, error) {
	var r struct {
		Choices []struct {
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
				Refusal string          `json:"refusal"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
		Usage   json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(data, &r) != nil {
		return TokenUsage{}, errors.New("invalid_upstream_response")
	}
	if protocol == "openai" {
		if len(r.Choices) == 0 {
			return TokenUsage{}, errors.New("invalid_upstream_response")
		}
		for _, c := range r.Choices {
			if c.Message.Role != "assistant" || (!textContent(c.Message.Content) && c.Message.Refusal == "") {
				return TokenUsage{}, errors.New("invalid_upstream_response")
			}
		}
	} else {
		if r.Type != "message" || r.Role != "assistant" || !textContent(r.Content) {
			return TokenUsage{}, errors.New("invalid_upstream_response")
		}
	}
	return ParseUsage(protocol, r.Usage)
}
