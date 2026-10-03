package gateway

func addToolHistoryRecovery(detail map[string]any, code, field string) {
	switch code {
	case "invalid_tool_history":
		detail["message"] = "invalid_tool_history: " + field + ": a generic tool_result is only valid in a user message. A provider built-in tool may have produced malformed history. Start a clean conversation or explicitly repair the stored block in your client; use client-executed tools. Replaying unchanged history will fail."
	case "invalid_upstream_tool_history":
		detail["message"] = "invalid_upstream_tool_history: " + field + ": the provider returned a generic tool_result in an assistant response. TideMux withheld the malformed block. Start a clean conversation or explicitly repair any previously stored malformed history; use client-executed tools."
	default:
		return
	}
	detail["recovery"] = map[string]any{
		"retryable":  false,
		"action":     "start_clean_conversation_or_explicitly_repair_history",
		"workaround": "client_executed_tools",
	}
}
