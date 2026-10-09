package adapter

import (
	"encoding/json"
	"fmt"
)

// Some Anthropic-compatible providers use generic tool_result blocks for
// provider-hosted tools. Keep those blocks intact when they reference a server
// tool emitted earlier in the same message. Client-tool results still belong
// in user messages; an unpaired assistant result remains invalid history.
type assistantToolHistory struct {
	serverTools map[string]struct{}
}

func (h *assistantToolHistory) accept(block contentBlock) bool {
	switch block.Type {
	case "server_tool_use":
		if block.ID != "" && block.Name != "" && object(block.Input) {
			if h.serverTools == nil {
				h.serverTools = make(map[string]struct{})
			}
			h.serverTools[block.ID] = struct{}{}
		}
	case "tool_result":
		_, paired := h.serverTools[block.ToolUseID]
		return paired && validToolResult(block)
	}
	return true
}

func assistantToolResultPath(content json.RawMessage, prefix string) string {
	return (&assistantToolHistory{}).contentPath(content, prefix)
}

func (h *assistantToolHistory) contentPath(content json.RawMessage, prefix string) string {
	var blocks []contentBlock
	if LenientJSON(content, &blocks) != nil {
		return ""
	}
	for index, block := range blocks {
		if !h.accept(block) {
			return fmt.Sprintf("%s[%d].type", prefix, index)
		}
	}
	return ""
}
