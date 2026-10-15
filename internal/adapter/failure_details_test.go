package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
)

func TestFailureDetailsSurviveUpstreamDeadline(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("request-id", "req_deadline12345678")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	client, l := newCandidateTestClient(t, upstream.Client())
	defer l.Close()
	client.BaseURL, client.Limits.UpstreamTimeoutSeconds = upstream.URL+"/v1", 1
	_, id, err := client.Call(context.Background(), []byte(`{"model":"m","messages":[{"role":"user","content":"probe"}]}`), "m")
	var e *CallError
	if !errors.As(err, &e) || e.Code != "upstream_timeout" || e.Failure == nil {
		t.Fatalf("error=%+v", err)
	}
	f := e.Failure
	if f.Phase != "response_body" || f.HTTPStatus != 200 || f.Category != "timeout" || f.Retryability != "retryable" || f.ProviderRequestID != "req_deadline12345678" {
		t.Fatalf("deadline failure=%+v", f)
	}
	rows, err := l.Recent(context.Background(), 10)
	if err != nil || len(rows) != 1 || rows[0].ID != id || rows[0].Failure == nil || rows[0].Failure.Category != "timeout" {
		t.Fatalf("audit=%+v err=%v", rows, err)
	}
}

func TestFailureDetailsRetainCancellationPhaseWithoutProviderBlame(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if trace := httptrace.ContextClientTrace(r.Context()); trace != nil && trace.WroteHeaders != nil {
			trace.WroteHeaders()
		}
		cancel()
		return nil, context.Canceled
	})
	client, l := newCandidateTestClient(t, &http.Client{Transport: transport})
	defer l.Close()
	_, _, err := client.Call(ctx, []byte(`{"model":"m","messages":[{"role":"user","content":"probe"}]}`), "m")
	var e *CallError
	if !errors.As(err, &e) || e.Code != "request_canceled" || e.Failure == nil || e.Failure.Source != "canceled" || e.Failure.Phase != "response_headers" || e.Failure.Category != "canceled" || e.Failure.Retryability != "terminal" {
		t.Fatalf("cancellation failure=%+v", e)
	}
}

func TestFailureDetailsRedactCredentialEchoesEvenForConfiguredCodes(t *testing.T) {
	const credential = "credential_marker"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("request-id", "req_"+credential)
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"code":"credential_marker","message":"PRIVATE_MESSAGE"}}`)
	}))
	defer upstream.Close()
	client, l := newCandidateTestClient(t, upstream.Client())
	defer l.Close()
	client.BaseURL, client.APIKey = upstream.URL+"/v1", credential
	client.ErrorCodeMappings = []ProviderErrorMapping{{UpstreamCode: credential, Category: ProviderErrorInvalidRequest}}
	_, _, err := client.Call(context.Background(), []byte(`{"model":"m","messages":[{"role":"user","content":"probe"}]}`), "m")
	var e *CallError
	if !errors.As(err, &e) || e.Failure == nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(e.Failure)
	if e.ProviderCode != "" || e.Failure.ProviderCode != "" || e.Failure.ProviderRequestID != "" || strings.Contains(string(encoded), credential) {
		t.Fatalf("credential echo not redacted: %s", encoded)
	}
}
