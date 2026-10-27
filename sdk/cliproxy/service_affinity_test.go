package cliproxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func affinityTestService(t *testing.T, cfg *config.Config) *Service {
	t.Helper()
	manager := coreauth.NewManager(nil, coreauth.NewSessionAffinitySelector(&coreauth.FillFirstSelector{}), nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: "account", Provider: "claude"}); err != nil {
		t.Fatal(err)
	}
	s := &Service{cfg: cfg, coreManager: manager}
	t.Cleanup(func() {
		s.stopAffinityPersistence()
		manager.StopAutoRefresh()
	})
	return s
}

func TestServiceAffinityPersistsBeforeSelectorShutdownAndRestores(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.Routing.SessionAffinity = true
	original := affinityTestService(t, cfg)
	original.startAffinityPersistence(context.Background(), cfg, nil)
	opts := cliproxyexecutor.Options{Headers: map[string][]string{"X-Claude-Code-Session-Id": {"resume"}}}
	if _, err := original.coreManager.Selector().Pick(context.Background(), "claude", "model", opts, original.coreManager.List()); err != nil {
		t.Fatal(err)
	}
	prefixOptions := cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FormatOpenAI,
		OriginalRequest: []byte(`{"messages":[{"role":"user","content":"prefix"}]}`),
		Metadata:        map[string]any{cliproxyexecutor.CallerScopeMetadataKey: "caller"},
	}
	if _, err := original.coreManager.Selector().Pick(context.Background(), "claude", "model", prefixOptions, original.coreManager.List()); err != nil {
		t.Fatal(err)
	}
	prefixID, _ := prefixOptions.Metadata[cliproxyexecutor.LCPAffinitySessionIDMetadataKey].(string)
	if prefixID == "" {
		t.Fatal("prefix affinity was not created")
	}
	if err := original.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.AuthDir, ".session-affinity.state")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("missing private final checkpoint: %v", err)
	}
	restarted := affinityTestService(t, cfg)
	restarted.startAffinityPersistence(context.Background(), cfg, nil)
	if auth, status := restarted.coreManager.LookupSessionAffinity("claude", "model", "resume"); auth == nil || auth.ID != "account" || status != "bound" {
		t.Fatalf("service restart lost affinity: %v/%s", auth, status)
	}
	if auth, status := restarted.coreManager.LookupSessionAffinity("claude", "model", prefixID); auth == nil || auth.ID != "account" || status != "bound" {
		t.Fatalf("selector stopped before prefix checkpoint: %v/%s", auth, status)
	}
}

func TestServiceAffinityCorruptionStartsColdAndDirectoryChangeStopsWrites(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.Routing.SessionAffinity = true
	path := filepath.Join(cfg.AuthDir, ".session-affinity.state")
	if err := os.WriteFile(path, []byte("corrupt-state"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := affinityTestService(t, cfg)
	s.startAffinityPersistence(context.Background(), cfg, nil)
	if s.affinityCancel == nil {
		t.Fatal("corruption prevented service checkpointing from starting")
	}
	s.cfgMu.Lock()
	s.cfg = &config.Config{AuthDir: t.TempDir()}
	s.cfgMu.Unlock()
	s.stopAffinityPersistence()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "corrupt-state" {
		t.Fatal("directory change wrote state into former credential directory")
	}
}

func TestServiceAffinityLoadFailureKeepsExistingState(t *testing.T) {
	cfg := &config.Config{AuthDir: t.TempDir()}
	cfg.Routing.SessionAffinity = true
	path := filepath.Join(cfg.AuthDir, ".session-affinity.state")
	previous := []byte(`{"version":1,"bindings":[{"auth_id":"oauth-account","aliases":["session"],"expires_at":"2099-01-01T00:00:00Z"}]}`)
	if err := os.WriteFile(path, previous, 0o600); err != nil {
		t.Fatal(err)
	}
	s := affinityTestService(t, cfg)
	s.startAffinityPersistence(context.Background(), cfg, errors.New("auth store unavailable"))
	if s.affinityCancel != nil || s.affinitySave != nil {
		t.Fatal("checkpoint writer started with incomplete credentials")
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != string(previous) {
		t.Fatal("failed credential load overwrote the saved bindings")
	}
}
