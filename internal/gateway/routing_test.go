package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

func addRoutingProvider(c *Config, ref, baseURL, protocol string, models ...string) {
	if c.Providers == nil {
		c.Providers = make(map[string]Provider)
	}
	c.Providers[ref] = Provider{
		Protocol: protocol, BaseURL: baseURL + "/v1", UpstreamID: ref,
		APIKey:           "provider-key-" + ref,
		UpstreamKeychain: KeychainReference{Service: "test.provider", Account: ref},
		SupportedModels:  models,
	}
}

func routingTestConfig(path string) Config {
	c := testConfig(path, "https://legacy.example/v1")
	c.Protocol, c.BaseURL, c.APIVersion, c.APIKey = "", "", "", ""
	c.UpstreamKeychain, c.UpstreamID, c.Model = KeychainReference{}, "", ""
	c.Providers = nil
	return c
}

func configureRouting(c *Config, settings RoutingConfig) {
	if settings == (RoutingConfig{}) {
		c.Routing = nil
		return
	}
	c.Routing = &settings
}

func routingRequest(t *testing.T, h http.Handler, protocol, body string) *httptest.ResponseRecorder {
	t.Helper()
	path := "/v1/chat/completions"
	if protocol == "anthropic" {
		path = "/v1/messages"
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if protocol == "anthropic" {
		req.Header.Set("x-api-key", "local-secret")
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer local-secret")
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, req)
	return response
}

func openAIResponseForModel(model string) string {
	return strings.Replace(responseBody("openai"), `"model":"custom-model"`, `"model":"`+model+`"`, 1)
}

func TestExplicitModelDoesNotUseAutoChain(t *testing.T) {
	var requested []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"model-one"},{"id":"model-two"}]}`)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		requested = append(requested, body.Model)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"missing_model"}}`)
	}))
	defer upstream.Close()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "provider-a", upstream.URL, "openai", "model-one", "model-two")
	provider := c.Providers["provider-a"]
	c.AutoChain = []AutoChainEntry{{Provider: "provider-a", Model: "model-one"}, {Provider: "provider-a", Model: "model-two"}}
	provider.ErrorCodeMappings = []adapter.ProviderErrorMapping{{UpstreamCode: "missing_model", HTTPStatus: 404, Category: adapter.ProviderErrorModelNotFound}}
	c.Providers["provider-a"] = provider
	h, closeDB, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()

	response := routingRequest(t, h, "openai", `{"model":"provider-a/model-one","messages":[{"role":"user","content":"hello"}]}`)
	if response.Code != http.StatusNotFound || strings.Join(requested, ",") != "model-one" {
		t.Fatalf("status=%d requested=%v body=%s", response.Code, requested, response.Body.String())
	}
}

