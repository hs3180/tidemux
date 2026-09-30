package adapter

import (
	"encoding/json"
	"strings"
)

func setResponseModel(response []byte, model string) []byte {
	if model == "" {
		return response
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(response, &payload) != nil || payload == nil {
		return response
	}
	encodedModel, err := json.Marshal(model)
	if err != nil {
		return response
	}
	payload["model"] = encodedModel
	encoded, err := json.Marshal(payload)
	if err != nil {
		return response
	}
	return encoded
}

func setStreamResponseModel(protocol string, frame []byte, model string) []byte {
	if model == "" {
		return frame
	}
	event, data, err := parseSSEFrame(frame)
	if err != nil || data == "" {
		return frame
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal([]byte(data), &payload) != nil || payload == nil {
		return frame
	}
	encodedModel, err := json.Marshal(model)
	if err != nil {
		return frame
	}
	if protocol == "anthropic" {
		var kind string
		_ = json.Unmarshal(payload["type"], &kind)
		if event != "message_start" && kind != "message_start" {
			return frame
		}
		var message map[string]json.RawMessage
		if json.Unmarshal(payload["message"], &message) != nil || message == nil {
			return frame
		}
		message["model"] = encodedModel
		encodedMessage, marshalErr := json.Marshal(message)
		if marshalErr != nil {
			return frame
		}
		payload["message"] = encodedMessage
	} else {
		if _, ok := payload["choices"]; !ok {
			return frame
		}
		payload["model"] = encodedModel
	}
	encodedPayload, err := json.Marshal(payload)
	if err != nil {
		return frame
	}
	var rebuilt strings.Builder
	replaced := false
	for _, line := range strings.SplitAfter(string(frame), "\n") {
		content := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if !strings.HasPrefix(content, "data:") {
			rebuilt.WriteString(line)
			continue
		}
		if replaced {
			continue
		}
		newline := ""
		switch {
		case strings.HasSuffix(line, "\r\n"):
			newline = "\r\n"
		case strings.HasSuffix(line, "\n"):
			newline = "\n"
		}
		rebuilt.WriteString("data: ")
		rebuilt.Write(encodedPayload)
		rebuilt.WriteString(newline)
		replaced = true
	}
	if !replaced {
		return frame
	}
	return []byte(rebuilt.String())
}
