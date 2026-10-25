package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
	"github.com/hs3180/tidemux/internal/ledger"
)

func TestProviderModelAliasesValidateAndRoundTrip(t *testing.T) {
	c := namedProviderConfig(testConfig("ledger.db", "https://provider.example/v1"), "ark")
	for _, aliases := range []map[string]string{
		{"kimi-k3": "kimi-k3.1-pro"}, {"stable": "vendor/model-v2"}, {},
	} {
		p := c.Providers["ark"]
		p.ModelAliases = aliases
		c.Providers["ark"] = p
		data, _ := json.Marshal(c)
		got, err := decodeValidatedConfig(data)
		if err != nil || len(got.Providers["ark"].ModelAliases) != len(aliases) {
			t.Fatalf("alias roundtrip: %v", err)
		}
	}
	for _, aliases := range []map[string]string{
		{"": "remote"}, {"stable": ""}, {" stable": "remote"}, {"stable": "remote\n"},
		{"ark/stable": "remote"}, {"auto": "remote"}, {"stable": "auto"},
		{"stable": "stable"}, {"a": "b", "b": "remote"}, {"a": "b", "b": "a"},
		{strings.Repeat("a", 129): "remote"}, {"stable": strings.Repeat("b", 129)},
	} {
		p := c.Providers["ark"]
		p.ModelAliases = aliases
		c.Providers["ark"] = p
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "model_aliases") {
			t.Fatal("invalid alias map accepted")
		}
	}
}

func aliasStream(protocol, model string) string {
	if protocol == "anthropic" {
		return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"" + model + "\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
			"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	}
	return "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"" + model + "\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"" + model + "\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
}

func TestProviderAliasDispatchPreservesClientNamesAndAuditsTarget(t *testing.T) {
	for _, clientProtocol := range []string{"openai", "anthropic"} {
		for _, upstreamProtocol := range []string{"openai", "anthropic"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s-to-%s/stream=%t", clientProtocol, upstreamProtocol, stream), func(t *testing.T) {
					const target = "kimi-k3.1-pro"
					var calls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == http.MethodGet {
							_, _ = io.WriteString(w, `{"object":"list","has_more":false,"data":[{"id":"kimi-k3.1-pro","context_length":65536,"max_output_tokens":64}]}`)
							return
						}
						calls.Add(1)
						var body map[string]any
						if json.NewDecoder(r.Body).Decode(&body) != nil || body["model"] != target {
							t.Error("client alias reached upstream")
						}
						if clientProtocol == "openai" && upstreamProtocol == "anthropic" && body["max_tokens"] != float64(64) {
							t.Errorf("target output default not applied: %v", body["max_tokens"])
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							_, _ = io.WriteString(w, aliasStream(upstreamProtocol, target))
						} else {
							_, _ = io.WriteString(w, strings.ReplaceAll(responseBody(upstreamProtocol), "custom-model", target))
						}
					}))
					defer upstream.Close()
					c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
					addRoutingProvider(&c, "ark", upstream.URL, upstreamProtocol, "kimi-k3")
					p := c.Providers["ark"]
					p.ModelAliases = map[string]string{"kimi-k3": target}
					price, stale := testPrice(), testPrice()
					price.Version, stale.Version = "target-price", "old-alias-price"
					p.Prices = map[string]adapter.Price{target: price, "kimi-k3": stale}
					c.Providers["ark"] = p
					c.AutoChain = []AutoChainEntry{{Provider: "ark", Model: "kimi-k3"}}
					h, closeDB, err := NewHandler(c, upstream.Client())
					if err != nil {
						t.Fatal(err)
					}
					defer closeDB()
					root := h.(*handler)
					if root.capabilitiesForModel("ark", "kimi-k3") != (ModelCapabilities{ContextTokens: 65536, MaxOutputTokens: 64}) {
						t.Fatal("alias capabilities did not use target catalog")
					}
					for _, requested := range []string{"kimi-k3", "ark/kimi-k3", "auto"} {
						body := map[string]any{"model": requested, "messages": []map[string]string{{"role": "user", "content": "hello"}}, "stream": stream}
						if clientProtocol == "anthropic" {
							body["max_tokens"] = 16
						}
						data, _ := json.Marshal(body)
						response := routingRequest(t, h, clientProtocol, string(data))
						if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"model":"kimi-k3"`) || strings.Contains(response.Body.String(), target) {
							t.Fatalf("alias response mismatch: %d %s", response.Code, response.Body.String())
						}
						if stream && clientProtocol == "openai" && !strings.Contains(response.Body.String(), "[DONE]") || stream && clientProtocol == "anthropic" && !strings.Contains(response.Body.String(), "message_stop") {
							t.Fatal("stream lost terminal marker")
						}
					}
					if rejected := routingRequest(t, h, clientProtocol, requestBodyFor(clientProtocol, "ark", target)); rejected.Code != http.StatusNotFound || calls.Load() != 3 {
						t.Fatal("alias expanded the client allowlist or replayed a request")
					}
					rows, err := root.ledger.Recent(context.Background(), 10)
					if err != nil || len(rows) != 3 {
						t.Fatalf("terminal audits: %v count=%d", err, len(rows))
					}
					for _, row := range rows {
						if row.Model != target || row.ProviderRef != "ark" || row.Status != "ok" || row.EstimatedCost == nil || !strings.Contains(string(row.PriceSnapshot), "target-price") {
							t.Fatalf("wrong target accounting: %+v", row)
						}
					}
				})
			}
		}
	}
}

func TestAliasesShareTargetHealthAndCannotBypassCooldown(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"unavailable"}}`)
	}))
	defer upstream.Close()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "ark", upstream.URL, "openai", "stable", "other")
	p := c.Providers["ark"]
	p.ModelAliases = map[string]string{"stable": "remote", "other": "remote"}
	c.Providers["ark"] = p
	h, closeDB, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	if first := routingRequest(t, h, "openai", requestBodyFor("openai", "ark", "stable")); first.Code != http.StatusBadGateway {
		t.Fatal(first.Code)
	}
	if second := routingRequest(t, h, "openai", requestBodyFor("openai", "ark", "other")); second.Code != http.StatusTooManyRequests || calls.Load() != 1 {
		t.Fatal("second alias bypassed the target cooldown")
	}
	root := h.(*handler)
	report := root.availabilityReport()
	if len(report.Providers) != 1 || len(report.Providers[0].Models) != 2 {
		t.Fatal("target health not exposed under both client aliases")
	}
	view, err := root.prepareConfigView(context.Background(), c, c, root, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	view.activateRouting()
	if view.modelRouteAvailable("ark", "stable") || view.modelRouteAvailable("ark", "other") {
		t.Fatal("policy reload discarded aliased target health")
	}
}

