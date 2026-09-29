package adapter

import (
	"encoding/json"
	"strconv"
	"strings"
)

// Tool schemas and arguments are opaque JSON. Envelopes are validated here;
// the chosen provider validates the model-specific JSON Schema dialect.
type Function struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}
type Tool struct {
	Type                string          `json:"type,omitempty"`
	Function            *Function       `json:"function,omitempty"`
	Name                string          `json:"name,omitempty"`
	Description         string          `json:"description,omitempty"`
	InputSchema         json.RawMessage `json:"input_schema,omitempty"`
	CacheControl        *CacheControl   `json:"cache_control,omitempty"`
	EagerInputStreaming *bool           `json:"eager_input_streaming,omitempty"`
}
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

func (c *CacheControl) valid() bool {
	return c == nil || c.Type == "ephemeral" && (c.TTL == "" || c.TTL == "5m" || c.TTL == "1h")
}
func object(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(raw, &m) == nil && m != nil
}
func validTools(protocol string, tools []Tool) bool {
	for _, t := range tools {
		if protocol == "openai" {
			if t.Type != "function" || t.Function == nil || strings.TrimSpace(t.Function.Name) == "" || t.Name != "" || t.InputSchema != nil || t.CacheControl != nil || t.Description != "" {
				return false
			}
			if t.Function.Parameters != nil && !object(t.Function.Parameters) {
				return false
			}
		} else {
			if t.Type != "" && t.Type != "custom" || t.Function != nil || strings.TrimSpace(t.Name) == "" || !object(t.InputSchema) || !t.CacheControl.valid() {
				return false
			}
		}
	}
	return true
}
func validCalls(calls []ToolCall) bool {
	seen := map[string]bool{}
	for _, c := range calls {
		if c.ID == "" || seen[c.ID] || c.Type != "function" || strings.TrimSpace(c.Function.Name) == "" {
			return false
		}
		// Arguments are a string on the wire; malformed model output must remain
		// available to the client so it can report a tool error and recover.
		seen[c.ID] = true
	}
	return true
}
func validChoice(protocol string, raw json.RawMessage) bool {
	if raw == nil {
		return true
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return protocol == "openai" && (s == "auto" || s == "none" || s == "required")
	}
	var c struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
		DisableParallel *bool `json:"disable_parallel_tool_use"`
	}
	if LenientJSON(raw, &c) != nil {
		return false
	}
	if protocol == "openai" {
		return c.Type == "function" && c.Function != nil && c.Function.Name != "" && c.Name == "" && c.DisableParallel == nil
	}
	if c.Function != nil {
		return false
	}
	return (c.Type == "auto" || c.Type == "any" || c.Type == "none") && c.Name == "" || c.Type == "tool" && c.Name != ""
}

type contentBlock struct {
	Type             string          `json:"type"`
	Text             *string         `json:"text,omitempty"`
	CacheControl     *CacheControl   `json:"cache_control,omitempty"`
	ID               string          `json:"id,omitempty"`
	Name             string          `json:"name,omitempty"`
	Input            json.RawMessage `json:"input,omitempty"`
	ToolUseID        string          `json:"tool_use_id,omitempty"`
	Content          json.RawMessage `json:"content,omitempty"`
	IsError          *bool           `json:"is_error,omitempty"`
	Thinking         *string         `json:"thinking,omitempty"`
	Signature        string          `json:"signature,omitempty"`
	Data             string          `json:"data,omitempty"`
	Source           json.RawMessage `json:"source,omitempty"`
	Citations        json.RawMessage `json:"citations,omitempty"`
	Transformations  json.RawMessage `json:"transformations,omitempty"`
	Title            string          `json:"title,omitempty"`
	Context          string          `json:"context,omitempty"`
	Caller           json.RawMessage `json:"caller,omitempty"`
	EncryptedContent string          `json:"encrypted_content,omitempty"`
	ReturnCode       *int64          `json:"return_code,omitempty"`
	Stderr           string          `json:"stderr,omitempty"`
	FileID           string          `json:"file_id,omitempty"`
}

