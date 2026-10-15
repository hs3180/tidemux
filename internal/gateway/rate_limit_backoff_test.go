package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hs3180/tidemux/internal/adapter"
)

func TestRateLimitBackoffSilenceAndRecoveryWithoutTelemetry(t *testing.T) {
	p := newProviderKeyPool([]string{"key-a"})
	p.jitter = func(time.Duration) time.Duration { return 0 }
	p.setDiagnostics(false)
	now := time.Unix(1792000000, 0)
	waits := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute, 30 * time.Minute, 30 * time.Minute, 24 * time.Hour}
	var stale rateLimitObservation
	for i, want := range waits {
		ready, _, observation := p.candidateStateAt(0, now)
		if !ready {
			t.Fatalf("attempt %d not ready", i+1)
		}
		if i == 0 {
			stale = observation
		}
		keyWait, providerWait, count := p.rateLimitedAt(0, 0, false, now)
		if keyWait != want || providerWait != want || count != uint16(i+1) {
			t.Fatalf("attempt %d: key=%s provider=%s count=%d", i+1, keyWait, providerWait, count)
		}
		candidates, _ := p.CandidatesAt(now.Add(want - time.Nanosecond))
		if len(candidates) != 0 {
			t.Fatal("provider became eligible before the real deadline")
		}
		if p.rateSucceeded(0, stale) {
			t.Fatal("old success erased later rate limits")
		}
		now = now.Add(want)
	}
	ready, _, observation := p.candidateStateAt(0, now)
	if !ready || !p.rateSucceeded(0, observation) {
		t.Fatal("successful real probe did not reset the streak")
	}
	status := p.diagnosticStatus(now)
	if status.ConsecutiveRateLimits != 0 || status.Keys[0].ConsecutiveRateLimits != 0 || status.TelemetryEnabled || status.Keys[0].Counters != nil {
		t.Fatalf("operational state depends on telemetry: %+v", status)
	}
	wait, _, count := p.rateLimitedAt(0, 0, false, now)
	if wait != 30*time.Second || count != 1 {
		t.Fatalf("new sequence wait=%s count=%d", wait, count)
	}
}

func TestRateLimitPreservesHealthyPeerAndValidUpstreamDelay(t *testing.T) {
	p := newProviderKeyPool([]string{"key-a", "key-b"})
	p.jitter = func(time.Duration) time.Duration { return 0 }
	now := time.Unix(1792000000, 0)
	for i := 0; i < 11; i++ {
		wait, providerWait, _ := p.rateLimitedAt(0, 2*time.Second, true, now)
		if i < 10 && wait != 2*time.Second {
			t.Fatalf("valid upstream wait changed to %s", wait)
		}
		if i == 10 && wait != 24*time.Hour {
			t.Fatal("11th failure did not silence the failing key")
		}
		if providerWait != 0 {
			t.Fatal("one limited key cooled a healthy peer")
		}
		now = now.Add(wait)
	}
	ready, _, observation := p.candidateStateAt(1, now)
	if !ready || !p.rateSucceeded(1, observation) || p.diagnosticStatus(now).ConsecutiveRateLimits != 0 {
		t.Fatal("healthy peer success did not reset provider streak")
	}
}

func TestRateLimitedProviderOwnsOneProbeAfterSilence(t *testing.T) {
	s, now := availabilityFixture()
	lease, blocked := s.begin(1, "a", "one", 1)
	if blocked != nil {
		t.Fatal(blocked)
	}
	s.complete(lease, false, &adapter.CallError{Category: adapter.ProviderErrorRateLimited, AllKeysCooling: true, RateLimitFailures: 11, Cooldown: 24 * time.Hour}, nil)
	entry := s.entries[availabilityTarget{"a", "", 1}]
	if entry == nil || entry.failures != 11 || entry.reason != "rate_limited" || entry.retryAt.Sub(*now) != 24*time.Hour || !s.available(1, "b", "one", 2) {
		t.Fatalf("provider limit state=%+v", entry)
	}
	*now = now.Add(24*time.Hour - time.Nanosecond)
	if _, blocked = s.begin(1, "a", "one", 1); blocked == nil {
		t.Fatal("silent provider admitted an early probe")
	}
	*now = now.Add(time.Nanosecond)
	var count atomic.Int32
	var winner *availabilityLease
	var mu sync.Mutex
	var group sync.WaitGroup
	for i := 0; i < 40; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			candidate, blocked := s.begin(1, "a", "one", 1)
			if blocked == nil {
				count.Add(1)
				mu.Lock()
				winner = candidate
				mu.Unlock()
			}
		}()
	}
	group.Wait()
	if count.Load() != 1 {
		t.Fatalf("recovery probes=%d", count.Load())
	}
	s.complete(winner, false, nil, nil)
	if !s.available(1, "a", "one", 1) || s.entries[availabilityTarget{"a", "", 1}].failures != 0 {
		t.Fatal("successful probe did not restore provider")
	}
}