func TestAliasBudgetDoesNotUseStaleClientNamePrice(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls.Add(1)
		_, _ = io.WriteString(w, responseBody("openai"))
	}))
	defer upstream.Close()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	addRoutingProvider(&c, "ark", upstream.URL, "openai", "stable")
	p := c.Providers["ark"]
	p.ModelAliases = map[string]string{"stable": "remote"}
	p.Prices = map[string]adapter.Price{"stable": testPrice()}
	p.Budget = &ledger.BudgetPolicy{Currency: "USD", FiveHourLimit: 1, WeeklyLimit: 1, AlertThreshold: .5, Mode: "hard"}
	c.Providers["ark"] = p
	h, closeDB, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	w := routingRequest(t, h, "openai", requestBodyFor("openai", "ark", "stable"))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "budget_pricing_unconfigured") || calls.Load() != 0 {
		t.Fatalf("stale alias price used for target budget: %d %s", w.Code, w.Body.String())
	}
}

func TestAliasReloadKeepsAdmittedTargetPricesAndFencesOldCompletion(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Model == "remote-v1" {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, openAIResponseForModel(body.Model))
	}))
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		upstream.Close()
	}()
	c := routingTestConfig(filepath.Join(t.TempDir(), "ledger.db"))
	c.MaxInFlight = 0
	addRoutingProvider(&c, "ark", upstream.URL, "openai", "stable")
	p := c.Providers["ark"]
	p.ModelAliases = map[string]string{"stable": "remote-v1"}
	oldPrice, newPrice := testPrice(), testPrice()
	oldPrice.Version, newPrice.Version = "old", "new"
	p.Prices = map[string]adapter.Price{"remote-v1": oldPrice, "remote-v2": newPrice}
	c.Providers["ark"] = p
	h, closeDB, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	root := h.(*handler)
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- routingRequest(t, h, "openai", requestBodyFor("openai", "ark", "stable")) }()
	select {
	case <-entered:
	case first := <-done:
		t.Fatal("admitted request did not reach upstream", first.Code, first.Body.String())
	case <-time.After(5 * time.Second):
		t.Fatal("admitted request did not reach upstream")
	}
	next := c
	next.Providers = map[string]Provider{}
	p.ModelAliases = map[string]string{"stable": "remote-v2"}
	next.Providers["ark"] = p
	view, err := root.prepareConfigView(context.Background(), next, c, root, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	view.activateRouting()
	if !reflect.DeepEqual(root.providers["ark"].ModelAliases, map[string]string{"stable": "remote-v1"}) || view.keyPools["ark"] != root.keyPools["ark"] || view.providerGenerations["ark"] != root.providerGenerations["ark"] {
		t.Fatal("reload mutated admitted mapping or reset provider/key identity")
	}
	w := routingRequest(t, view, "openai", requestBodyFor("openai", "ark", "stable"))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	close(release)
	if old := <-done; old.Code != http.StatusOK {
		t.Fatal(old.Code, old.Body.String())
	}
	rows, err := root.ledger.Recent(context.Background(), 10)
	if err != nil || len(rows) != 2 {
		t.Fatal("missing reload audits", err)
	}
	for _, row := range rows {
		want := "old"
		if row.Model == "remote-v2" {
			want = "new"
		} else if row.Model != "remote-v1" {
			t.Fatal("audit used client name")
		}
		if !strings.Contains(string(row.PriceSnapshot), `"version":"`+want+`"`) {
			t.Fatal("admitted target lost price snapshot")
		}
	}
	if !view.modelRouteAvailable("ark", "stable") {
		t.Fatal("old completion damaged remapped route")
	}
}
