package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hs3180/tidemux/internal/ledger"
	"github.com/hs3180/tidemux/internal/limiter"
)

const defaultDeepSeekBaseURL = "https://api.deepseek.com"

// DeepSeekConfig contains the provider boundary configuration. API keys are
// supplied by the caller (the runtime should source them from Keychain), and
// are never copied into ledger records or error messages.
type DeepSeekConfig struct {
	APIKey           string
	BaseURL          string
	HTTPClient       *http.Client
	Ledger           *ledger.Ledger
	Limiter          *limiter.ConcurrencyGate
	PriceInPerMTok   float64
	PriceOutPerMTok  float64
	PriceHitPerMTok  float64
	PriceMissPerMTok float64
}

// DeepSeekClient is a deliberately small OpenAI-compatible provider client.
// Scheduling, concurrency and shaping remain outside the provider boundary.
type DeepSeekClient struct {
	config DeepSeekConfig
}

// UpstreamError preserves only a provider HTTP status for the gateway. It
// deliberately does not retain upstream response content, which can contain
// request-derived or provider-sensitive details.
type UpstreamError struct{ StatusCode int }

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("deepseek request failed with status %d", e.StatusCode)
}

type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Stream   bool          `json:"stream,omitempty"`
}

type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type usagePayload struct {
	PromptTokens          int64 `json:"prompt_tokens"`
	CompletionTokens      int64 `json:"completion_tokens"`
	PromptCacheHitTokens  int64 `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens int64 `json:"prompt_cache_miss_tokens"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   usagePayload `json:"usage"`
}

// NewDeepSeekClient validates the minimum provider boundary configuration.
func NewDeepSeekClient(config DeepSeekConfig) (*DeepSeekClient, error) {
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, fmt.Errorf("deepseek API key is required")
	}
	if config.BaseURL == "" {
		config.BaseURL = defaultDeepSeekBaseURL
	}
	config.BaseURL = strings.TrimRight(config.BaseURL, "/")
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	return &DeepSeekClient{config: config}, nil
}

// Chat sends one non-streaming chat completion and records the provider
// boundary result in the append-only ledger when one is configured.
func (c *DeepSeekClient) Chat(ctx context.Context, request ChatRequest) (ChatChoice, error) {
	if request.Stream {
		return ChatChoice{}, fmt.Errorf("deepseek streaming is not implemented in v0")
	}
	started := time.Now()
	var admission limiter.Admission
	if c.config.Limiter != nil {
		var err error
		admission, err = c.config.Limiter.Acquire(ctx)
		if err != nil {
			return ChatChoice{}, fmt.Errorf("wait for DeepSeek concurrency slot: %w", err)
		}
		defer c.config.Limiter.Release()
	}
	body, err := json.Marshal(request)
	if err != nil {
		return ChatChoice{}, fmt.Errorf("encode deepseek request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.config.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatChoice{}, fmt.Errorf("build deepseek request: %w", err)
	}
	httpRequest.Header.Set("Authorization", "Bearer "+c.config.APIKey)
	httpRequest.Header.Set("Content-Type", "application/json")
	response, err := c.config.HTTPClient.Do(httpRequest)
	if err != nil {
		return ChatChoice{}, fmt.Errorf("call deepseek: %w", err)
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		return ChatChoice{}, fmt.Errorf("read deepseek response: %w", readErr)
	}
	var parsed chatResponse
	decodeErr := json.Unmarshal(responseBody, &parsed)
	status := "ok"
	var shaping *string
	var events []ledger.Event
	if admission.Queued {
		status = "queued_ok"
		value := "concurrency_queue"
		shaping = &value
		detail := admission.QueueWait.String()
		events = append(events, ledger.Event{TimestampMS: started.UnixMilli(), Type: "queue_wait", Reason: "并发上限，已排队", Detail: &detail})
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		status = "error"
	}
	if c.config.Ledger != nil {
		record := Record{
			TimestampMS:      started.UnixMilli(),
			Model:            request.Model,
			Status:           status,
			Usage:            Usage{PromptTokens: parsed.Usage.PromptTokens, CompletionTokens: parsed.Usage.CompletionTokens, TotalLatencyMS: int64Pointer(time.Since(started).Milliseconds())},
			PriceInPerMTok:   c.config.PriceInPerMTok,
			PriceOutPerMTok:  c.config.PriceOutPerMTok,
			PriceHitPerMTok:  c.config.PriceHitPerMTok,
			PriceMissPerMTok: c.config.PriceMissPerMTok,
			Shaping:          shaping,
			Events:           events,
		}
		if parsed.Usage.PromptCacheHitTokens > 0 || parsed.Usage.PromptCacheMissTokens > 0 {
			hit, miss := parsed.Usage.PromptCacheHitTokens, parsed.Usage.PromptCacheMissTokens
			record.Usage.CacheHitTokens, record.Usage.CacheMissTokens = &hit, &miss
		}
		if status == "error" {
			code := fmt.Sprintf("%d", response.StatusCode)
			reason := "DeepSeek provider returned an error"
			record.ErrorCode, record.ErrorReason = &code, &reason
		}
		if _, ledgerErr := AppendLedger(ctx, c.config.Ledger, record); ledgerErr != nil {
			return ChatChoice{}, fmt.Errorf("record deepseek request: %w", ledgerErr)
		}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return ChatChoice{}, &UpstreamError{StatusCode: response.StatusCode}
	}
	if decodeErr != nil {
		return ChatChoice{}, fmt.Errorf("decode deepseek response: %w", decodeErr)
	}
	if len(parsed.Choices) == 0 {
		return ChatChoice{}, fmt.Errorf("deepseek response contained no choices")
	}
	return parsed.Choices[0], nil
}

func int64Pointer(v int64) *int64 { return &v }
