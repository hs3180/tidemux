package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPrepareRequestOpenAIToAnthropic(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"be concise"},{"role":"user","content":"read it"},{"role":"assistant","content":null,"tool_calls":[{"id":"call1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call1","content":"OK"}],"max_completion_tokens":32,"stop":"DONE","tools":[{"type":"function","function":{"name":"read_file","description":"Read a file","parameters":{"type":"object"}}}],"tool_choice":"required"}`)
	encoded, model, err := PrepareRequest("openai", "anthropic", body, "fallback", 0)
	if err != nil || model != "m" {
		t.Fatalf("model=%q err=%v body=%s", model, err, encoded)
	}
	var got struct {
		Model         string              `json:"model"`
		System        string              `json:"system"`
		Messages      []translatedMessage `json:"messages"`
		MaxTokens     int64               `json:"max_tokens"`
		StopSequences []string            `json:"stop_sequences"`
		Tools         []anthropicTool     `json:"tools"`
		ToolChoice    struct {
			Type string `json:"type"`
		} `json:"tool_choice"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "m" || got.System != "be concise" || got.MaxTokens != 32 || strings.Join(got.StopSequences, ",") != "DONE" || len(got.Tools) != 1 || got.Tools[0].Name != "read_file" || got.ToolChoice.Type != "any" {
		t.Fatalf("translated request=%s", encoded)
	}
	if len(got.Messages) != 3 || got.Messages[0].Role != "user" || got.Messages[1].Role != "assistant" || got.Messages[2].Role != "user" {
		t.Fatalf("translated messages=%s", encoded)
	}
	if !strings.Contains(string(got.Messages[1].Content), `"tool_use"`) || !strings.Contains(string(got.Messages[2].Content), `"tool_result"`) {
		t.Fatalf("tool messages=%s", encoded)
	}
}

func TestPrepareRequestAnthropicToOpenAI(t *testing.T) {
	body := []byte(`{"model":"m","system":[{"type":"text","text":"be concise"}],"messages":[{"role":"user","content":"read it"},{"role":"assistant","content":[{"type":"tool_use","id":"call1","name":"read_file","input":{"path":"a"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","content":"OK"}]}],"max_tokens":32,"stop_sequences":["DONE"],"tools":[{"name":"read_file","description":"Read a file","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"read_file","disable_parallel_tool_use":true},"metadata":{"user_id":"user-1"},"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`)
	encoded, model, err := PrepareRequest("anthropic", "openai", body, "fallback", 0)
	if err != nil || model != "m" {
		t.Fatalf("model=%q err=%v body=%s", model, err, encoded)
	}
	var got struct {
		Model             string          `json:"model"`
		Messages          []Message       `json:"messages"`
		MaxTokens         int64           `json:"max_tokens"`
		Stop              []string        `json:"stop"`
		Tools             []Tool          `json:"tools"`
		ToolChoice        json.RawMessage `json:"tool_choice"`
		ParallelToolCalls *bool           `json:"parallel_tool_calls"`
		ReasoningEffort   string          `json:"reasoning_effort"`
		User              string          `json:"user"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "m" || len(got.Messages) != 4 || got.Messages[0].Role != "system" || got.Messages[1].Role != "user" || got.Messages[2].Role != "assistant" || got.Messages[3].Role != "tool" || got.MaxTokens != 32 || strings.Join(got.Stop, ",") != "DONE" || len(got.Tools) != 1 || got.Tools[0].Function == nil || got.Tools[0].Function.Name != "read_file" || !strings.Contains(string(got.ToolChoice), `"type":"function"`) || !strings.Contains(string(got.ToolChoice), `"name":"read_file"`) || got.ParallelToolCalls == nil || *got.ParallelToolCalls || got.User != "user-1" {
		t.Fatalf("translated request=%s", encoded)
	}
	if !strings.Contains(string(encoded), `"response_format"`) || len(got.Messages[2].ToolCalls) != 1 {
		t.Fatalf("translated request lost fields=%s", encoded)
	}
}

func TestTranslateAnthropicResponseToOpenAI(t *testing.T) {
	data := []byte(`{"id":"msg1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"done"},{"type":"tool_use","id":"call1","name":"read_file","input":{"path":"a"}}],"stop_reason":"tool_use","usage":{"input_tokens":3,"cache_read_input_tokens":2,"output_tokens":4}}`)
	encoded, err := TranslateResponse("anthropic", "openai", data, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content  *string    `json:"content"`
				ToolCall []ToolCall `json:"tool_calls"`
			} `json:"message"`
			Finish string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got.Object != "chat.completion" || len(got.Choices) != 1 || got.Choices[0].Message.Content == nil || *got.Choices[0].Message.Content != "done" || len(got.Choices[0].Message.ToolCall) != 1 || got.Choices[0].Finish != "tool_calls" || got.Usage["prompt_tokens"] != float64(5) || got.Usage["completion_tokens"] != float64(4) {
		t.Fatalf("translated response=%s", encoded)
	}
}

func TestTranslateOpenAIResponseToAnthropic(t *testing.T) {
	data := []byte(`{"id":"chat1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done","tool_calls":[{"id":"call1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":4}}`)
	encoded, err := TranslateResponse("openai", "anthropic", data, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Type       string         `json:"type"`
		Role       string         `json:"role"`
		Content    []contentBlock `json:"content"`
		StopReason string         `json:"stop_reason"`
		Usage      map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "message" || got.Role != "assistant" || len(got.Content) != 2 || got.Content[0].Type != "text" || got.Content[1].Type != "tool_use" || got.Content[1].Name != "read_file" || got.StopReason != "tool_use" || got.Usage["input_tokens"] != float64(5) || got.Usage["output_tokens"] != float64(4) {
		t.Fatalf("translated response=%s", encoded)
	}
}

func TestTranslateAnthropicStreamToOpenAI(t *testing.T) {
	translator := newAnthropicStreamTranslator("m")
	frames := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg1\",\"model\":\"m\",\"role\":\"assistant\",\"usage\":{\"input_tokens\":3}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n",
	}
	var output strings.Builder
	for _, frame := range frames {
		converted, err := translator.frame([]byte(frame))
		if err != nil {
			t.Fatal(err)
		}
		output.Write(converted)
	}
	terminal, err := translator.terminal(nil)
	if err != nil {
		t.Fatal(err)
	}
	output.Write(terminal)
	result := output.String()
	if !strings.Contains(result, `"role":"assistant"`) || !strings.Contains(result, `"content":"hi"`) || !strings.Contains(result, `"finish_reason":"stop"`) || !strings.Contains(result, "data: [DONE]") || strings.Contains(result, "event:") {
		t.Fatalf("translated stream=%s", result)
	}
}

func TestTranslateOpenAIStreamToAnthropic(t *testing.T) {
	translator := newOpenAIStreamTranslator("m")
	frames := []string{
		`data: {"id":"chat1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"chat1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"chat1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}` + "\n\n",
	}
	var output strings.Builder
	for _, frame := range frames {
		converted, err := translator.frame([]byte(frame))
		if err != nil {
			t.Fatal(err)
		}
		output.Write(converted)
	}
	terminal, err := translator.terminal([]byte("data: [DONE]\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	output.Write(terminal)
	result := output.String()
	if !strings.Contains(result, "event: message_start") || !strings.Contains(result, `"text":"hi","type":"text_delta"`) || !strings.Contains(result, `"stop_reason":"end_turn"`) || !strings.Contains(result, `"output_tokens":2`) || !strings.Contains(result, "event: message_stop") || strings.Contains(result, "[DONE]") {
		t.Fatalf("translated stream=%s", result)
	}
}
