package cliproxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
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
	original.startAffinityPersistence(context.Background(), cfg)
	opts := cliproxyexecutor.Options{Headers: map[string][]string{"X-Claude-Code-Session-Id": {"resume"}}}
	if _, err := original.coreManager.Selector().Pick(context.Background(), "claude", "model", opts, original.coreManager.List()); err != nil {
		t.Fatal(err)
	}
	if err := original.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.AuthDir, ".session-affinity.state")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("missing private final checkpoint: %v", err)
	}
	restarted := affinityTestService(t, cfg)
	restarted.startAffinityPersistence(context.Background(), cfg)
	if auth, status := restarted.coreManager.LookupSessionAffinity("claude", "model", "resume"); auth == nil || auth.ID != "account" || status != "bound" {
		t.Fatalf("service restart lost affinity: %v/%s", auth, status)
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
	s.startAffinityPersistence(context.Background(), cfg)
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
