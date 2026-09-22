package adapter

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

const defaultAnthropicMaxTokens int64 = 4096

// PrepareRequest validates the client wire format and converts it to the
// configured provider wire format. Both client protocols are available at
// the gateway boundary; the resolved provider protocol selects the upstream
// wire format.
func PrepareRequest(clientProtocol, providerProtocol string, data []byte, defaultModel string, maxOutputTokens int64) ([]byte, string, error) {
	if clientProtocol == providerProtocol {
		return data, firstNonEmpty(modelFromBody(data), defaultModel), nil
	}
	switch {
	case clientProtocol == "openai" && providerProtocol == "anthropic":
		return translateOpenAIRequest(data, defaultModel, maxOutputTokens)
	case clientProtocol == "anthropic" && providerProtocol == "openai":
		return translateAnthropicRequest(data, defaultModel)
	default:
		return nil, "", errors.New("unsupported_protocol_translation")
	}
}

func modelFromBody(data []byte) string {
	var value struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(data, &value) != nil {
		return ""
	}
	return value.Model
}

type anthropicRequest struct {
	Model         string                 `json:"model"`
	Messages      []translatedMessage    `json:"messages"`
	MaxTokens     int64                  `json:"max_tokens"`
	Stream        bool                   `json:"stream,omitempty"`
	System        json.RawMessage        `json:"system,omitempty"`
	Temperature   *float64               `json:"temperature,omitempty"`
	TopP          *float64               `json:"top_p,omitempty"`
	StopSequences []string               `json:"stop_sequences,omitempty"`
	Tools         []anthropicTool        `json:"tools,omitempty"`
	ToolChoice    json.RawMessage        `json:"tool_choice,omitempty"`
	OutputConfig  *anthropicOutputConfig `json:"output_config,omitempty"`
}

type translatedMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicOutputConfig struct {
	Format *anthropicOutputFormat `json:"format,omitempty"`
}

type anthropicOutputFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

