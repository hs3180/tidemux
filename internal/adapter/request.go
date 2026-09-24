package adapter

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// ValidationError is a safe client-input error. Code remains stable for
// diagnostics while Param identifies the JSON field that needs attention.
type ValidationError struct {
	Code  string
	Param string
}

func (e *ValidationError) Error() string { return e.Code }

func validationError(code, param string) error {
	return &ValidationError{Code: code, Param: param}
}

func validationErrorFromJSON(err error) error {
	var typeError *json.UnmarshalTypeError
	if errors.As(err, &typeError) && typeError.Field != "" {
		return validationError("invalid_request", typeError.Field)
	}
	return validationError("invalid_request", "")
}

// ValidationParameter returns a safe JSON field path for a validation error.
// Translation helpers may still return a stable legacy code, so the fallback
// keeps those errors actionable without exposing decoder internals.
func ValidationParameter(err error) string {
	var validation *ValidationError
	if errors.As(err, &validation) {
		return validation.Param
	}
	switch errString := strings.TrimSpace(err.Error()); errString {
	case "model", "thinking", "stream_options", "context_management", "response_format", "output_config", "parallel_tool_calls", "messages", "tools", "tool_choice", "tool_calls", "tool_call_id", "reasoning_content", "max_tokens", "max_completion_tokens", "temperature", "top_p", "stop", "stop_sequences", "system", "invalid_session_id", "invalid_beta_header":
		return errString
	default:
		return ""
	}
}

func ignoredRequestFields(data []byte, input Input) []string {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return nil
	}
	fields := collectUnknownFields(value, reflect.TypeOf(Input{}), "")
	for index, tool := range input.Tools {
		if tool.EagerInputStreaming != nil {
			fields = append(fields, fmt.Sprintf("tools[%d].eager_input_streaming", index))
		}
	}
	sort.Strings(fields)
	return uniqueStrings(fields)
}

// discardForeignProtocolFields removes modeled fields that belong only to the
// other wire protocol and prunes unknown content-block fields before decoding
// into the shared request type. Filtering raw JSON first ensures a malformed
// foreign field cannot turn an otherwise valid request into a type error.
func discardForeignProtocolFields(data []byte, protocol string) ([]byte, []string, error) {
	var object map[string]json.RawMessage
	if err := LenientJSON(data, &object); err != nil {
		return nil, nil, err
	}

	var foreignFields []string
	var topLevel []string
	switch protocol {
	case "openai":
		topLevel = []string{"system", "stop_sequences", "metadata", "output_config", "context_management"}
	case "anthropic":
		topLevel = []string{"user", "stream_options", "response_format", "reasoning_effort", "parallel_tool_calls", "max_completion_tokens", "stop"}
	}
	foreignFields = append(foreignFields, removeJSONFields(object, topLevel)...)
	if choice, key, ok := findJSONField(object, "tool_choice"); ok {
		var schema reflect.Type
		switch protocol {
		case "openai":
			schema = reflect.TypeOf(struct {
				Type     string `json:"type"`
				Function *struct {
					Name string `json:"name"`
				} `json:"function"`
			}{})
		case "anthropic":
			schema = reflect.TypeOf(struct {
				Type               string `json:"type"`
				Name               string `json:"name"`
				DisableParallelUse *bool  `json:"disable_parallel_tool_use"`
			}{})
		}
		if schema != nil {
			cleaned, paths, err := pruneUnknownJSONFields(choice, schema, "tool_choice")
			if err != nil {
				return nil, nil, err
			}
			object[key] = cleaned
			foreignFields = append(foreignFields, paths...)
		}
	}
	contentFields, err := sanitizeRequestContent(object, protocol)
	if err != nil {
		return nil, nil, err
	}
	foreignFields = append(foreignFields, contentFields...)

	if protocol == "anthropic" {
		if messages, key, ok := findJSONField(object, "messages"); ok {
			var items []json.RawMessage
			if json.Unmarshal(messages, &items) == nil {
				for index, item := range items {
					var message map[string]json.RawMessage
					if LenientJSON(item, &message) != nil || message == nil {
						continue // Preserve the typed decoder's error for malformed messages.
					}
					for _, field := range removeJSONFields(message, []string{"tool_calls", "tool_call_id", "reasoning_content"}) {
						foreignFields = append(foreignFields, fmt.Sprintf("messages[%d].%s", index, field))
					}
					encoded, err := json.Marshal(message)
					if err != nil {
						return nil, nil, err
					}
					items[index] = encoded
				}
				encoded, err := json.Marshal(items)
				if err != nil {
					return nil, nil, err
				}
				object[key] = encoded
			}
		}
	}

	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, nil, err
	}
	return encoded, sortedUniqueStrings(foreignFields), nil
}

