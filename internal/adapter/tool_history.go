package adapter

import (
	"encoding/json"
	"fmt"
)

// assistantToolResultPath recognizes only the invalid generic client-tool
// result/assistant pairing. Provider-specific server-tool tagged unions remain
// opaque and are never rewritten or pruned here.
func assistantToolResultPath(content json.RawMessage, prefix string) string {
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	for index, block := range blocks {
		if block.Type == "tool_result" {
			return fmt.Sprintf("%s[%d].type", prefix, index)
		}
	}
	return ""
}
