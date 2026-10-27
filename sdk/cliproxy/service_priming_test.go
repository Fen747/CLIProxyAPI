package cliproxy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/maintenance"
)

func primingTestSetup(t *testing.T) (*quotaPrimer, *config.Config, *coreauth.InitialQuotaPolicy, []*coreauth.Auth, *maintenance.Gate, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	cfg := &config.Config{AuthDir: t.TempDir(), Routing: config.RoutingConfig{SessionAffinity: true, QuotaAwareInitial: true, ClaudeWindowPriming: true}}
	m := coreauth.NewManager(nil, newRoutingSelector(normalizedRoutingRuntimeState(cfg)), nil)
	t.Cleanup(m.StopAutoRefresh)
	for _, id := range []string{"a", "b"} {
		_, err := m.Register(coreauth.WithSkipPersist(context.Background()), &coreauth.Auth{ID: id, Provider: "claude", Metadata: map[string]any{"access_token": "private-token", "account_uuid": "same-subscription"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	auths := m.List()
	gate := maintenance.New()
	p := newQuotaPrimer(m, gate.Begin, func(context.Context, *config.Config, *coreauth.Auth) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"five_hour":{"utilization":0,"resets_at":%q},"seven_day":null}`, now.Add(5*time.Hour).Format(time.RFC3339))), nil
	})
	p.authDir = cfg.AuthDir
	p.now = func() time.Time { return now }
	p.run = func(context.Context, *coreauth.Auth) error { return nil }
	t.Cleanup(p.stop)
	policy := m.InitialQuotaPolicy()
	for _, a := range auths {
		if err := policy.Observe(a, []byte(`{"five_hour":null,"seven_day":null}`), now); err != nil {
			t.Fatal(err)
		}
	}
	p.reconcile(cfg, auths)
	return p, cfg, policy, auths, gate, now
}

func TestQuotaPrimerPersistsBeforeInferenceAndConfirmsReset(t *testing.T) {
	p, cfg, policy, auths, gate, now := primingTestSetup(t)
	persisted := make(chan bool, 1)
	p.run = func(_ context.Context, a *coreauth.Auth) error {
		data, err := os.ReadFile(filepath.Join(cfg.AuthDir, ".claude-window-priming.state"))
		persisted <- err == nil && strings.Contains(string(data), primingSubscriptionKey(a)) && !strings.Contains(string(data), "private-token")
		return nil
	}
	if !p.observe(context.Background(), cfg, policy, auths[0], now) {
		t.Fatal("inactive window did not start")
	}
	if !<-persisted {
		t.Fatal("inference preceded durable reservation")
	}
	result := <-p.results
	p.workers.Wait()
	if !p.complete(result, now) {
		t.Fatal("reset not confirmed")
	}
	if gate.State().Active != 0 {
		t.Fatal("background admission leaked")
	}
	if ok, err := p.state.Reserve(primingSubscriptionKey(auths[0]), now.Add(time.Hour)); err != nil || ok {
		t.Fatalf("confirmed window not suppressed: %v/%v", ok, err)
	}
}

func TestQuotaPrimerRejectsDrainAndKeepsAdmittedWorkCounted(t *testing.T) {
	p, cfg, policy, auths, gate, now := primingTestSetup(t)
	_, _ = gate.Drain("owner")
	if p.observe(context.Background(), cfg, policy, auths[0], now) {
		t.Fatal("primed during maintenance")
	}
	if _, err := os.Stat(p.path); !os.IsNotExist(err) {
		t.Fatal("rejected admission consumed attempt")
	}
	_, _ = gate.Resume("owner")
	started, release := make(chan struct{}), make(chan struct{})
	p.run = func(context.Context, *coreauth.Auth) error { close(started); <-release; return nil }
	if !p.observe(context.Background(), cfg, policy, auths[0], now) {
		t.Fatal("not admitted")
	}
	<-started
	state, _ := gate.Drain("owner")
	if state.Ready || state.Active != 1 {
		t.Fatalf("drain ignored priming: %+v", state)
	}
	close(release)
	result := <-p.results
	p.workers.Wait()
	p.complete(result, now)
	if !gate.State().Ready {
		t.Fatal("drain did not complete")
	}
}

func TestQuotaPrimerDisableCancelsAndRestartKeepsAttempt(t *testing.T) {
	p, cfg, policy, auths, gate, now := primingTestSetup(t)
	started := make(chan struct{})
	p.run = func(ctx context.Context, _ *coreauth.Auth) error { close(started); <-ctx.Done(); return ctx.Err() }
	if !p.observe(context.Background(), cfg, policy, auths[0], now) {
		t.Fatal("not admitted")
	}
	<-started
	cfg.Routing.ClaudeWindowPriming = false
	p.reconcile(cfg, auths)
	result := <-p.results
	p.workers.Wait()
	if p.complete(result, now) {
		t.Fatal("cancelled result accepted")
	}
	if gate.State().Active != 0 {
		t.Fatal("cancelled admission leaked")
	}
	state, err := openPrimingState(filepath.Join(cfg.AuthDir, ".claude-window-priming.state"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if ok, err := state.Reserve(primingSubscriptionKey(auths[0]), now.Add(time.Minute)); err != nil || ok {
		t.Fatalf("restart lost attempt: %v/%v", ok, err)
	}
}

func TestQuotaPrimerDeduplicatesSubscriptionAndRejectsMissingState(t *testing.T) {
	p, cfg, policy, auths, _, now := primingTestSetup(t)
	p.fetch = func(context.Context, *config.Config, *coreauth.Auth) ([]byte, error) {
		return nil, fmt.Errorf("unknown result")
	}
	if !p.observe(context.Background(), cfg, policy, auths[0], now) {
		t.Fatal("not admitted")
	}
	result := <-p.results
	p.workers.Wait()
	p.complete(result, now)
	if p.observe(context.Background(), cfg, policy, auths[1], now) {
		t.Fatal("duplicate subscription primed twice")
	}
	p.state.Close()
	p.state = nil
	p.failed = true
	if p.observe(context.Background(), cfg, policy, auths[1], now.Add(5*time.Minute)) {
		t.Fatal("primed without durable state")
	}
}