func sanitizeRequestContent(object map[string]json.RawMessage, protocol string) ([]string, error) {
	var fields []string
	if protocol == "anthropic" {
		if system, key, ok := findJSONField(object, "system"); ok {
			cleaned, paths, err := sanitizeContentBlocks(system, "system", protocol)
			if err != nil {
				return nil, err
			}
			object[key] = cleaned
			fields = append(fields, paths...)
		}
	}
	if messages, key, ok := findJSONField(object, "messages"); ok {
		var items []json.RawMessage
		if json.Unmarshal(messages, &items) == nil {
			for index, item := range items {
				var message map[string]json.RawMessage
				if LenientJSON(item, &message) != nil || message == nil {
					continue
				}
				content, contentKey, ok := findJSONField(message, "content")
				if !ok {
					continue
				}
				cleaned, paths, err := sanitizeContentBlocks(content, fmt.Sprintf("messages[%d].content", index), protocol)
				if err != nil {
					return nil, err
				}
				message[contentKey] = cleaned
				fields = append(fields, paths...)
				encoded, err := json.Marshal(message)
				if err != nil {
					return nil, err
				}
				items[index] = encoded
			}
			encoded, err := json.Marshal(items)
			if err != nil {
				return nil, err
			}
			object[key] = encoded
		}
	}
	return fields, nil
}

func sanitizeContentBlocks(raw json.RawMessage, path, protocol string) (json.RawMessage, []string, error) {
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) != nil || blocks == nil {
		return raw, nil, nil
	}
	var fields []string
	for index, encoded := range blocks {
		blockPath := fmt.Sprintf("%s[%d]", path, index)
		var rawBlock map[string]json.RawMessage
		if LenientJSON(encoded, &rawBlock) == nil && rawBlock != nil && protocol == "openai" {
			for _, field := range removeJSONFields(rawBlock, []string{"cache_control"}) {
				fields = append(fields, blockPath+"."+field)
			}
			filteredBlock, err := json.Marshal(rawBlock)
			if err != nil {
				return nil, nil, err
			}
			encoded = filteredBlock
		}
		cleaned, paths, err := pruneUnknownJSONFields(encoded, reflect.TypeOf(contentBlock{}), blockPath)
		if err != nil {
			return nil, nil, err
		}
		fields = append(fields, paths...)
		var cleanedBlock map[string]json.RawMessage
		if LenientJSON(cleaned, &cleanedBlock) != nil || cleanedBlock == nil {
			continue
		}
		var kind string
		if typeValue, _, ok := findJSONField(cleanedBlock, "type"); ok {
			_ = json.Unmarshal(typeValue, &kind)
		}
		if kind == "tool_result" {
			if content, key, ok := findJSONField(cleanedBlock, "content"); ok {
				nested, nestedFields, err := sanitizeContentBlocks(content, blockPath+".content", protocol)
				if err != nil {
					return nil, nil, err
				}
				cleanedBlock[key] = nested
				fields = append(fields, nestedFields...)
			}
		}
		encoded, err := json.Marshal(cleanedBlock)
		if err != nil {
			return nil, nil, err
		}
		blocks[index] = encoded
	}
	encoded, err := json.Marshal(blocks)
	return encoded, fields, err
}

