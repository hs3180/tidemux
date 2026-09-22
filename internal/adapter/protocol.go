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
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ToolCalls        []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID       string          `json:"tool_call_id,omitempty"`
	ReasoningContent *string         `json:"reasoning_content,omitempty"`
}
type Thinking struct {
	Type         string `json:"type"`
	BudgetTokens *int64 `json:"budget_tokens,omitempty"`
	Display      string `json:"display,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}
type OutputFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema,omitempty"`
}
type OutputConfig struct {
	Effort string        `json:"effort,omitempty"`
	Format *OutputFormat `json:"format,omitempty"`
}
type ResponseFormat struct {
	Type       string `json:"type"`
	JSONSchema *struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Schema      json.RawMessage `json:"schema"`
		Strict      *bool           `json:"strict,omitempty"`
	} `json:"json_schema,omitempty"`
}
type Metadata struct {
	UserID string `json:"user_id,omitempty"`
}
type Input struct {
	ContextManagement json.RawMessage `json:"context_management,omitempty"`
	StreamOptions     *StreamOptions  `json:"stream_options,omitempty"`
	Metadata          *Metadata       `json:"metadata,omitempty"`
	OutputConfig      *OutputConfig   `json:"output_config,omitempty"`
	ResponseFormat    *ResponseFormat `json:"response_format,omitempty"`
	ReasoningEffort   string          `json:"reasoning_effort,omitempty"`

	Tools             []Tool          `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`

	Thinking            *Thinking       `json:"thinking,omitempty"`
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
	if in.Thinking != nil {
		if in.Thinking.Display != "" && (protocol != "anthropic" || in.Thinking.Type == "disabled" || (in.Thinking.Display != "summarized" && in.Thinking.Display != "omitted")) {
			return nil, "", errors.New("thinking")
		}
		switch in.Thinking.Type {
		case "disabled", "adaptive":
			if in.Thinking.BudgetTokens != nil || in.Thinking.Type == "adaptive" && protocol != "anthropic" {
				return nil, "", errors.New("thinking")
			}
		case "enabled":
			if protocol == "anthropic" && (in.Thinking.BudgetTokens == nil || *in.Thinking.BudgetTokens < 1024) {
				return nil, "", errors.New("thinking")
			}
			if in.Thinking.BudgetTokens != nil && (*in.Thinking.BudgetTokens < 1 || *in.Thinking.BudgetTokens > 1_000_000) {
				return nil, "", errors.New("thinking")
			}
		default:
			return nil, "", errors.New("thinking")
		}
	}
	if in.StreamOptions != nil && (protocol != "openai" || !in.Stream) {
		return nil, "", errors.New("stream_options")
	}
	if in.ContextManagement != nil && (protocol != "anthropic" || !object(in.ContextManagement)) {
		return nil, "", errors.New("context_management")
	}
	if protocol == "openai" && (in.Metadata != nil || in.OutputConfig != nil) {
		return nil, "", errors.New("invalid_request")
	}
	if protocol == "anthropic" && (in.ResponseFormat != nil || in.ReasoningEffort != "") {
		return nil, "", errors.New("invalid_request")
	}
	if in.ResponseFormat != nil {
		f := in.ResponseFormat
		if f.Type == "json_schema" {
			if f.JSONSchema == nil || f.JSONSchema.Name == "" || !object(f.JSONSchema.Schema) {
				return nil, "", errors.New("response_format")
			}
		} else if (f.Type != "json_object" && f.Type != "text") || f.JSONSchema != nil {
			return nil, "", errors.New("response_format")
		}
	}
	if in.OutputConfig != nil {
		o := in.OutputConfig
		if o.Effort != "" && o.Effort != "low" && o.Effort != "medium" && o.Effort != "high" && o.Effort != "max" {
			return nil, "", errors.New("output_config")
		}
		if o.Format != nil && (o.Format.Type != "json_schema" || !object(o.Format.Schema)) {
			return nil, "", errors.New("output_config")
		}
	}
	if in.Stream && protocol == "openai" && in.StreamOptions == nil {
		in.StreamOptions = &StreamOptions{IncludeUsage: true}
	}
	if len(in.Messages) == 0 {
		return nil, "", errors.New("messages")
	}
	if !validTools(protocol, in.Tools) || !validChoice(protocol, in.ToolChoice) {
		return nil, "", errors.New("tools")
	}
	if protocol == "anthropic" && in.ParallelToolCalls != nil {
		return nil, "", errors.New("parallel_tool_calls")
	}
	for _, m := range in.Messages {
		// System-role messages are a compatible-provider extension emitted by
		// gateway clients. Preserve their position; do not promote or rewrite them.
		allowed := m.Role == "user" || m.Role == "assistant" || m.Role == "system"
		if protocol == "openai" {
			allowed = allowed || m.Role == "system" || m.Role == "developer" || m.Role == "tool"
		}
		if !allowed {
			return nil, "", errors.New("messages")
		}
		if protocol == "anthropic" && (len(m.ToolCalls) > 0 || m.ToolCallID != "" || m.ReasoningContent != nil) {
			return nil, "", errors.New("messages")
		}
		if len(m.ToolCalls) > 0 && (m.Role != "assistant" || !validCalls(m.ToolCalls)) {
			return nil, "", errors.New("tool_calls")
		}
		if (m.Role == "tool") != (m.ToolCallID != "") {
			return nil, "", errors.New("tool_call_id")
		}
		if m.ReasoningContent != nil && m.Role != "assistant" {
			return nil, "", errors.New("reasoning_content")
		}
		emptyAssistant := protocol == "openai" && m.Role == "assistant" && len(m.ToolCalls) > 0 && (len(m.Content) == 0 || string(m.Content) == "null")
		if !emptyAssistant && !messageContent(protocol, m.Role, m.Content) {
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
		if len(in.System) > 0 && !messageContent("anthropic", "system", in.System) {
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
	// pi-ai sends this optional Anthropic client hint on every tool. Accept it
	// at the gateway boundary for client compatibility, but never pass the hint
	// through to a provider that may not implement it.
	for i := range in.Tools {
		in.Tools[i].EagerInputStreaming = nil
	}
	encoded, err := json.Marshal(in)
	return encoded, in.Model, err
}

type TokenUsage struct{ Input, Output, CacheRead, CacheWrite *int64 }
type Price struct {
	Currency       string   `json:"currency"`
	Source         string   `json:"source"`
	Version        string   `json:"version"`
	InputCacheHit  *float64 `json:"input_cache_hit_per_million"`
	InputCacheMiss *float64 `json:"input_cache_miss_per_million"`
	Output         *float64 `json:"output_per_million"`
}

func (p Price) Validate() error {
	if len(p.Currency) != 3 || strings.ToUpper(p.Currency) != p.Currency || strings.TrimSpace(p.Source) == "" || strings.TrimSpace(p.Version) == "" || p.InputCacheHit == nil || p.InputCacheMiss == nil || p.Output == nil {
		return errors.New("pricing requires currency, source, version, input cache hit, input cache miss and output rates")
	}
	for _, x := range []*float64{p.InputCacheHit, p.InputCacheMiss, p.Output} {
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
func (p Price) Estimate(_ string, u TokenUsage) *float64 {
	if u.Input == nil || u.Output == nil || *u.Input < 0 || *u.Output < 0 || p.InputCacheHit == nil || p.InputCacheMiss == nil || p.Output == nil {
		return nil
	}
	cacheRead := int64(0)
	if u.CacheRead != nil {
		if *u.CacheRead < 0 {
			return nil
		}
		cacheRead = *u.CacheRead
	}
	if u.CacheWrite != nil && (*u.CacheWrite < 0 || cacheRead > *u.Input || *u.CacheWrite > *u.Input-cacheRead) {
		return nil
	}
	if cacheRead > *u.Input {
		return nil
	}
	cost := float64(*u.Output) * *p.Output
	if u.CacheRead == nil {
		if *p.InputCacheHit != *p.InputCacheMiss {
			return nil
		}
		cost += float64(*u.Input) * *p.InputCacheMiss
	} else {
		// Cache creation/write tokens remain in total input and therefore use
		// the input-cache-miss rate; there is no separate write price.
		cacheMiss := *u.Input - cacheRead
		cost += float64(cacheRead)*(*p.InputCacheHit) + float64(cacheMiss)*(*p.InputCacheMiss)
	}
	cost /= 1e6
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
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				Refusal   string          `json:"refusal"`
				ToolCalls []ToolCall      `json:"tool_calls"`
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
			if c.Message.Role != "assistant" || (!textContent(c.Message.Content) && c.Message.Refusal == "" && len(c.Message.ToolCalls) == 0) || !validCalls(c.Message.ToolCalls) {
				return TokenUsage{}, errors.New("invalid_upstream_response")
			}
		}
	} else {
		if r.Type != "message" || r.Role != "assistant" || !messageContent("anthropic", "assistant", r.Content) {
			return TokenUsage{}, errors.New("invalid_upstream_response")
		}
	}
	return ParseUsage(protocol, r.Usage)
}
