package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type testSecrets map[string]string

func (s testSecrets) Lookup(_ context.Context, r KeychainReference) (string, error) {
	return s[r.Service], nil
}
func TestConfigCredentialsAndValidation(t *testing.T) {
	c := testConfig("l.db", "https://example.com/prefix/v1")
	resolved, err := c.ResolveCredentials(context.Background(), testSecrets{"test.provider": "provider-private", "test.gateway": "local-private"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(resolved)
	if strings.Contains(string(data), "private") {
		t.Fatal("serialized secret")
	}
	for _, url := range []string{"http://example.com/v1", "https://user:pass@example.com/v1", "https://example.com/v1?key=x", "file:///tmp/x"} {
		bad := c
		bad.BaseURL = url
		if bad.Validate() == nil {
			t.Errorf("accepted %s", url)
		}
	}
	bad := c
	bad.ListenAddr = "0.0.0.0:8787"
	if bad.Validate() == nil {
		t.Fatal("non-loopback accepted")
	}
	for _, body := range []string{`{"deepseek_api_key":"secret"}`, `{} {}`, `{"protocol":"openai","protocol":"anthropic"}`} {
		p := filepath.Join(t.TempDir(), "config.json")
		os.WriteFile(p, []byte(body), 0600)
		if _, err := LoadConfig(p); err == nil {
			t.Fatal("bad config accepted")
		}
	}
}