func TestSharedModelStrategiesChooseOneEligibleProvider(t *testing.T) {
	for _, strategy := range []string{"random", "price_priority"} {
		t.Run(strategy, func(t *testing.T) {
			callsA, callsB := 0, 0
			newUpstream := func(calls *int) *httptest.Server {
				return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"shared-model"}]}`)
						return
					}
					if r.Method == http.MethodPost {
						*calls++
					}
					_, _ = io.WriteString(w, openAIResponseForModel("shared-model"))
				}))
			}
			upA, upB := newUpstream(&callsA), newUpstream(&callsB)
			defer upA.Close()
			defer upB.Close()
			c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
			addRoutingProvider(&c, "a", upA.URL, "openai", "shared-model")
			addRoutingProvider(&c, "b", upB.URL, "openai", "shared-model")
			low := testPrice()
			high := testPrice()
			*low.InputCacheHit, *low.InputCacheMiss, *low.Output = 1, 2, 3
			*high.InputCacheHit, *high.InputCacheMiss, *high.Output = 4, 5, 6
			a, b := c.Providers["a"], c.Providers["b"]
			if strategy == "price_priority" {
				a.Prices = map[string]adapter.Price{"shared-model": high}
				b.Prices = map[string]adapter.Price{"shared-model": low}
			}
			c.Providers["a"], c.Providers["b"] = a, b
			configureRouting(&c, RoutingConfig{SharedModelStrategy: strategy})
			h, closeDB, err := NewHandler(c, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()
			if strategy == "random" {
				h.(*handler).randomIndex = func(size int) int {
					if size != 2 {
						t.Errorf("random choice size=%d", size)
					}
					return 1
				}
			}
			response := routingRequest(t, h, "openai", `{"model":"shared-model","messages":[{"role":"user","content":"hello"}]}`)
			if response.Code != http.StatusOK || callsA != 0 || callsB != 1 {
				t.Fatalf("status=%d calls=%d/%d body=%s", response.Code, callsA, callsB, response.Body.String())
			}
			rows, err := h.(*handler).ledger.Recent(context.Background(), 10)
			if err != nil || len(rows) != 1 || rows[0].ProviderRef != "b" || rows[0].Model != "shared-model" {
				t.Fatalf("selected provider/model attribution missing: rows=%+v err=%v", rows, err)
			}
		})
	}
}

func TestPricePriorityRejectsIncomparablePrices(t *testing.T) {
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "a", "https://a.example", "openai", "shared-model")
	addRoutingProvider(&c, "b", "https://b.example", "openai", "shared-model")
	a, b := c.Providers["a"], c.Providers["b"]
	a.Prices = map[string]adapter.Price{"shared-model": testPrice()}
	bPrice := testPrice()
	bPrice.Currency = "EUR"
	b.Prices = map[string]adapter.Price{"shared-model": bPrice}
	c.Providers["a"], c.Providers["b"] = a, b
	configureRouting(&c, RoutingConfig{SharedModelStrategy: "price_priority"})
	h := &handler{handlerRuntime: &handlerRuntime{}, config: c, providers: c.Providers, clients: map[string]*adapter.Client{
		"a": {Protocol: "openai"}, "b": {Protocol: "openai"},
	}}
	if _, err := h.orderSharedModelProviders("shared-model", []string{"a", "b"}); err == nil {
		t.Fatal("mixed-currency rates were compared")
	}
	bPrice.Currency = "USD"
	bPrice.InputCacheHit = nil
	b.Prices = map[string]adapter.Price{"shared-model": bPrice}
	h.providers["b"] = b
	if _, err := h.orderSharedModelProviders("shared-model", []string{"a", "b"}); err == nil {
		t.Fatal("incomplete rates were treated as a low price")
	}
}

func TestSharedRoutingExcludesCoolingProvidersAndBoundsBillingAttempts(t *testing.T) {
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	for _, ref := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		addRoutingProvider(&c, ref, "https://"+ref+".example", "openai", "shared-model")
	}
	configureRouting(&c, RoutingConfig{SharedModelStrategy: "random", BillingExhaustionFailover: true})
	clients := make(map[string]*adapter.Client)
	pools := make(map[string]*providerKeyPool)
	for ref, provider := range c.Providers {
		clients[ref] = &adapter.Client{Protocol: provider.Protocol}
		pools[ref] = newProviderKeyPool([]string{"key-" + ref})
	}
	now := time.Now()
	pools["a"].CooldownAt(0, time.Minute, now)
	h := &handler{handlerRuntime: &handlerRuntime{}, config: c, providers: c.Providers, clients: clients, keyPools: pools, randomIndex: func(int) int { return 0 }}
	routes, code, status := h.resolveRoutes("shared-model", "openai", []byte(`{"model":"shared-model"}`), nil)
	if code != "" || status != 0 || len(routes) == 0 || routes[0].provider != "b" {
		t.Fatalf("cooldown selection routes=%+v code=%q status=%d", routes, code, status)
	}
	for _, route := range routes {
		if route.provider == "a" {
			t.Fatalf("cooling provider was selected: %+v", routes)
		}
	}
	routes = h.appendBillingRoutes([]routeStep{{provider: "b", model: "shared-model", trigger: routeInitial}}, "b", "shared-model", "openai", nil)
	if len(routes) != 1+maxBillingProviderAttempts {
		t.Fatalf("billing route count=%d want at most %d", len(routes), 1+maxBillingProviderAttempts)
	}
	seen := make(map[string]bool)
	for _, route := range routes {
		if seen[route.provider] {
			t.Fatalf("provider repeated in billing routes: %+v", routes)
		}
		seen[route.provider] = true
	}
}

func TestRouteAdvanceUsesOnlySafeClassifications(t *testing.T) {
	billing := routeStep{trigger: routeBillingProvider}
	tests := []struct {
		name      string
		route     routeStep
		callErr   *adapter.CallError
		config    RoutingConfig
		ctxErr    error
		delivered bool
		want      bool
	}{
		{name: "arbitrary 403", route: billing, callErr: &adapter.CallError{Status: 502, Code: "upstream_error", UpstreamStatus: 403}, config: RoutingConfig{BillingExhaustionFailover: true}, want: false},
		{name: "429 never switches provider", route: billing, callErr: &adapter.CallError{FailoverSafe: true, Retryable: true, RateLimited: true, Category: adapter.ProviderErrorRateLimited}, config: RoutingConfig{BillingExhaustionFailover: true}, want: false},
		{name: "mapped balance opted in", route: billing, callErr: &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorInsufficientBalance}, config: RoutingConfig{BillingExhaustionFailover: true}, want: true},
		{name: "mapped balance opt out", route: billing, callErr: &adapter.CallError{FailoverSafe: true, Category: adapter.ProviderErrorInsufficientBalance}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := routeCanAdvance(test.config, test.route, test.callErr, test.ctxErr, test.delivered); got != test.want {
				t.Fatalf("routeCanAdvance=%t want %t", got, test.want)
			}
		})
	}
}

func TestBillingFailoverRequiresExactMappedSignalAndAuditsEachProvider(t *testing.T) {
	for _, mapped := range []bool{false, true} {
		t.Run(map[bool]string{false: "unmapped", true: "mapped"}[mapped], func(t *testing.T) {
			callsA, callsB := 0, 0
			upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"shared-model"}]}`)
					return
				}
				callsA++
				w.WriteHeader(http.StatusForbidden)
				code := "access_denied"
				if mapped {
					code = "credits_empty"
				}
				_, _ = io.WriteString(w, `{"error":{"code":"`+code+`","message":"insufficient funds private detail"}}`)
			}))
			defer upA.Close()
			upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"shared-model"}]}`)
					return
				}
				callsB++
				_, _ = io.WriteString(w, openAIResponseForModel("shared-model"))
			}))
			defer upB.Close()
			c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
			addRoutingProvider(&c, "provider-a", upA.URL, "openai", "shared-model")
			addRoutingProvider(&c, "provider-b", upB.URL, "openai", "shared-model")
			providerA := c.Providers["provider-a"]
			providerA.ErrorCodeMappings = []adapter.ProviderErrorMapping{{UpstreamCode: "credits_empty", HTTPStatus: 403, Category: adapter.ProviderErrorInsufficientBalance}}
			c.Providers["provider-a"] = providerA
			configureRouting(&c, RoutingConfig{BillingExhaustionFailover: true})
			h, closeDB, err := NewHandler(c, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer closeDB()
			response := routingRequest(t, h, "openai", `{"model":"provider-a/shared-model","messages":[{"role":"user","content":"hello"}]}`)
			if mapped {
				if response.Code != http.StatusOK || callsA != 1 || callsB != 1 {
					t.Fatalf("status=%d calls=%d/%d body=%s", response.Code, callsA, callsB, response.Body.String())
				}
				rows, err := h.(*handler).ledger.Recent(context.Background(), 10)
				if err != nil || len(rows) != 2 {
					t.Fatalf("audits=%+v err=%v", rows, err)
				}
				byProvider := map[string]bool{}
				for _, row := range rows {
					byProvider[row.Upstream] = true
					if row.Model != "shared-model" {
						t.Fatalf("wrong model attribution: %+v", row)
					}
					if row.Upstream == "provider-b" && (row.Status != "ok" || row.InputTokens == nil || *row.InputTokens != 3) {
						t.Fatalf("successful provider audit=%+v", row)
					}
				}
				if !byProvider["provider-a"] || !byProvider["provider-b"] {
					t.Fatalf("provider attempts missing from audit: %v", byProvider)
				}
				if h.(*handler).providerRouteAvailable("provider-a") {
					t.Fatal("mapped billing exhaustion did not cool down that provider's keys")
				}
			} else if response.Code == http.StatusOK || callsA != 1 || callsB != 0 || strings.Contains(response.Body.String(), "private detail") {
				t.Fatalf("unmapped 403 crossed provider: status=%d calls=%d/%d body=%s", response.Code, callsA, callsB, response.Body.String())
			}
		})
	}
}

func TestNativeAnthropicToolsRouteOnlyToAnthropicProvider(t *testing.T) {
	openAICalls, anthropicCalls := 0, 0
	openAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"shared-model"}]}`)
			return
		}
		openAICalls++
		_, _ = io.WriteString(w, responseBody("openai"))
	}))
	defer openAI.Close()
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[{"id":"shared-model","type":"model"}],"has_more":false}`)
			return
		}
		anthropicCalls++
		_, _ = io.WriteString(w, responseBody("anthropic"))
	}))
	defer anthropic.Close()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "a-openai", openAI.URL, "openai", "shared-model")
	addRoutingProvider(&c, "b-anthropic", anthropic.URL, "anthropic", "shared-model")
	configureRouting(&c, RoutingConfig{SharedModelStrategy: "random"})
	h, closeDB, err := NewHandler(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	h.(*handler).randomIndex = func(size int) int { return 0 }
	body := `{"model":"shared-model","max_tokens":32,"messages":[{"role":"user","content":"search"}],"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":2}]}`
	response := routingRequest(t, h, "anthropic", body)
	if response.Code != http.StatusOK || openAICalls != 0 || anthropicCalls != 1 {
		t.Fatalf("status=%d calls openai=%d anthropic=%d body=%s", response.Code, openAICalls, anthropicCalls, response.Body.String())
	}
}

func TestAutoModelDoesNotReplayAfterStreamOutputAndRebindsNextRequest(t *testing.T) {
	var requested []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"model-one"},{"id":"model-two"}]}`)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		requested = append(requested, body.Model)
		if body.Model != "model-one" {
			_, _ = io.WriteString(w, openAIResponseForModel(body.Model))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "event: error\ndata: {\"error\":{\"code\":\"missing_model\",\"status\":404}}\n\n")
	}))
	defer upstream.Close()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "provider-a", upstream.URL, "openai", "model-one", "model-two")
	provider := c.Providers["provider-a"]
	c.AutoChain = []AutoChainEntry{{Provider: "provider-a", Model: "model-one"}, {Provider: "provider-a", Model: "model-two"}}
	provider.ErrorCodeMappings = []adapter.ProviderErrorMapping{{UpstreamCode: "missing_model", HTTPStatus: 404, Category: adapter.ProviderErrorModelNotFound}}
	c.Providers["provider-a"] = provider
	h, closeDB, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	body := `{"model":"auto","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	response := affinityGatewayRequest(h, "openai", "stream-session", body)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "partial") || !strings.Contains(response.Body.String(), "event: error") || strings.Join(requested, ",") != "model-one" {
		t.Fatalf("status=%d requested=%v body=%s", response.Code, requested, response.Body.String())
	}
	next := affinityGatewayRequest(h, "openai", "stream-session", `{"model":"auto","messages":[{"role":"user","content":"next"}]}`)
	if next.Code != http.StatusOK || strings.Join(requested, ",") != "model-one,model-two" {
		t.Fatalf("confirmed stream failure did not rebind the next request: status=%d requested=%v body=%s", next.Code, requested, next.Body.String())
	}
}

func TestBillingRoutesExcludeTheOtherClientProtocol(t *testing.T) {
	h := affinityUnitHandler(t)
	h.clients["b"].Protocol = "anthropic"
	routes := h.appendBillingRoutes([]routeStep{{provider: "a", model: "shared-model"}}, "a", "shared-model", "openai", nil)
	if len(routes) != 1 {
		t.Fatalf("OpenAI billing failover included an Anthropic provider: %+v", routes)
	}
	routes = h.appendBillingRoutes([]routeStep{{provider: "b", model: "shared-model"}}, "b", "shared-model", "anthropic", nil)
	if len(routes) != 1 {
		t.Fatalf("Anthropic billing failover included an OpenAI provider: %+v", routes)
	}
}

func TestSharedRoutingFiltersProtocolFeaturesBeyondServerTools(t *testing.T) {
	h := affinityUnitHandler(t)
	h.clients["b"].Protocol = "anthropic"
	h.randomIndex = func(int) int { return 0 }
	body := []byte(`{"model":"shared-model","max_tokens":32,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","media_type":"text/plain","data":"reference"}}]}]}`)
	route := affinityRoute(t, h, "shared-model", "anthropic", "session", body)
	if route.provider != "b" {
		t.Fatalf("unrepresentable document selected the OpenAI provider: %+v", route)
	}
	delete(h.providers, "b")
	if _, code, status := h.resolveRoutes("shared-model", "anthropic", body, h.sharedSessionKey("shared-model", "anthropic", "session")); code != "unsupported_request_feature" || status != http.StatusBadRequest {
		t.Fatalf("no compatible route returned code=%s status=%d", code, status)
	}
	if field := h.routeErrorParameter("shared-model", "anthropic", body); field != "messages[0].content[0].type" {
		t.Fatalf("unsupported feature lost its actionable field: %q", field)
	}
}
