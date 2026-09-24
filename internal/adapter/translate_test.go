package adapter

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareRequestOpenAIToAnthropic(t *testing.T) {
	body := []byte(`{"model":"m","user":"user-1","messages":[{"role":"system","content":"be concise"},{"role":"user","content":"read it"},{"role":"assistant","content":null,"tool_calls":[{"id":"call1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call1","content":"OK"}],"max_completion_tokens":32,"stop":"DONE","tools":[{"type":"function","function":{"name":"read_file","description":"Read a file","parameters":{"type":"object"}}}],"tool_choice":"required","parallel_tool_calls":false}`)
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
			Type                 string `json:"type"`
			DisableParallelTools bool   `json:"disable_parallel_tool_use"`
		} `json:"tool_choice"`
		Metadata *Metadata `json:"metadata"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "m" || got.System != "be concise" || got.MaxTokens != 32 || strings.Join(got.StopSequences, ",") != "DONE" || len(got.Tools) != 1 || got.Tools[0].Name != "read_file" || got.ToolChoice.Type != "any" || !got.ToolChoice.DisableParallelTools || got.Metadata == nil || got.Metadata.UserID != "user-1" {
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

func TestCrossProtocolRequestLossIsReportedAndNativeRequestIsPreserved(t *testing.T) {
	openAIRequest := []byte(`{"model":"m","messages":[{"role":"user","content":"think carefully"}],"max_completion_tokens":64,"reasoning_effort":"high"}`)
	converted, _, warnings, err := PrepareRequestWithWarnings("openai", "anthropic", openAIRequest, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(warnings, []string{"reasoning_effort"}) {
		t.Fatalf("warnings=%v", warnings)
	}
	if strings.Contains(string(converted), "reasoning_effort") {
		t.Fatalf("unmapped effort was forwarded: %s", converted)
	}
	native, _, warnings, err := PrepareRequestWithWarnings("openai", "openai", openAIRequest, "", 0)
	if err != nil || len(warnings) != 0 || !strings.Contains(string(native), `"reasoning_effort":"high"`) {
		t.Fatalf("native request changed: warnings=%v err=%v body=%s", warnings, err, native)
	}

	anthropicRequest := []byte(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"think carefully"}],"output_config":{"effort":"high"}}`)
	converted, _, warnings, err = PrepareRequestWithWarnings("anthropic", "openai", anthropicRequest, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(warnings, []string{"output_config.effort"}) || strings.Contains(string(converted), "reasoning_effort") {
		t.Fatalf("unmapped Anthropic effort not reported/dropped: warnings=%v body=%s", warnings, converted)
	}
}

func TestOpenAIRequestMapsAnthropicThinkingAndReportsOtherLosses(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"early instruction"},{"role":"user","content":"hello"},{"role":"developer","content":[{"type":"text","text":"later instruction"}]}],"thinking":{"type":"enabled","budget_tokens":2048},"response_format":{"type":"json_schema","json_schema":{"name":"answer","description":"an answer","schema":{"type":"object"},"strict":true}},"tools":[{"type":"function","function":{"name":"lookup","strict":true,"parameters":{"type":"object"}}}]}`)
	encoded, _, warnings, err := PrepareRequestWithWarnings("openai", "anthropic", body, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"messages[2].role", "response_format.json_schema.description", "response_format.json_schema.name", "response_format.json_schema.strict", "tools[0].function.strict"}
	if !reflect.DeepEqual(warnings, want) {
		t.Fatalf("warnings=%v want=%v", warnings, want)
	}
	var got struct {
		Thinking *Thinking `json:"thinking"`
		System   []struct {
			Text string `json:"text"`
		} `json:"system"`
		OutputConfig *OutputConfig                `json:"output_config"`
		Tools        []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if got.Thinking == nil || got.Thinking.Type != "enabled" || got.Thinking.BudgetTokens == nil || *got.Thinking.BudgetTokens != 2048 {
		t.Fatalf("Anthropic thinking control was not preserved: %s", encoded)
	}
	if len(got.System) != 2 || got.System[0].Text != "early instruction" || got.System[1].Text != "later instruction" {
		t.Fatalf("system instruction order changed: %s", encoded)
	}
	if len(got.Tools) != 1 {
		t.Fatalf("tool declaration missing: %s", encoded)
	}
	if got.OutputConfig == nil || got.OutputConfig.Format == nil || string(got.OutputConfig.Format.Schema) != `{"type":"object"}` {
		t.Fatalf("structured output schema was not preserved: %s", encoded)
	}
	if _, present := got.Tools[0]["strict"]; present {
		t.Fatalf("unsupported strict flag was forwarded: %s", encoded)
	}
}

func TestAnthropicThinkingBudgetMustFitProviderTokenLimit(t *testing.T) {
	for _, test := range []struct {
		name, body string
	}{
		{
			name: "below minimum",
			body: `{"model":"m","max_completion_tokens":4096,"messages":[{"role":"user","content":"hello"}],"thinking":{"type":"enabled","budget_tokens":512}}`,
		},
		{
			name: "equal to output limit",
			body: `{"model":"m","max_completion_tokens":2048,"messages":[{"role":"user","content":"hello"}],"thinking":{"type":"enabled","budget_tokens":2048}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, _, err := PrepareRequestWithWarnings("openai", "anthropic", []byte(test.body), "", 0); err == nil || ValidationParameter(err) != "thinking.budget_tokens" {
				t.Fatalf("err=%v param=%q", err, ValidationParameter(err))
			}
		})
	}
	anthropic := []byte(`{"model":"m","max_tokens":1024,"messages":[{"role":"user","content":"hello"}],"thinking":{"type":"enabled","budget_tokens":1024}}`)
	if _, _, err := Request("anthropic", anthropic, ""); err == nil || ValidationParameter(err) != "thinking.budget_tokens" {
		t.Fatalf("native Anthropic budget error=%v param=%q", err, ValidationParameter(err))
	}
}