func translateOpenAIRequest(data []byte, defaultModel string, maxOutputTokens int64) ([]byte, string, error) {
	validated, model, err := Request("openai", data, defaultModel)
	if err != nil {
		return nil, "", err
	}
	var in Input
	if err := StrictJSON(validated, &in); err != nil {
		return nil, "", errors.New("invalid_request")
	}
	if in.ReasoningEffort != "" {
		return nil, "", errors.New("reasoning_effort")
	}
	out := anthropicRequest{
		Model:       in.Model,
		MaxTokens:   defaultAnthropicMaxTokens,
		Stream:      in.Stream,
		Temperature: in.Temperature,
		TopP:        in.TopP,
	}
	if maxOutputTokens > 0 {
		out.MaxTokens = maxOutputTokens
	}
	if in.MaxCompletionTokens != nil {
		out.MaxTokens = *in.MaxCompletionTokens
	} else if in.MaxTokens != nil {
		out.MaxTokens = *in.MaxTokens
	}

	system, messages, err := translateMessages(in.Messages)
	if err != nil {
		return nil, "", err
	}
	out.System = system
	out.Messages = messages
	if out.StopSequences, err = stopSequences(in.Stop); err != nil {
		return nil, "", errors.New("stop")
	}
	for _, tool := range in.Tools {
		if tool.Function == nil {
			return nil, "", errors.New("tools")
		}
		schema := tool.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{}`)
		}
		out.Tools = append(out.Tools, anthropicTool{Name: tool.Function.Name, Description: tool.Function.Description, InputSchema: schema})
	}
	if len(in.Tools) > 0 && in.ToolChoice != nil {
		out.ToolChoice, err = translateToolChoice(in.ToolChoice)
		if err != nil {
			return nil, "", errors.New("tools")
		}
	}
	if in.ResponseFormat != nil {
		format := &anthropicOutputFormat{Type: "json_schema"}
		if in.ResponseFormat.Type == "json_object" {
			format.Schema = json.RawMessage(`{"type":"object"}`)
		} else if in.ResponseFormat.JSONSchema != nil {
			format.Schema = in.ResponseFormat.JSONSchema.Schema
		} else {
			return nil, "", errors.New("response_format")
		}
		out.OutputConfig = &anthropicOutputConfig{Format: format}
	}
	encoded, err := json.Marshal(out)
	return encoded, model, err
}

type translatedOpenAIRequest struct {
	Model             string          `json:"model"`
	Messages          []Message       `json:"messages"`
	MaxTokens         *int64          `json:"max_tokens,omitempty"`
	Stream            bool            `json:"stream,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
	TopP              *float64        `json:"top_p,omitempty"`
	Stop              json.RawMessage `json:"stop,omitempty"`
	Tools             []Tool          `json:"tools,omitempty"`
	ToolChoice        json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool           `json:"parallel_tool_calls,omitempty"`
	ReasoningEffort   string          `json:"reasoning_effort,omitempty"`
	ResponseFormat    json.RawMessage `json:"response_format,omitempty"`
	User              string          `json:"user,omitempty"`
}

func translateAnthropicRequest(data []byte, defaultModel string) ([]byte, string, error) {
	validated, model, err := Request("anthropic", data, defaultModel)
	if err != nil {
		return nil, "", err
	}
	var in Input
	if err := StrictJSON(validated, &in); err != nil {
		return nil, "", errors.New("invalid_request")
	}
	if in.ContextManagement != nil {
		return nil, "", errors.New("context_management")
	}
	if in.Thinking != nil && in.Thinking.Type != "disabled" {
		return nil, "", errors.New("thinking")
	}
	system, messages, err := translateAnthropicMessages(in.System, in.Messages)
	if err != nil {
		return nil, "", err
	}
	out := translatedOpenAIRequest{
		Model:       in.Model,
		Messages:    messages,
		MaxTokens:   in.MaxTokens,
		Stream:      in.Stream,
		Temperature: in.Temperature,
		TopP:        in.TopP,
	}
	if system != "" {
		out.Messages = append([]Message{{Role: "system", Content: json.RawMessage(strconv.Quote(system))}}, out.Messages...)
	}
	if len(in.StopSequences) > 0 {
		if len(in.StopSequences) > 4 {
			return nil, "", errors.New("stop_sequences")
		}
		out.Stop, err = json.Marshal(in.StopSequences)
		if err != nil {
			return nil, "", errors.New("stop_sequences")
		}
	}
	for _, tool := range in.Tools {
		parameters := tool.InputSchema
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{}`)
		}
		out.Tools = append(out.Tools, Tool{Type: "function", Function: &Function{
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  parameters,
		}})
	}
	if in.ToolChoice != nil {
		out.ToolChoice, out.ParallelToolCalls, err = translateAnthropicToolChoice(in.ToolChoice)
		if err != nil {
			return nil, "", errors.New("tools")
		}
	}
	if in.Metadata != nil {
		out.User = in.Metadata.UserID
	}
	if in.OutputConfig != nil {
		if in.OutputConfig.Effort != "" {
			if in.OutputConfig.Effort == "max" {
				return nil, "", errors.New("output_config")
			}
			out.ReasoningEffort = in.OutputConfig.Effort
		}
		if in.OutputConfig.Format != nil {
			out.ResponseFormat, err = json.Marshal(map[string]any{
				"type": "json_schema",
				"json_schema": map[string]any{
					"name":   "response",
					"schema": json.RawMessage(in.OutputConfig.Format.Schema),
				},
			})
			if err != nil {
				return nil, "", errors.New("output_config")
			}
		}
	}
	encoded, err := json.Marshal(out)
	return encoded, model, err
}

func translateAnthropicMessages(system json.RawMessage, messages []Message) (string, []Message, error) {
	systemText, err := anthropicSystemText(system)
	if err != nil {
		return "", nil, err
	}
	var out []Message
	for _, message := range messages {
		if len(message.Content) == 0 || string(message.Content) == "null" {
			return "", nil, errors.New("messages")
		}
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			out = append(out, Message{Role: message.Role, Content: message.Content})
			continue
		}
		var blocks []contentBlock
		if StrictJSON(message.Content, &blocks) != nil || len(blocks) == 0 {
			return "", nil, errors.New("messages")
		}
		switch message.Role {
		case "assistant":
			translated, err := translateAnthropicAssistantBlocks(blocks)
			if err != nil {
				return "", nil, err
			}
			out = append(out, translated)
		case "user":
			translated, err := translateAnthropicUserBlocks(blocks)
			if err != nil {
				return "", nil, err
			}
			out = append(out, translated...)
		default:
			return "", nil, errors.New("messages")
		}
	}
	return systemText, out, nil
}

func anthropicSystemText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var blocks []contentBlock
	if StrictJSON(raw, &blocks) != nil || len(blocks) == 0 {
		return "", errors.New("system")
	}
	texts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		if block.Type != "text" || block.Text == nil {
			return "", errors.New("system")
		}
		texts = append(texts, *block.Text)
	}
	return strings.Join(texts, "\n"), nil
}

func translateAnthropicAssistantBlocks(blocks []contentBlock) (Message, error) {
	var text strings.Builder
	var calls []ToolCall
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text == nil {
				return Message{}, errors.New("messages")
			}
			text.WriteString(*block.Text)
		case "tool_use":
			if block.ID == "" || block.Name == "" || !object(block.Input) {
				return Message{}, errors.New("tool_calls")
			}
			call := ToolCall{ID: block.ID, Type: "function"}
			call.Function.Name = block.Name
			call.Function.Arguments = string(block.Input)
			calls = append(calls, call)
		default:
			return Message{}, errors.New("messages")
		}
	}
	message := Message{Role: "assistant"}
	if text.Len() > 0 {
		message.Content = json.RawMessage(strconv.Quote(text.String()))
	}
	message.ToolCalls = calls
	return message, nil
}

func translateAnthropicUserBlocks(blocks []contentBlock) ([]Message, error) {
	var out []Message
	var text strings.Builder
	hasText := false
	flushText := func() {
		if !hasText {
			return
		}
		out = append(out, Message{Role: "user", Content: json.RawMessage(strconv.Quote(text.String()))})
		text.Reset()
		hasText = false
	}
	for _, block := range blocks {
		switch block.Type {
		case "text":
			if block.Text == nil {
				return nil, errors.New("messages")
			}
			text.WriteString(*block.Text)
			hasText = true
		case "tool_result":
			flushText()
			content, err := anthropicToolResultText(block.Content)
			if err != nil {
				return nil, err
			}
			if block.IsError != nil && *block.IsError {
				content = "Tool error: " + content
			}
			out = append(out, Message{Role: "tool", ToolCallID: block.ToolUseID, Content: json.RawMessage(strconv.Quote(content))})
		default:
			return nil, errors.New("messages")
		}
	}
	flushText()
	if len(out) == 0 {
		return nil, errors.New("messages")
	}
	return out, nil
}

func anthropicToolResultText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var blocks []contentBlock
	if StrictJSON(raw, &blocks) != nil || len(blocks) == 0 {
		return "", errors.New("tool_result")
	}
	var result strings.Builder
	for _, block := range blocks {
		if block.Type != "text" || block.Text == nil {
			return "", errors.New("tool_result")
		}
		result.WriteString(*block.Text)
	}
	return result.String(), nil
}

func translateAnthropicToolChoice(raw json.RawMessage) (json.RawMessage, *bool, error) {
	var choice struct {
		Type               string `json:"type"`
		Name               string `json:"name"`
		DisableParallelUse *bool  `json:"disable_parallel_tool_use,omitempty"`
	}
	if StrictJSON(raw, &choice) != nil {
		return nil, nil, errors.New("tools")
	}
	var result any
	switch choice.Type {
	case "auto", "none":
		result = choice.Type
	case "any":
		result = "required"
	case "tool":
		if choice.Name == "" {
			return nil, nil, errors.New("tools")
		}
		result = map[string]any{"type": "function", "function": map[string]string{"name": choice.Name}}
	default:
		return nil, nil, errors.New("tools")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return nil, nil, err
	}
	var parallel *bool
	if choice.DisableParallelUse != nil && *choice.DisableParallelUse {
		value := false
		parallel = &value
	}
	return encoded, parallel, nil
}

func translateMessages(messages []Message) (json.RawMessage, []translatedMessage, error) {
	var systemText []string
	var systemBlocks []any
	hasSystemBlocks := false
	var out []translatedMessage
	for _, message := range messages {
		if message.Role == "system" || message.Role == "developer" {
			var text string
			if json.Unmarshal(message.Content, &text) == nil {
				systemText = append(systemText, text)
				continue
			}
			var blocks []any
			if StrictJSON(message.Content, &blocks) != nil || len(blocks) == 0 {
				return nil, nil, errors.New("system")
			}
			hasSystemBlocks = true
			systemBlocks = append(systemBlocks, blocks...)
			continue
		}
		content, err := translateMessageContent(message)
		if err != nil {
			return nil, nil, err
		}
		role := message.Role
		if role == "tool" {
			role = "user"
		}
		out = append(out, translatedMessage{Role: role, Content: content})
	}
	if len(out) == 0 {
		return nil, nil, errors.New("messages")
	}
	var system json.RawMessage
	if hasSystemBlocks || len(systemText) > 0 {
		if hasSystemBlocks {
			for _, text := range systemText {
				systemBlocks = append(systemBlocks, map[string]any{"type": "text", "text": text})
			}
			encoded, _ := json.Marshal(systemBlocks)
			system = encoded
		} else {
			system = json.RawMessage(strconv.Quote(strings.Join(systemText, "\n")))
		}
	}
	return system, out, nil
}

func translateMessageContent(message Message) (json.RawMessage, error) {
	if message.Role == "tool" {
		if message.ToolCallID == "" {
			return nil, errors.New("tool_call_id")
		}
		block := map[string]any{
			"type":        "tool_result",
			"tool_use_id": message.ToolCallID,
			"content":     json.RawMessage(message.Content),
		}
		return json.Marshal([]any{block})
	}
	if len(message.ToolCalls) == 0 {
		return message.Content, nil
	}
	var blocks []any
	if len(message.Content) > 0 && string(message.Content) != "null" {
		var text string
		if json.Unmarshal(message.Content, &text) == nil {
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		} else {
			var existing []any
			if StrictJSON(message.Content, &existing) != nil {
				return nil, errors.New("messages")
			}
			blocks = append(blocks, existing...)
		}
	}
	for _, call := range message.ToolCalls {
		var input map[string]any
		if json.Unmarshal([]byte(call.Function.Arguments), &input) != nil || input == nil {
			return nil, errors.New("tool_calls")
		}
		blocks = append(blocks, map[string]any{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Function.Name,
			"input": input,
		})
	}
	return json.Marshal(blocks)
}

func stopSequences(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return []string{single}, nil
	}
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		return nil, errors.New("stop")
	}
	return list, nil
}

func translateToolChoice(raw json.RawMessage) (json.RawMessage, error) {
	var name string
	if json.Unmarshal(raw, &name) == nil {
		switch name {
		case "auto", "none":
			return json.Marshal(map[string]string{"type": name})
		case "required":
			return json.Marshal(map[string]string{"type": "any"})
		default:
			return nil, errors.New("tools")
		}
	}
	var choice struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if StrictJSON(raw, &choice) != nil || choice.Type != "function" || choice.Function.Name == "" {
		return nil, errors.New("tools")
	}
	return json.Marshal(map[string]string{"type": "tool", "name": choice.Function.Name})
}

// TranslateResponse converts a provider response to the client wire format.
func TranslateResponse(providerProtocol, clientProtocol string, data []byte, defaultModel string) ([]byte, error) {
	if providerProtocol == clientProtocol {
		return data, nil
	}
	if providerProtocol == "openai" && clientProtocol == "anthropic" {
		return translateOpenAIResponse(data, defaultModel)
	}
	if providerProtocol != "anthropic" || clientProtocol != "openai" {
		return nil, errors.New("unsupported_protocol_translation")
	}
	var in struct {
		ID         string          `json:"id"`
		Model      string          `json:"model"`
		Role       string          `json:"role"`
		Content    []contentBlock  `json:"content"`
		StopReason *string         `json:"stop_reason"`
		Usage      json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(data, &in) != nil || in.Role != "assistant" || len(in.Content) == 0 {
		return nil, errors.New("invalid_upstream_response")
	}
	var text strings.Builder
	var reasoning strings.Builder
	var calls []map[string]any
	for _, block := range in.Content {
		switch block.Type {
		case "text":
			if block.Text != nil {
				text.WriteString(*block.Text)
			}
		case "thinking":
			if block.Thinking != nil {
				reasoning.WriteString(*block.Thinking)
			}
		case "tool_use":
			if block.ID == "" || block.Name == "" || len(block.Input) == 0 {
				return nil, errors.New("invalid_upstream_response")
			}
			calls = append(calls, map[string]any{
				"id":   block.ID,
				"type": "function",
				"function": map[string]string{
					"name":      block.Name,
					"arguments": string(block.Input),
				},
			})
		}
	}
	message := map[string]any{"role": "assistant", "content": nil}
	if text.Len() > 0 {
		message["content"] = text.String()
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	model := in.Model
	if model == "" {
		model = defaultModel
	}
	choice := map[string]any{"index": 0, "message": message, "finish_reason": translateStopReason(in.StopReason)}
	out := map[string]any{"id": in.ID, "object": "chat.completion", "model": model, "choices": []any{choice}}
	if usage := translateAnthropicUsage(in.Usage, nil); usage != nil {
		out["usage"] = usage
	}
	return json.Marshal(out)
}

func translateOpenAIResponse(data []byte, defaultModel string) ([]byte, error) {
	var in struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				Refusal   string          `json:"refusal"`
				ToolCalls []ToolCall      `json:"tool_calls"`
			} `json:"message"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal(data, &in) != nil || in.ID == "" || len(in.Choices) != 1 {
		return nil, errors.New("invalid_upstream_response")
	}
	choice := in.Choices[0]
	if choice.Message.Role != "assistant" {
		return nil, errors.New("invalid_upstream_response")
	}
	content, err := translateOpenAIResponseContent(choice.Message.Content, choice.Message.Refusal, choice.Message.ToolCalls)
	if err != nil {
		return nil, err
	}
	model := firstNonEmpty(in.Model, defaultModel)
	if model == "" {
		return nil, errors.New("invalid_upstream_response")
	}
	out := map[string]any{
		"id":            in.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   translateOpenAIStopReason(choice.FinishReason),
		"stop_sequence": nil,
	}
	if usage := translateOpenAIUsage(in.Usage); usage != nil {
		out["usage"] = usage
	}
	return json.Marshal(out)
}

func translateOpenAIResponseContent(raw json.RawMessage, refusal string, calls []ToolCall) ([]any, error) {
	var out []any
	if len(raw) > 0 && string(raw) != "null" {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			if text != "" {
				out = append(out, map[string]string{"type": "text", "text": text})
			}
		} else {
			var parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
				return nil, errors.New("invalid_upstream_response")
			}
			for _, part := range parts {
				if part.Type != "text" {
					return nil, errors.New("invalid_upstream_response")
				}
				out = append(out, map[string]string{"type": "text", "text": part.Text})
			}
		}
	}
	if refusal != "" {
		out = append(out, map[string]string{"type": "text", "text": refusal})
	}
	for _, call := range calls {
		if call.ID == "" || call.Function.Name == "" {
			return nil, errors.New("invalid_upstream_response")
		}
		var input map[string]any
		if json.Unmarshal([]byte(call.Function.Arguments), &input) != nil || input == nil {
			return nil, errors.New("invalid_upstream_response")
		}
		out = append(out, map[string]any{"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": input})
	}
	if len(out) == 0 {
		return nil, errors.New("invalid_upstream_response")
	}
	return out, nil
}