func pruneUnknownJSONFields(raw json.RawMessage, target reflect.Type, path string) (json.RawMessage, []string, error) {
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if target == reflect.TypeOf(json.RawMessage{}) || target.Kind() == reflect.Interface {
		return raw, nil, nil
	}
	switch target.Kind() {
	case reflect.Struct:
		var object map[string]json.RawMessage
		if LenientJSON(raw, &object) != nil || object == nil {
			return raw, nil, nil
		}
		known := jsonStructFields(target)
		var fields []string
		for key, child := range object {
			fieldType, ok := known[strings.ToLower(key)]
			fieldPath := joinJSONPath(path, key)
			if !ok {
				delete(object, key)
				fields = append(fields, fieldPath)
				continue
			}
			cleaned, nested, err := pruneUnknownJSONFields(child, fieldType, fieldPath)
			if err != nil {
				return nil, nil, err
			}
			object[key] = cleaned
			fields = append(fields, nested...)
		}
		encoded, err := json.Marshal(object)
		return encoded, fields, err
	case reflect.Slice, reflect.Array:
		var items []json.RawMessage
		if json.Unmarshal(raw, &items) != nil || items == nil {
			return raw, nil, nil
		}
		var fields []string
		for index, child := range items {
			cleaned, nested, err := pruneUnknownJSONFields(child, target.Elem(), fmt.Sprintf("%s[%d]", path, index))
			if err != nil {
				return nil, nil, err
			}
			items[index] = cleaned
			fields = append(fields, nested...)
		}
		encoded, err := json.Marshal(items)
		return encoded, fields, err
	default:
		return raw, nil, nil
	}
}

func removeJSONFields(object map[string]json.RawMessage, fields []string) []string {
	wanted := make(map[string]string, len(fields))
	for _, field := range fields {
		wanted[strings.ToLower(field)] = field
	}
	var removed []string
	for key := range object {
		if field, ok := wanted[strings.ToLower(key)]; ok {
			delete(object, key)
			removed = append(removed, field)
		}
	}
	return removed
}

func findJSONField(object map[string]json.RawMessage, field string) (json.RawMessage, string, bool) {
	for key, value := range object {
		if strings.EqualFold(key, field) {
			return value, key, true
		}
	}
	return nil, "", false
}

func collectUnknownFields(value any, target reflect.Type, path string) []string {
	for target.Kind() == reflect.Pointer {
		target = target.Elem()
	}
	if target == reflect.TypeOf(json.RawMessage{}) || target.Kind() == reflect.Interface {
		return nil
	}
	switch target.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return nil
		}
		fields := jsonStructFields(target)
		var unknown []string
		for key, child := range object {
			fieldType, ok := fields[strings.ToLower(key)]
			fieldPath := joinJSONPath(path, key)
			if !ok {
				unknown = append(unknown, fieldPath)
				continue
			}
			unknown = append(unknown, collectUnknownFields(child, fieldType, fieldPath)...)
		}
		return unknown
	case reflect.Slice, reflect.Array:
		items, ok := value.([]any)
		if !ok {
			return nil
		}
		var unknown []string
		for index, child := range items {
			unknown = append(unknown, collectUnknownFields(child, target.Elem(), fmt.Sprintf("%s[%d]", path, index))...)
		}
		return unknown
	default:
		return nil
	}
}

func jsonStructFields(target reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type)
	for index := 0; index < target.NumField(); index++ {
		field := target.Field(index)
		if field.PkgPath != "" {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[strings.ToLower(name)] = field.Type
	}
	return fields
}

func joinJSONPath(parent, child string) string {
	if parent == "" {
		return child
	}
	return parent + "." + child
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	unique := values[:0]
	for _, value := range values {
		if len(unique) == 0 || unique[len(unique)-1] != value {
			unique = append(unique, value)
		}
	}
	return unique
}

func sortedUniqueStrings(values []string) []string {
	sort.Strings(values)
	return uniqueStrings(values)
}