func TestAnthropicConversionReportsCacheAndToolErrorLosses(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":64,"system":[{"type":"text","text":"rules","cache_control":{"type":"ephemeral"}}],"tools":[{"name":"read_file","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call1","is_error":true,"content":[{"type":"text","text":"failed","cache_control":{"type":"ephemeral"}}]}]}],"output_config":{"effort":"high"}}`)
	encoded, _, warnings, err := PrepareRequestWithWarnings("anthropic", "openai", body, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"messages[0].content[0].content[0].cache_control",
		"messages[0].content[0].is_error",
		"output_config.effort",
		"system[0].cache_control",
		"tools[0].cache_control",
	}
	if !reflect.DeepEqual(warnings, want) {
		t.Fatalf("warnings=%v want=%v", warnings, want)
	}
	if strings.Contains(string(encoded), "cache_control") || strings.Contains(string(encoded), "output_config") {
		t.Fatalf("Anthropic-only controls were forwarded: %s", encoded)
	}
	if !strings.Contains(string(encoded), "Tool error: failed") {
		t.Fatalf("tool error signal was not represented in content: %s", encoded)
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

func TestReasoningOnlyOpenAIResponseIsNotTranslatedToEmptySuccess(t *testing.T) {
	data := []byte(`{"id":"chat1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"reasoning_content":"analysis"},"finish_reason":"length"}],"usage":{"prompt_tokens":5,"completion_tokens":16}}`)
	if _, err := ValidateResponse("openai", data); err != nil {
		t.Fatalf("valid reasoning-only provider response rejected: %v", err)
	}
	_, _, err := TranslateResponseWithWarnings("openai", "anthropic", data, "")
	var conversion *TranslationError
	if !errors.As(err, &conversion) || conversion.Code != "unrepresentable_reasoning_only_response" || conversion.Field != "choices[0].message.reasoning_content" {
		t.Fatalf("translation error=%#v", err)
	}
}

func TestAnthropicThinkingResponseMapsToOpenAIReasoningWithoutSignature(t *testing.T) {
	data := []byte(`{"id":"msg1","type":"message","role":"assistant","model":"m","content":[{"type":"thinking","thinking":"analysis","signature":"provider-signature"}],"stop_reason":"max_tokens","usage":{"input_tokens":2,"output_tokens":8}}`)
	encoded, warnings, err := TranslateResponseWithWarnings("anthropic", "openai", data, "")
	if err != nil || len(warnings) != 0 {
		t.Fatalf("warnings=%v err=%v", warnings, err)
	}
	var got struct {
		Choices []struct {
			Message struct {
				Content          any    `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Choices) != 1 || got.Choices[0].Message.Content != nil || got.Choices[0].Message.ReasoningContent != "analysis" || got.Choices[0].FinishReason != "length" || strings.Contains(string(encoded), "signature") {
		t.Fatalf("reasoning response was altered incorrectly: %s", encoded)
	}
}

func TestCrossProtocolFinishReasonLossIsExplicit(t *testing.T) {
	openAI := []byte(`{"id":"chat1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"filtered"},"finish_reason":"content_filter"}]}`)
	_, _, err := TranslateResponseWithWarnings("openai", "anthropic", openAI, "")
	var conversion *TranslationError
	if !errors.As(err, &conversion) || conversion.Field != "choices[0].finish_reason" {
		t.Fatalf("OpenAI finish reason error=%#v", err)
	}

	anthropic := []byte(`{"id":"msg1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"continuing"}],"stop_reason":"pause_turn"}`)
	_, _, err = TranslateResponseWithWarnings("anthropic", "openai", anthropic, "")
	if !errors.As(err, &conversion) || conversion.Field != "stop_reason" {
		t.Fatalf("Anthropic stop reason error=%#v", err)
	}

	anthropic = []byte(`{"id":"msg1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"done"}],"stop_reason":"stop_sequence","stop_sequence":"END"}`)
	encoded, warnings, err := TranslateResponseWithWarnings("anthropic", "openai", anthropic, "")
	if err != nil || !reflect.DeepEqual(warnings, []string{"stop_sequence"}) || !strings.Contains(string(encoded), `"finish_reason":"stop"`) {
		t.Fatalf("stop sequence conversion warnings=%v err=%v body=%s", warnings, err, encoded)
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

func TestReasoningOnlyOpenAIStreamReturnsTranslationError(t *testing.T) {
	translator := newOpenAIStreamTranslator("m")
	frames := []string{
		`data: {"id":"chat1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"chat1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"reasoning_content":"analysis"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"chat1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n",
	}
	for _, frame := range frames {
		if _, err := translator.frame([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	_, err := translator.terminal(nil)
	var conversion *TranslationError
	if !errors.As(err, &conversion) || conversion.Field != "choices[0].delta.reasoning_content" {
		t.Fatalf("translation error=%#v", err)
	}
	if !reflect.DeepEqual(translator.warnings(), []string{"choices[0].delta.reasoning_content"}) {
		t.Fatalf("warnings=%v", translator.warnings())
	}
}

func TestOpenAIStreamUnsupportedFinishReasonReturnsTranslationError(t *testing.T) {
	translator := newOpenAIStreamTranslator("m")
	translator.started = true
	translator.finished = true
	translator.finishReason = ptr("content_filter")
	_, err := translator.terminal(nil)
	var conversion *TranslationError
	if !errors.As(err, &conversion) || conversion.Field != "choices[0].finish_reason" {
		t.Fatalf("translation error=%#v", err)
	}
}