func translateOpenAIStopReason(reason *string) string {
	if reason == nil || *reason == "" {
		return "end_turn"
	}
	switch *reason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func translateOpenAIUsage(raw json.RawMessage) map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	usage, err := ParseUsage("openai", raw)
	if err != nil || (usage.Input == nil && usage.Output == nil) {
		return nil
	}
	out := map[string]any{}
	if usage.Input != nil {
		out["input_tokens"] = *usage.Input
	}
	if usage.Output != nil {
		out["output_tokens"] = *usage.Output
	}
	if usage.CacheRead != nil {
		out["cache_read_input_tokens"] = *usage.CacheRead
	}
	if usage.CacheWrite != nil {
		out["cache_creation_input_tokens"] = *usage.CacheWrite
	}
	return out
}

func translateStopReason(reason *string) any {
	if reason == nil || *reason == "" {
		return nil
	}
	switch *reason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

func translateAnthropicUsage(raw json.RawMessage, inputFallback *int64) map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		if inputFallback == nil {
			return nil
		}
		raw = json.RawMessage(`{}`)
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil || values == nil {
		return nil
	}
	get := func(name string) (int64, bool) {
		value, ok := values[name]
		if !ok {
			return 0, false
		}
		var n int64
		if json.Unmarshal(value, &n) != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	input, inputOK := get("input_tokens")
	if !inputOK && inputFallback != nil {
		input, inputOK = *inputFallback, true
	}
	cacheRead, hasCacheRead := get("cache_read_input_tokens")
	cacheWrite, hasCacheWrite := get("cache_creation_input_tokens")
	if inputOK {
		if hasCacheRead {
			input += cacheRead
		}
		if hasCacheWrite {
			input += cacheWrite
		}
	}
	output, outputOK := get("output_tokens")
	if !inputOK && !outputOK {
		return nil
	}
	result := map[string]any{}
	if inputOK {
		result["prompt_tokens"] = input
	}
	if outputOK {
		result["completion_tokens"] = output
	}
	if hasCacheRead {
		result["prompt_tokens_details"] = map[string]any{"cached_tokens": cacheRead}
	}
	if inputOK || outputOK {
		prompt, _ := result["prompt_tokens"].(int64)
		completion, _ := result["completion_tokens"].(int64)
		result["total_tokens"] = prompt + completion
	}
	return result
}

type anthropicStreamTranslator struct {
	model       string
	id          string
	created     int64
	inputTokens *int64
	toolIndices map[int]int
	nextTool    int
}

type streamTranslator interface {
	frame([]byte) ([]byte, error)
	terminal([]byte) ([]byte, error)
}

func newStreamTranslator(providerProtocol, clientProtocol, model string) streamTranslator {
	switch {
	case providerProtocol == "anthropic" && clientProtocol == "openai":
		return newAnthropicStreamTranslator(model)
	case providerProtocol == "openai" && clientProtocol == "anthropic":
		return newOpenAIStreamTranslator(model)
	}
	return nil
}

func newAnthropicStreamTranslator(model string) *anthropicStreamTranslator {
	return &anthropicStreamTranslator{model: model, created: time.Now().Unix(), toolIndices: map[int]int{}}
}

func (t *anthropicStreamTranslator) frame(frame []byte) ([]byte, error) {
	event, data, err := parseSSEFrame(frame)
	if err != nil || data == "" {
		return nil, err
	}
	var object map[string]json.RawMessage
	if StrictJSON([]byte(data), &object) != nil || object == nil {
		return nil, errors.New("invalid_upstream_stream")
	}
	switch event {
	case "message_start":
		var message struct {
			ID    string          `json:"id"`
			Model string          `json:"model"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal(object["message"], &message) != nil {
			return nil, errors.New("invalid_upstream_stream")
		}
		t.id, t.model = message.ID, firstNonEmpty(message.Model, t.model)
		if input, ok := anthropicInputTokens(message.Usage); ok {
			t.inputTokens = &input
		}
		return t.chunk(map[string]any{"role": "assistant", "content": ""}, nil, nil), nil
	case "content_block_start":
		var block struct {
			Index        int          `json:"index"`
			ContentBlock contentBlock `json:"content_block"`
		}
		if json.Unmarshal([]byte(data), &block) != nil {
			return nil, errors.New("invalid_upstream_stream")
		}
		if block.ContentBlock.Type != "tool_use" {
			return nil, nil
		}
		index := t.nextTool
		t.nextTool++
		t.toolIndices[block.Index] = index
		return t.chunk(map[string]any{"tool_calls": []any{map[string]any{
			"index": index,
			"id":    block.ContentBlock.ID,
			"type":  "function",
			"function": map[string]string{
				"name":      block.ContentBlock.Name,
				"arguments": "",
			},
		}}}, nil, nil), nil
	case "content_block_delta":
		var delta struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(data), &delta) != nil {
			return nil, errors.New("invalid_upstream_stream")
		}
		switch delta.Delta.Type {
		case "text_delta":
			return t.chunk(map[string]any{"content": delta.Delta.Text}, nil, nil), nil
		case "thinking_delta":
			return t.chunk(map[string]any{"reasoning_content": delta.Delta.Thinking}, nil, nil), nil
		case "input_json_delta":
			index, ok := t.toolIndices[delta.Index]
			if !ok {
				return nil, errors.New("invalid_upstream_stream")
			}
			return t.chunk(map[string]any{"tool_calls": []any{map[string]any{
				"index":    index,
				"function": map[string]string{"arguments": delta.Delta.PartialJSON},
			}}}, nil, nil), nil
		default:
			return nil, nil
		}
	case "message_delta":
		var delta struct {
			Delta struct {
				StopReason *string `json:"stop_reason"`
			} `json:"delta"`
			Usage json.RawMessage `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &delta) != nil {
			return nil, errors.New("invalid_upstream_stream")
		}
		usage := translateAnthropicUsage(delta.Usage, t.inputTokens)
		return t.chunk(map[string]any{}, translateStopReason(delta.Delta.StopReason), usage), nil
	case "ping", "content_block_stop":
		return nil, nil
	default:
		return nil, nil
	}
}

func (t *anthropicStreamTranslator) terminal(_ []byte) ([]byte, error) {
	return []byte("data: [DONE]\n\n"), nil
}

func (t *anthropicStreamTranslator) chunk(delta map[string]any, finish any, usage map[string]any) []byte {
	choice := map[string]any{"index": 0, "delta": delta, "finish_reason": finish}
	payload := map[string]any{"id": t.id, "object": "chat.completion.chunk", "created": t.created, "model": t.model, "choices": []any{choice}}
	if usage != nil {
		payload["usage"] = usage
	}
	encoded, _ := json.Marshal(payload)
	return append([]byte("data: "), append(encoded, '\n', '\n')...)
}

type openAIStreamTranslator struct {
	model        string
	id           string
	started      bool
	finished     bool
	finishReason *string
	inputTokens  *int64
	outputTokens *int64
	nextIndex    int
	activeType   string
	activeIndex  int
	toolIndices  map[int]int
}

func newOpenAIStreamTranslator(model string) *openAIStreamTranslator {
	return &openAIStreamTranslator{model: model, toolIndices: map[int]int{}}
}

func (t *openAIStreamTranslator) frame(frame []byte) ([]byte, error) {
	_, data, err := parseSSEFrame(frame)
	if err != nil || data == "" {
		return nil, err
	}
	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Index int `json:"index"`
			Delta struct {
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return nil, errors.New("invalid_upstream_stream")
	}
	if chunk.ID != "" {
		t.id = chunk.ID
	}
	if chunk.Model != "" {
		t.model = chunk.Model
	}
	t.updateUsage(chunk.Usage)
	var output []byte
	if !t.started {
		t.started = true
		input := int64(0)
		if t.inputTokens != nil {
			input = *t.inputTokens
		}
		output = append(output, anthropicEvent("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            t.id,
				"type":          "message",
				"role":          "assistant",
				"model":         t.model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         map[string]any{"input_tokens": input},
			},
		})...)
	}
	for _, choice := range chunk.Choices {
		if choice.Index != 0 {
			continue
		}
		if text, ok := streamText(choice.Delta.Content); ok && text != "" {
			var err error
			output, err = t.startBlock(output, "text", "", "")
			if err != nil {
				return nil, err
			}
			output = append(output, anthropicEvent("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": t.activeIndex,
				"delta": map[string]any{"type": "text_delta", "text": text},
			})...)
		}
		for _, call := range choice.Delta.ToolCalls {
			index, ok := t.toolIndices[call.Index]
			if !ok {
				if call.ID == "" || call.Function.Name == "" {
					return nil, errors.New("invalid_upstream_stream")
				}
				var err error
				output, err = t.startBlock(output, "tool_use", call.ID, call.Function.Name)
				if err != nil {
					return nil, err
				}
				index = t.activeIndex
				t.toolIndices[call.Index] = index
			} else if t.activeType != "tool_use" || t.activeIndex != index {
				output = append(output, t.stopActiveBlock()...)
				t.activeType, t.activeIndex = "tool_use", index
			}
			if call.Function.Arguments != "" {
				output = append(output, anthropicEvent("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": index,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Function.Arguments},
				})...)
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			t.finished = true
			reason := *choice.FinishReason
			t.finishReason = &reason
		}
	}
	return output, nil
}

