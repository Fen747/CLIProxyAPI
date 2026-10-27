package cliproxy

import (
	"context"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

// startAffinityPersistence runs after credentials load and before admission.
// The state file uses a non-JSON suffix so credential watchers ignore it.
func (s *Service) startAffinityPersistence(ctx context.Context, cfg *config.Config) {
	if s.coreManager == nil || cfg == nil || cfg.Home.Enabled || cfg.AuthDir == "" {
		return
	}
	s.affinityMu.Lock()
	defer s.affinityMu.Unlock()
	if s.affinityCancel != nil {
		return
	}
	path := filepath.Join(cfg.AuthDir, ".session-affinity.state")
	if cfg.Routing.SessionAffinity {
		if errRestore := s.coreManager.RestoreSessionAffinity(path); errRestore != nil {
			log.WithError(errRestore).Warn("session affinity state could not be restored; starting cold")
		}
	}
	save := func() error {
		current := s.currentConfig()
		// A directory/Home change needs a restart; never write new identities to
		// the former credential directory during a config transition.
		if current == nil || current.Home.Enabled || filepath.Join(current.AuthDir, ".session-affinity.state") != path {
			return nil
		}
		return s.coreManager.SaveSessionAffinity(path)
	}
	ctx, cancel := context.WithCancel(ctx)
	s.affinityCancel, s.affinityDone, s.affinitySave = cancel, make(chan struct{}), save
	done := s.affinityDone
	go func() {
		defer close(done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if errSave := save(); errSave != nil {
					log.WithError(errSave).Warn("session affinity checkpoint failed")
				}
			}
		}
	}()
}

func (s *Service) stopAffinityPersistence() {
	s.affinityMu.Lock()
	cancel, done, save := s.affinityCancel, s.affinityDone, s.affinitySave
	s.affinityCancel, s.affinityDone, s.affinitySave = nil, nil, nil
	s.affinityMu.Unlock()
	if cancel != nil {
		cancel()
		<-done
		if errSave := save(); errSave != nil {
			log.WithError(errSave).Warn("final session affinity checkpoint failed")
		}
	}
}
