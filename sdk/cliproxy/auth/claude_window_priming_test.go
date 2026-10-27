package auth

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestWindowPrimingRequiresExplicitInactiveFreshWindowAndWeeklyMargin(t *testing.T) {
	for _, tc := range []struct {
		name, window, weekly string
		age                  time.Duration
		want                 bool
	}{
		{name: "inactive", window: "null", weekly: "null", want: true},
		{name: "zero active", window: `{"utilization":0,"resets_at":"2026-10-27T17:00:00Z"}`, weekly: "null"},
		{name: "zero no reset", window: `{"utilization":0,"resets_at":null}`, weekly: "null"},
		{name: "exhausted weekly", window: "null", weekly: `{"utilization":99,"resets_at":"2026-11-01T17:00:00Z"}`},
		{name: "expired weekly", window: "null", weekly: `{"utilization":10,"resets_at":"2026-10-26T17:00:00Z"}`},
		{name: "stale", window: "null", weekly: "null", age: 2 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, now, auths := quotaTestPolicy(t)
			body := []byte(fmt.Sprintf(`{"five_hour":%s,"seven_day":%s}`, tc.window, tc.weekly))
			if err := p.Observe(auths[0], body, now.Add(-tc.age)); err != nil {
				t.Fatal(err)
			}
			got, _ := p.WindowPrimingDecision(auths[0], now)
			if got != tc.want {
				t.Fatalf("prime=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestPrimeClaudeWindowInvokesOnlyPinnedCredentialOnceAndKeepsAffinity(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{QuotaAwareInitial: true})
	defer selector.Stop()
	manager := NewManager(nil, selector, nil)
	executor := &retryRoundCallExecutor{identifier: "claude"}
	manager.RegisterExecutor(executor)
	manager.SetRetryConfig(3, 0, 0)
	for _, id := range []string{"a", "b"} {
		if _, err := manager.Register(WithSkipPersist(context.Background()), quotaTestAuth(id)); err != nil {
			t.Fatal(err)
		}
	}
	a, _ := manager.GetByID("a")
	selector.cache.Set("kept", "b")
	if err := manager.PrimeClaudeWindow(context.Background(), a); err == nil {
		t.Fatal("expected executor error")
	}
	if ids := executor.ids("execute"); fmt.Sprint(ids) != "[a]" {
		t.Fatalf("unexpected retries/failover: %v", ids)
	}
	if binding, _ := selector.cache.Get("kept"); binding != "b" {
		t.Fatal("priming changed user affinity")
	}
	a.CredentialVersion++
	if err := manager.PrimeClaudeWindow(context.Background(), a); err == nil {
		t.Fatal("accepted obsolete credential")
	}
	if ids := executor.ids("execute"); len(ids) != 1 {
		t.Fatal("stale credential executed")
	}
}

func TestWindowPrimingRespectsNewerPassiveActiveWindow(t *testing.T) {
	p, now, auths := quotaTestPolicy(t)
	a := auths[0]
	if err := p.Observe(a, []byte(`{"five_hour":null,"seven_day":null}`), now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	a.Quota.ObserveResponseHeadersForProvider("claude", http.Header{
		"Anthropic-Ratelimit-Unified-5h-Utilization": {"0"},
		"Anthropic-Ratelimit-Unified-5h-Reset":       {fmt.Sprint(now.Add(5 * time.Hour).Unix())},
	}, now)
	canPrime, reset := p.WindowPrimingDecision(a, now)
	if canPrime || !reset.Equal(now.Add(5*time.Hour)) {
		t.Fatal("newer active window ignored")
	}
}

type primingIdentityExecutor struct {
	retryRoundCallExecutor
	prepares int
}

func (e *primingIdentityExecutor) ShouldPrepareRequestAuth(a *Auth) bool {
	return authMetadataString(a, "account_uuid") == ""
}
func (e *primingIdentityExecutor) PrepareRequestAuth(_ context.Context, a *Auth) (*Auth, error) {
	e.prepares++
	a = a.Clone()
	a.Metadata["account_uuid"] = "prepared-subscription"
	return a, nil
}

func TestPrimeClaudeWindowUsesSerializedIdentityPreparation(t *testing.T) {
	manager := NewManager(nil, nil, nil)
	executor := &primingIdentityExecutor{retryRoundCallExecutor: retryRoundCallExecutor{identifier: "claude"}}
	manager.RegisterExecutor(executor)
	_, err := manager.Register(WithSkipPersist(context.Background()), quotaTestAuth("imported"))
	if err != nil {
		t.Fatal(err)
	}
	a, _ := manager.GetByID("imported")
	prepared, err := manager.PrepareClaudeWindowPrimingAuth(context.Background(), a)
	if err != nil || authMetadataString(prepared, "account_uuid") != "prepared-subscription" || executor.prepares != 1 {
		t.Fatalf("missing imported identity: %v/%v", prepared, err)
	}
	if err := manager.PrimeClaudeWindow(context.Background(), prepared); err == nil {
		t.Fatal("expected test executor failure")
	}
	if ids := executor.ids("execute"); fmt.Sprint(ids) != "[imported]" || executor.prepares != 1 {
		t.Fatalf("preparation/retry mismatch: %v/%d", ids, executor.prepares)
	}
}
