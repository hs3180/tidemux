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