func messageContent(protocol, role string, raw json.RawMessage) bool {
	var s string
	if json.Unmarshal(raw, &s) == nil && string(raw) != "null" {
		return true
	}
	var blocks []contentBlock
	if LenientJSON(raw, &blocks) != nil || len(blocks) == 0 {
		return false
	}
	for _, b := range blocks {
		if !b.CacheControl.valid() || protocol == "openai" && b.CacheControl != nil {
			return false
		}
		switch b.Type {
		case "text":
			if b.Text == nil || b.ID != "" || b.Name != "" || b.Input != nil || b.ToolUseID != "" || b.Content != nil || b.IsError != nil || b.Thinking != nil || b.Signature != "" || b.Data != "" || b.Source != nil || b.Transformations != nil || b.Title != "" || b.Context != "" || b.Caller != nil || b.EncryptedContent != "" || b.ReturnCode != nil || b.Stderr != "" || b.FileID != "" {
				return false
			}
		case "image":
			if protocol != "anthropic" || role != "user" && role != "tool_result" || !object(b.Source) || b.Text != nil || b.ID != "" || b.Name != "" || b.Input != nil || b.ToolUseID != "" || b.Content != nil || b.IsError != nil || b.Thinking != nil || b.Signature != "" || b.Data != "" || b.Citations != nil || b.Title != "" || b.Context != "" || b.Caller != nil || b.EncryptedContent != "" || b.ReturnCode != nil || b.Stderr != "" || b.FileID != "" {
				return false
			}
		case "document":
			if protocol != "anthropic" || role != "user" && role != "tool_result" || !object(b.Source) || b.Text != nil || b.ID != "" || b.Name != "" || b.Input != nil || b.ToolUseID != "" || b.Content != nil || b.IsError != nil || b.Thinking != nil || b.Signature != "" || b.Data != "" || b.Transformations != nil || b.Caller != nil || b.EncryptedContent != "" || b.ReturnCode != nil || b.Stderr != "" || b.FileID != "" {
				return false
			}
		case "search_result":
			if protocol != "anthropic" || b.Content == nil || b.Source == nil || b.Text != nil || b.ID != "" || b.Name != "" || b.Input != nil || b.ToolUseID != "" || b.IsError != nil || b.Thinking != nil || b.Signature != "" || b.Data != "" || b.Transformations != nil || b.Context != "" || b.Caller != nil || b.EncryptedContent != "" || b.ReturnCode != nil || b.Stderr != "" || b.FileID != "" {
				return false
			}
		case "tool_use":
			if protocol != "anthropic" || role != "assistant" || b.ID == "" || b.Name == "" || !object(b.Input) || b.Text != nil || b.ToolUseID != "" || b.Content != nil || b.IsError != nil || b.Thinking != nil || b.Signature != "" || b.Data != "" || b.Source != nil || b.Citations != nil || b.Transformations != nil || b.Title != "" || b.Context != "" || b.EncryptedContent != "" || b.ReturnCode != nil || b.Stderr != "" || b.FileID != "" {
				return false
			}
		case "tool_result":
			if protocol != "anthropic" || role != "user" || b.ToolUseID == "" || b.Text != nil || b.ID != "" || b.Name != "" || b.Input != nil || b.Thinking != nil || b.Signature != "" || b.Data != "" || b.Source != nil || b.Citations != nil || b.Transformations != nil || b.Title != "" || b.Context != "" || b.Caller != nil || b.EncryptedContent != "" || b.ReturnCode != nil || b.Stderr != "" || b.FileID != "" {
				return false
			}
			if b.Content != nil && !messageContent(protocol, "tool_result", b.Content) {
				return false
			}
		case "thinking", "redacted_thinking":
			if protocol != "anthropic" || role != "assistant" || b.Text != nil || b.ID != "" || b.Name != "" || b.Input != nil || b.ToolUseID != "" || b.Content != nil || b.IsError != nil || b.Source != nil || b.Citations != nil || b.Transformations != nil || b.Title != "" || b.Context != "" || b.Caller != nil || b.EncryptedContent != "" || b.ReturnCode != nil || b.Stderr != "" || b.FileID != "" {
				return false
			}
			if b.Type == "thinking" && (b.Thinking == nil || b.Signature == "" || b.Data != "") {
				return false
			}
			if b.Type == "redacted_thinking" && (b.Data == "" || b.Thinking != nil || b.Signature != "") {
				return false
			}
		case "compaction":
			if protocol != "anthropic" || role != "assistant" || !validCompactionContent(b.Content) || b.Text != nil || b.ID != "" || b.Name != "" || b.Input != nil || b.ToolUseID != "" || b.IsError != nil || b.Thinking != nil || b.Signature != "" || b.Data != "" || b.Source != nil || b.Citations != nil || b.Transformations != nil || b.Title != "" || b.Context != "" || b.Caller != nil || b.EncryptedContent != "" || b.ReturnCode != nil || b.Stderr != "" || b.FileID != "" {
				return false
			}
		case "server_tool_use":
			if protocol != "anthropic" || role != "assistant" || b.ID == "" || b.Name == "" || !object(b.Input) || b.Text != nil || b.ToolUseID != "" || b.Content != nil || b.IsError != nil || b.Thinking != nil || b.Signature != "" || b.Data != "" || b.Source != nil || b.Citations != nil || b.Transformations != nil || b.Title != "" || b.Context != "" || b.EncryptedContent != "" || b.ReturnCode != nil || b.Stderr != "" || b.FileID != "" {
				return false
			}
		case "web_search_tool_result", "web_fetch_tool_result":
			if protocol != "anthropic" || b.ToolUseID == "" || b.Content == nil || b.Text != nil || b.ID != "" || b.Name != "" || b.Input != nil || b.Thinking != nil || b.Signature != "" || b.Data != "" || b.Source != nil || b.Citations != nil || b.Transformations != nil || b.Title != "" || b.Context != "" || b.FileID != "" {
				return false
			}
		case "code_execution_tool_result", "bash_code_execution_tool_result", "text_editor_code_execution_tool_result", "tool_search_tool_result":
			if protocol != "anthropic" || b.ToolUseID == "" || b.Content == nil || b.Text != nil || b.ID != "" || b.Name != "" || b.Input != nil || b.Thinking != nil || b.Signature != "" || b.Data != "" || b.Source != nil || b.Citations != nil || b.Transformations != nil || b.Title != "" || b.Context != "" || b.Caller != nil || b.FileID != "" {
				return false
			}
		case "container_upload":
			if protocol != "anthropic" || b.FileID == "" || b.Text != nil || b.ID != "" || b.Name != "" || b.Input != nil || b.ToolUseID != "" || b.Content != nil || b.IsError != nil || b.Thinking != nil || b.Signature != "" || b.Data != "" || b.Source != nil || b.Citations != nil || b.Transformations != nil || b.Title != "" || b.Context != "" || b.Caller != nil || b.EncryptedContent != "" || b.ReturnCode != nil || b.Stderr != "" {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validCompactionContent(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return true
	}
	var content string
	return json.Unmarshal(raw, &content) == nil
}

func unsupportedContentBlockPath(protocol, role string, raw json.RawMessage, path string) string {
	var blocks []struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil || blocks == nil {
		return path
	}
	for index, block := range blocks {
		blockPath := path + "[" + strconv.Itoa(index) + "]"
		if !supportedContentBlockType(protocol, block.Type) {
			return blockPath + ".type"
		}
		if block.Type == "tool_result" {
			if nested := unsupportedContentBlockPath(protocol, "tool_result", block.Content, blockPath+".content"); nested != "" {
				return nested
			}
		}
	}
	return path
}

func supportedContentBlockType(protocol, kind string) bool {
	if protocol == "openai" {
		return kind == "text"
	}
	switch kind {
	case "text", "image", "document", "search_result", "tool_use", "tool_result", "thinking", "redacted_thinking", "compaction", "server_tool_use", "web_search_tool_result", "web_fetch_tool_result", "code_execution_tool_result", "bash_code_execution_tool_result", "text_editor_code_execution_tool_result", "tool_search_tool_result", "container_upload":
		return true
	default:
		return false
	}
}
