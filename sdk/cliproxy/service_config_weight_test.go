package cliproxy

import (
	"context"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestWeightedRoundRobinRoutingSelector(t *testing.T) {
	state := normalizedRoutingRuntimeState(&internalconfig.Config{
		Routing: internalconfig.RoutingConfig{Strategy: "wrr"},
	})
	if state.strategy != "weighted-round-robin" {
		t.Fatalf("strategy = %q, want weighted-round-robin", state.strategy)
	}
	if _, ok := newRoutingSelector(state).(*coreauth.WeightedRoundRobinSelector); !ok {
		t.Fatalf("selector type = %T, want *auth.WeightedRoundRobinSelector", newRoutingSelector(state))
	}
}

func TestServiceRejectsInvalidCredentialWeightConfigCommit(t *testing.T) {
	originalCfg := &internalconfig.Config{}
	service := &Service{cfg: originalCfg}
	invalidWeight := internalconfig.MaxCredentialWeight + 1
	newCfg := &internalconfig.Config{
		VertexCompatAPIKey: []internalconfig.VertexCompatKey{{
			APIKey: "vertex-key",
			Weight: &invalidWeight,
		}},
	}

	if service.applyConfigUpdateWithAuthSynthesis(nil, newCfg, true) {
		t.Fatal("hot config application accepted an invalid credential weight")
	}
	if service.cfg != originalCfg {
		t.Fatal("invalid hot config replaced the active config")
	}
	if service.configSequence != 0 {
		t.Fatalf("config sequence = %d, want 0", service.configSequence)
	}
}

type trackingStoppableSelector struct {
	stopped bool
}

func (s *trackingStoppableSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*coreauth.Auth) (*coreauth.Auth, error) {
	return nil, nil
}

func (s *trackingStoppableSelector) Stop() {
	s.stopped = true
}

func TestApplyManagerConfigStopsReplacedServiceAffinitySelector(t *testing.T) {
	tracking := &trackingStoppableSelector{}
	service := &Service{
		coreManager: coreauth.NewManager(nil, tracking, nil),
	}

	newCfg := &internalconfig.Config{
		Routing: internalconfig.RoutingConfig{
			Strategy: "round-robin",
		},
	}
	commit := configCommit{cfg: newCfg, sequence: 1}
	if !service.applyManagerConfig(context.Background(), commit) {
		t.Fatal("applyManagerConfig failed")
	}

	if !tracking.stopped {
		t.Fatal("expected replaced selector to be stopped during routing config apply")
	}
}

func TestQuotaAwareRoutingHotReloadPreservesPolicyAndCanDisable(t *testing.T) {
	cfg := &internalconfig.Config{Routing: internalconfig.RoutingConfig{SessionAffinity: true, QuotaAwareInitial: true}}
	service := &Service{coreManager: coreauth.NewManager(nil, nil, nil)}
	t.Cleanup(service.coreManager.StopAutoRefresh)
	if !service.applyManagerConfig(context.Background(), configCommit{cfg: cfg, sequence: 1}) {
		t.Fatal("enable failed")
	}
	policy := service.coreManager.InitialQuotaPolicy()
	if policy == nil {
		t.Fatal("policy not enabled")
	}
	cfg2 := *cfg
	cfg2.Routing.Strategy = "weighted-round-robin"
	if !service.applyManagerConfig(context.Background(), configCommit{cfg: &cfg2, sequence: 2}) || service.coreManager.InitialQuotaPolicy() != policy {
		t.Fatal("routing reload replaced policy")
	}
	cfg2.Routing.QuotaAwareInitial = false
	if !service.applyManagerConfig(context.Background(), configCommit{cfg: &cfg2, sequence: 3}) || service.coreManager.InitialQuotaPolicy() != nil {
		t.Fatal("disable failed")
	}
}