func (t *openAIStreamTranslator) startBlock(output []byte, blockType, id, name string) ([]byte, error) {
	if t.activeType == blockType && blockType == "text" {
		return output, nil
	}
	output = append(output, t.stopActiveBlock()...)
	t.activeType = blockType
	t.activeIndex = t.nextIndex
	t.nextIndex++
	block := map[string]any{"type": blockType}
	if blockType == "text" {
		block["text"] = ""
	} else {
		if id == "" || name == "" {
			return nil, errors.New("invalid_upstream_stream")
		}
		block["id"], block["name"], block["input"] = id, name, map[string]any{}
	}
	return append(output, anthropicEvent("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         t.activeIndex,
		"content_block": block,
	})...), nil
}

func (t *openAIStreamTranslator) stopActiveBlock() []byte {
	if t.activeType == "" {
		return nil
	}
	output := anthropicEvent("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": t.activeIndex,
	})
	t.activeType = ""
	return output
}

func (t *openAIStreamTranslator) terminal(_ []byte) ([]byte, error) {
	if !t.started || !t.finished {
		return nil, errors.New("invalid_upstream_stream")
	}
	output := t.stopActiveBlock()
	usage := map[string]any{}
	if t.outputTokens != nil {
		usage["output_tokens"] = *t.outputTokens
	}
	messageDelta := map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   translateOpenAIStopReason(t.finishReason),
			"stop_sequence": nil,
		},
	}
	if len(usage) > 0 {
		messageDelta["usage"] = usage
	}
	output = append(output, anthropicEvent("message_delta", messageDelta)...)
	output = append(output, anthropicEvent("message_stop", map[string]any{"type": "message_stop"})...)
	return output, nil
}

