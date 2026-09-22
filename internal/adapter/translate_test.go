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
