package auth

import (
	"context"
	"errors"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

const ClaudeWindowPrimingModel = "claude-haiku-4-5-20251001"

// PrimeClaudeWindow makes exactly one executor invocation for an exact credential
// version. It bypasses account selection, affinity and manager retry/failover loops.
// The caller must hold background admission and persist its attempt beforehand.
func (m *Manager) PrimeClaudeWindow(ctx context.Context, expected *Auth) error {
	if expected == nil {
		return errors.New("Claude priming credential is nil")
	}
	current, ok := m.GetByID(expected.ID)
	if !ok || quotaIdentityOf(current) != quotaIdentityOf(expected) || !ClaudeQuotaEligible(current) {
		return errors.New("Claude priming credential changed")
	}
	if _, err := getSelectorAvailableAuths(ctx, []*Auth{current}, "claude", ClaudeWindowPrimingModel, time.Now()); err != nil {
		return err
	}
	executor := m.executorFor("claude")
	if executor == nil {
		return errors.New("Claude executor unavailable")
	}
	if transport := m.roundTripperFor(current); transport != nil {
		ctx = context.WithValue(ctx, roundTripperContextKey{}, transport)
		ctx = context.WithValue(ctx, "cliproxy.roundtripper", transport)
	}
	body := []byte(`{"model":"claude-haiku-4-5-20251001","max_tokens":1,"messages":[{"role":"user","content":"Reply with a dot."}]}`)
	req := cliproxyexecutor.Request{Model: ClaudeWindowPrimingModel, Payload: body, Format: sdktranslator.FormatClaude}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: body, Metadata: map[string]any{cliproxyexecutor.PinnedAuthMetadataKey: current.ID}}
	_, err := executor.Execute(ctx, current, req, opts)
	return err
}