func TestRateLimitMovesFromKeyToProviderWithoutReplaying(t *testing.T) {
	var posts atomic.Int32
	probeStarted, releaseProbe := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseProbe) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			io.WriteString(w, `{"data":[{"id":"custom-model"}]}`)
			return
		}
		if posts.Add(1) > 2 {
			select {
			case probeStarted <- struct{}{}:
			case <-releaseProbe:
			}
			<-releaseProbe
			io.WriteString(w, responseBody("openai"))
			return
		}
		w.WriteHeader(429)
		io.WriteString(w, `{"error":{"code":"1310","message":"PRIVATE_QUOTA_MESSAGE"}}`)
	}))
	defer func() { release(); upstream.Close() }()
	c := namedProviderConfig(testConfig(filepath.Join(t.TempDir(), "ledger.db"), upstream.URL+"/v1"), "p")
	c.MaxInFlight = 0
	p := c.Providers["p"]
	p.APIKey = "key-a"
	p.APIKeys = []string{"key-a", "key-b"}
	c.Providers["p"] = p
	off := false
	c.HealthDiagnostics = &off
	raw, closeDB, err := NewHandler(c, upstream.Client())
	if err != nil {
		t.Fatal(err)
	}
	defer closeDB()
	h := raw.(*handler)
	body := requestBodyFor("openai", "p", "custom-model")
	for i := 0; i < 2; i++ {
		w := affinityGatewayRequest(h, "openai", "session", body)
		if w.Code != 429 || posts.Load() != int32(i+1) {
			t.Fatalf("attempt %d status=%d posts=%d", i+1, w.Code, posts.Load())
		}
		available := h.modelRouteAvailable("p", "custom-model")
		if available != (i == 0) {
			t.Fatalf("after attempt %d available=%t", i+1, available)
		}
	}
	w := affinityGatewayRequest(h, "openai", "session", body)
	if w.Code != 503 || posts.Load() != 2 {
		t.Fatalf("cooled provider replayed: status=%d posts=%d", w.Code, posts.Load())
	}
	report := h.availabilityReport()
	if report.Providers[0].Reason != "rate_limited" || report.Providers[0].ConsecutiveFailures != 2 {
		t.Fatalf("report=%+v", report)
	}
	rows, err := h.ledger.Recent(context.Background(), 10)
	if err != nil || len(rows) != 2 {
		t.Fatalf("audits=%d err=%v", len(rows), err)
	}
	for _, row := range rows {
		if row.Failure == nil || row.Failure.CooldownSeconds < 30 || row.Failure.CooldownUntilMS <= row.TimestampMS || row.Failure.ProviderRateLimitStreak == 0 {
			t.Fatalf("missing operational failure history: %+v", row.Failure)
		}
	}
	// Simulate expiry after a full silence without touching the cache just
	// before the deadline: idle eviction must not lose recovery ownership.
	pool := h.keyPools["p"]
	pool.mu.Lock()
	for i := range pool.cooldowns {
		pool.cooldowns[i] = time.Now().Add(-time.Second)
	}
	pool.rateProviderUntil = time.Now().Add(-time.Second)
	pool.mu.Unlock()
	h.availability.mu.Lock()
	h.availability.entries = map[availabilityTarget]*availabilityEntry{}
	h.availability.mu.Unlock()
	if got := h.availabilityReport().Providers[0]; got.State != "cooling" || !got.ProbeDue || got.Reason != "rate_limited" {
		t.Fatalf("eviction lost operational recovery state: %+v", got)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- affinityGatewayRequest(h, "openai", "session", body) }()
	select {
	case <-probeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery probe never started")
	}
	var unexpected atomic.Int32
	var group sync.WaitGroup
	for i := 0; i < 40; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if w := affinityGatewayRequest(h, "openai", "session", body); w.Code != 503 {
				unexpected.Add(1)
			}
		}()
	}
	group.Wait()
	if unexpected.Load() != 0 || posts.Load() != 3 {
		t.Fatalf("concurrent recovery admitted extra work: unexpected=%d posts=%d", unexpected.Load(), posts.Load())
	}
	release()
	select {
	case w := <-done:
		if w.Code != 200 || pool.diagnosticStatus(time.Now()).ConsecutiveRateLimits != 0 {
			t.Fatalf("real recovery did not reset state: status=%d", w.Code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovery probe did not finish")
	}
}

func TestRateLimitProbeOwnershipAndPublicationFence(t *testing.T) {
	p := newProviderKeyPool([]string{"key"})
	now := time.Now()
	p.rateLimitedAt(0, time.Second, true, now.Add(-2*time.Second))
	first, blocked := p.beginRateRecovery(now)
	if first == nil || blocked != nil {
		t.Fatalf("first probe=%p blocked=%v", first, blocked)
	}
	if _, blocked := p.beginRateRecovery(now); blocked == nil || blocked.Cooldown != 0 {
		t.Fatal("parallel probe admitted, or an unknown finish time was invented")
	}
	p.finishRateRecovery(first) // local cancellation releases ownership
	second, blocked := p.beginRateRecovery(now)
	if second == nil || blocked != nil {
		t.Fatal("cancelled probe retained ownership")
	}
	p.finishRateRecovery(first)
	if _, blocked := p.beginRateRecovery(now); blocked == nil {
		t.Fatal("old completion released the new owner's probe")
	}
	p.finishRateRecovery(second)
	s, _ := availabilityFixture()
	s.activate(2, map[string]uint64{"a": 1, "b": 2})
	if s.withCurrent(1, "a", 1, func() { p.rateLimitedAt(0, 0, false, now) }) {
		t.Fatal("old routing epoch mutated a reused pool")
	}
	if p.diagnosticStatus(now).ConsecutiveRateLimits != 1 {
		t.Fatal("old failure changed the operational streak")
	}
}