func (t *openAIStreamTranslator) updateUsage(raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var values map[string]json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return
	}
	get := func(name string) *int64 {
		value, ok := values[name]
		if !ok {
			return nil
		}
		var n int64
		if json.Unmarshal(value, &n) != nil || n < 0 {
			return nil
		}
		return &n
	}
	if input := get("prompt_tokens"); input != nil {
		t.inputTokens = input
	}
	if output := get("completion_tokens"); output != nil {
		t.outputTokens = output
	}
}

func streamText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return "", false
	}
	return text, true
}

func anthropicEvent(event string, payload any) []byte {
	encoded, _ := json.Marshal(payload)
	result := []byte("event: " + event + "\ndata: ")
	result = append(result, encoded...)
	return append(result, '\n', '\n')
}

func parseSSEFrame(frame []byte) (string, string, error) {
	var event string
	var data []string
	for _, line := range strings.Split(string(frame), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if event == "" || len(data) == 0 {
		return event, strings.Join(data, "\n"), nil
	}
	return event, strings.Join(data, "\n"), nil
}

func anthropicInputTokens(raw json.RawMessage) (int64, bool) {
	var value struct {
		Input int64 `json:"input_tokens"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Input < 0 {
		return 0, false
	}
	return value.Input, true
}

func firstNonEmpty(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}
