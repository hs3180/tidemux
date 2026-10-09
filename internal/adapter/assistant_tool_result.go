package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const StripRedundantToolResults = "strip_redundant"

func ValidateAssistantToolResultPolicy(policy string) error {
	if policy != "" && policy != "reject" && policy != StripRedundantToolResults {
		return errors.New("must be reject or strip_redundant")
	}
	return nil
}

// Some GLM Anthropic responses repeat a server-tool's text output as a generic
// tool_result in an assistant message. Suppress only a complete duplicate:
// unique output, errors, media and unknown structures still fail closed.
type assistantToolResultFilter struct {
	text       strings.Builder
	blocks     map[int]*filteredToolBlock
	nextIndex  int
	suppressed int
}

type filteredToolBlock struct {
	index  int
	skip   bool
	isText bool
	closed bool
	deltas strings.Builder
}

func redundantToolResult(block map[string]json.RawMessage, text string) bool {
	for field := range block {
		if field != "type" && field != "tool_use_id" && field != "content" {
			return false
		}
	}
	var id string
	if json.Unmarshal(block["tool_use_id"], &id) != nil || id == "" {
		return false
	}
	var result string
	if json.Unmarshal(block["content"], &result) != nil {
		var parts []map[string]json.RawMessage
		if json.Unmarshal(block["content"], &parts) != nil || len(parts) == 0 {
			return false
		}
		var joined strings.Builder
		for _, part := range parts {
			var kind, value string
			if len(part) != 2 || json.Unmarshal(part["type"], &kind) != nil || kind != "text" || json.Unmarshal(part["text"], &value) != nil {
				return false
			}
			joined.WriteString(value)
		}
		result = joined.String()
	}
	return result != "" && strings.Contains(text, result)
}

func (f *assistantToolResultFilter) content(raw json.RawMessage, prefix string) (json.RawMessage, error) {
	var blocks []map[string]json.RawMessage
	if len(raw) == 0 || string(raw) == "null" {
		return raw, nil
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return raw, nil // Normal response validation diagnoses other shapes.
	}
	kept := make([]map[string]json.RawMessage, 0, len(blocks))
	changed := false
	for index, block := range blocks {
		var kind, text string
		_ = json.Unmarshal(block["type"], &kind)
		if kind == "tool_result" {
			if !redundantToolResult(block, f.text.String()) {
				return nil, &CallError{Status: 502, Code: "invalid_upstream_tool_history", Param: fmt.Sprintf("%s[%d].type", prefix, index)}
			}
			f.suppressed++
			changed = true
			continue
		}
		if kind == "text" && json.Unmarshal(block["text"], &text) == nil {
			f.text.WriteString(text)
		}
		kept = append(kept, block)
	}
	if !changed {
		return raw, nil
	}
	return json.Marshal(kept)
}

func (f *assistantToolResultFilter) response(raw []byte) ([]byte, error) {
	var message map[string]json.RawMessage
	if StrictJSON(raw, &message) != nil {
		return nil, &CallError{Status: 502, Code: "invalid_upstream_response"}
	}
	content, err := f.content(message["content"], "content")
	if err != nil || f.suppressed == 0 {
		return raw, err
	}
	message["content"] = content
	return json.Marshal(message)
}

// All indices after a suppressed block are remapped. Leaving array holes can
// corrupt the client's assistant history and poison a later continuation.
func (f *assistantToolResultFilter) frame(kind string, object map[string]json.RawMessage, raw []byte) ([]byte, bool, error) {
	fail := func(index int) ([]byte, bool, error) {
		return nil, false, &CallError{Status: 502, Code: "invalid_upstream_tool_history", Param: fmt.Sprintf("content[%d].type", index)}
	}
	if kind == "message_start" {
		var message map[string]json.RawMessage
		if json.Unmarshal(object["message"], &message) != nil {
			return raw, false, nil
		}
		content, err := f.content(message["content"], "message.content")
		if err != nil {
			return nil, false, err
		}
		var initial []map[string]json.RawMessage
		_ = json.Unmarshal(message["content"], &initial)
		if len(initial) > 0 {
			f.blocks = make(map[int]*filteredToolBlock)
			for index, block := range initial {
				var blockType string
				_ = json.Unmarshal(block["type"], &blockType)
				skip := blockType == "tool_result"
				f.blocks[index] = &filteredToolBlock{index: f.nextIndex, skip: skip, closed: true}
				if !skip {
					f.nextIndex++
				}
			}
		}
		if f.suppressed > 0 {
			message["content"] = content
			object["message"], _ = json.Marshal(message)
			return anthropicEvent(kind, object), false, nil
		}
		return raw, false, nil
	}
	if kind == "message_delta" || kind == "message_stop" {
		for index, block := range f.blocks {
			if block.skip && !block.closed {
				return fail(index)
			}
		}
	}
	if kind != "content_block_start" && kind != "content_block_delta" && kind != "content_block_stop" {
		return raw, false, nil
	}
	var index *int
	if json.Unmarshal(object["index"], &index) != nil || index == nil || *index < 0 {
		return nil, false, &CallError{Status: 502, Code: "invalid_upstream_stream"}
	}
	block := f.blocks[*index]
	if kind == "content_block_start" {
		if block != nil {
			return fail(*index)
		}
		var content map[string]json.RawMessage
		var blockType, text string
		if json.Unmarshal(object["content_block"], &content) != nil {
			return fail(*index)
		}
		_ = json.Unmarshal(content["type"], &blockType)
		block = &filteredToolBlock{index: f.nextIndex, isText: blockType == "text"}
		if blockType == "tool_result" {
			if !redundantToolResult(content, f.text.String()) {
				return fail(*index)
			}
			block.skip = true
			f.suppressed++
		} else {
			f.nextIndex++
			if block.isText && json.Unmarshal(content["text"], &text) == nil {
				f.text.WriteString(text)
			}
		}
		if f.blocks == nil {
			f.blocks = make(map[int]*filteredToolBlock)
		}
		f.blocks[*index] = block
	} else {
		if block == nil || block.closed {
			return fail(*index)
		}
		if kind == "content_block_delta" {
			var fields map[string]json.RawMessage
			var delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(object["delta"], &delta) != nil {
				return fail(*index)
			}
			if block.skip {
				if delta.Type != "text_delta" || json.Unmarshal(object["delta"], &fields) != nil || len(fields) != 2 {
					return fail(*index)
				}
				block.deltas.WriteString(delta.Text)
			} else if block.isText && delta.Type == "text_delta" {
				f.text.WriteString(delta.Text)
			}
		} else {
			if block.skip && block.deltas.Len() > 0 && !strings.Contains(f.text.String(), block.deltas.String()) {
				return fail(*index)
			}
			block.closed = true
		}
	}
	if block.skip {
		return nil, true, nil
	}
	if block.index != *index {
		object["index"], _ = json.Marshal(block.index)
		return anthropicEvent(kind, object), false, nil
	}
	return raw, false, nil
}
