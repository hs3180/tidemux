package adapter

import "encoding/json"

// Native Anthropic content is opaque beyond its tagged-object envelope. The
// provider and client own block fields, tool roles and history associations;
// protocol conversion validates the fields it actually needs to interpret.
func nativeContentBlocks(raw json.RawMessage) bool {
	var blocks []struct {
		Type string `json:"type"`
	}
	if LenientJSON(raw, &blocks) != nil || blocks == nil {
		return false
	}
	for _, block := range blocks {
		if block.Type == "" {
			return false
		}
	}
	return true
}

func nativeContentBlock(raw json.RawMessage) bool {
	var block struct {
		Type string `json:"type"`
	}
	return LenientJSON(raw, &block) == nil && block.Type != ""
}
