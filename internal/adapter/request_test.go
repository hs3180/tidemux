package adapter

import (
	"bytes"
	"reflect"
	"testing"
)

func TestRequestIgnoresUnknownFieldsAndReportsIgnoredPaths(t *testing.T) {
	body := []byte(`{"model":"m","store":true,"messages":[{"role":"user","content":"use a tool","client_extension":{"trace":true}}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}},"eager_input_streaming":true}]}`)
	encoded, model, ignored, err := RequestWithWarnings("openai", body, "")
	if err != nil || model != "m" {
		t.Fatalf("model=%q err=%v", model, err)
	}
	want := []string{"messages[0].client_extension", "store", "tools[0].eager_input_streaming"}
	if !reflect.DeepEqual(ignored, want) {
		t.Fatalf("ignored=%v want=%v", ignored, want)
	}
	if bytes.Contains(encoded, []byte("store")) || bytes.Contains(encoded, []byte("eager_input_streaming")) || bytes.Contains(encoded, []byte("client_extension")) {
		t.Fatalf("ignored fields survived normalization: %s", encoded)
	}
}

func TestRequestTypeErrorIncludesParameter(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"temperature":"not-a-number"}`)
	if _, _, err := Request("openai", body, ""); err == nil || ValidationParameter(err) != "temperature" {
		t.Fatalf("err=%v param=%q", err, ValidationParameter(err))
	}
}
