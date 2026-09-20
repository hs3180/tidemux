package adapter

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// PromptCache remembers the last normalized prompt for a client session. It
// stores only the normalized prompt bytes, never credentials or response text.
type PromptCache struct {
	mu      sync.Mutex
	prompts map[string][]byte
}

func NewPromptCache() *PromptCache { return &PromptCache{prompts: map[string][]byte{}} }

func (c *PromptCache) Remember(protocol, model, session string, request []byte) {
	prompt := normalizedPrompt(request)
	if c == nil || len(prompt) == 0 || strings.TrimSpace(session) == "" {
		return
	}
	key := protocol + "\x00" + model + "\x00" + session
	c.mu.Lock()
	if c.prompts == nil {
		c.prompts = map[string][]byte{}
	}
	c.prompts[key] = append([]byte(nil), prompt...)
	c.mu.Unlock()
}

// LocalEstimate applies the configured price to a content-based estimate. A
// session with no previous prompt is all cache-miss; a known session reuses its
// longest common prompt prefix as cache-hit input.
func (c *PromptCache) LocalEstimate(protocol, model, session string, request, response []byte, price Price) (*float64, TokenUsage, string) {
	prompt := normalizedPrompt(request)
	if len(prompt) == 0 {
		return nil, TokenUsage{}, ""
	}
	hitBytes := 0
	key := ""
	if c != nil && strings.TrimSpace(session) != "" {
		key = protocol + "\x00" + model + "\x00" + session
		c.mu.Lock()
		if c.prompts == nil {
			c.prompts = map[string][]byte{}
		}
		previous := c.prompts[key]
		hitBytes = commonPrefix(previous, prompt)
		c.prompts[key] = append([]byte(nil), prompt...)
		c.mu.Unlock()
	}
	if hitBytes > 0 && price.CacheRead == nil {
		// Without a cache-hit rate, price the whole input as cache-miss rather
		// than inventing a discount.
		hitBytes = 0
	}
	hit := estimateTokens(prompt[:hitBytes])
	miss := estimateTokens(prompt[hitBytes:])
	out := estimateOutputTokens(protocol, response)
	usage := TokenUsage{Input: int64Ptr(hit + miss), Output: int64Ptr(out), CacheRead: int64Ptr(hit)}
	cost := price.Estimate(protocol, usage)
	if cost == nil {
		return nil, usage, ""
	}
	return cost, usage, "local_estimated_cache_prefix"
}

func SessionID(protocol string, body []byte) string {
	var in Input
	if json.Unmarshal(body, &in) == nil && protocol == "anthropic" && in.Metadata != nil {
		return strings.TrimSpace(in.Metadata.UserID)
	}
	return ""
}

func normalizedPrompt(body []byte) []byte {
	var object map[string]any
	if json.Unmarshal(body, &object) != nil {
		return nil
	}
	for _, key := range []string{"model", "stream", "stream_options", "max_tokens", "max_completion_tokens", "temperature", "top_p", "stop", "stop_sequences", "metadata"} {
		delete(object, key)
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil
	}
	return encoded
}

func commonPrefix(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	for i > 0 && i < len(a) && i < len(b) && !utf8.RuneStart(a[i]) {
		i--
	}
	return i
}

// estimateTokens is deliberately a model-independent fallback. It is used
// only when the provider did not return usage; provider usage always wins.
func estimateTokens(data []byte) int64 {
	if len(data) == 0 {
		return 0
	}
	var tokens int64
	asciiRun := 0
	flush := func() {
		if asciiRun > 0 {
			tokens += int64((asciiRun + 3) / 4)
			asciiRun = 0
		}
	}
	for len(data) > 0 {
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size == 1 {
			flush()
			tokens++
			data = data[1:]
			continue
		}
		data = data[size:]
		if r < 128 {
			switch {
			case unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '\t' || r == '\n':
				asciiRun++
			default:
				flush()
				tokens++
			}
		} else {
			flush()
			tokens++
		}
	}
	flush()
	if tokens == 0 {
		return 1
	}
	return tokens
}

func estimateOutputTokens(protocol string, data []byte) int64 {
	if len(data) == 0 {
		return 0
	}
	var text bytes.Buffer
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "data:") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if line != "[DONE]" {
				appendOutputFields(&text, []byte(line))
			}
		}
	}
	if text.Len() == 0 {
		appendOutputFields(&text, data)
	}
	return estimateTokens(text.Bytes())
}

func appendOutputFields(out *bytes.Buffer, data []byte) {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return
	}
	var walk func(any, string)
	walk = func(v any, key string) {
		switch value := v.(type) {
		case map[string]any:
			for k, child := range value {
				walk(child, strings.ToLower(k))
			}
		case []any:
			for _, child := range value {
				walk(child, key)
			}
		case string:
			if key == "content" || key == "text" || key == "thinking" || key == "reasoning_content" || key == "arguments" || key == "input_json" || key == "output_text" {
				out.WriteString(value)
			}
		}
	}
	walk(value, "")
}

func int64Ptr(v int64) *int64 { return &v }
