package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
)

const maxAffinityStateBytes = 64 << 20

type sessionAffinityStateFile struct {
	Version  int                                 `json:"version"`
	Bindings []sessionBindingState               `json:"bindings"`
	Prefixes []cliproxysession.MerklePrefixState `json:"prefixes"`
}

// shareAffinityFrom keeps in-flight results attached to the same caches after a
// routing change. Ownership is retained before the replaced selector is stopped.
func (s *SessionAffinitySelector) shareAffinityFrom(old *SessionAffinitySelector) {
	if old.affinityRefs == nil || old.cache == nil || old.matcher == nil {
		return
	}
	for count := old.affinityRefs.Load(); count > 0; count = old.affinityRefs.Load() {
		if !old.affinityRefs.CompareAndSwap(count, count+1) {
			continue
		}
		ttl := s.cache.ttl
		s.cache.Stop()
		s.matcher.Clear()
		s.cache, s.matcher, s.affinityRefs = old.cache, old.matcher, old.affinityRefs
		s.cache.mu.Lock()
		s.cache.ttl = ttl
		s.cache.mu.Unlock()
		s.matcher.SetTTL(ttl)
		return
	}
}

// SaveSessionAffinity checkpoints opaque bindings independently of OAuth tokens.
// Non-affinity selectors write empty state so disabling affinity cannot revive
// stale bindings on the next restart. Callers serialize admission/draining.
func (m *Manager) SaveSessionAffinity(path string) error {
	m.affinityPersistMu.Lock()
	defer m.affinityPersistMu.Unlock()
	state := sessionAffinityStateFile{Version: 1}
	if selector, ok := m.Selector().(*SessionAffinitySelector); ok {
		state.Bindings = selector.cache.snapshot()
		state.Prefixes = selector.matcher.Snapshot()
	}
	data, errMarshal := json.Marshal(state)
	if errMarshal != nil || len(data) > maxAffinityStateBytes {
		return errors.New("session affinity snapshot exceeds format limits")
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("session affinity state must be a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, errCreate := os.CreateTemp(filepath.Dir(path), ".affinity-*.tmp")
	if errCreate != nil {
		return fmt.Errorf("create session affinity snapshot: %w", errCreate)
	}
	defer func() { _ = os.Remove(file.Name()) }()
	if _, errWrite := file.Write(data); errWrite != nil {
		_ = file.Close()
		return fmt.Errorf("write session affinity snapshot: %w", errWrite)
	}
	if errSync := file.Sync(); errSync != nil {
		_ = file.Close()
		return fmt.Errorf("sync session affinity snapshot: %w", errSync)
	}
	if errClose := file.Close(); errClose != nil {
		return fmt.Errorf("close session affinity snapshot: %w", errClose)
	}
	return os.Rename(file.Name(), path)
}

// RestoreSessionAffinity must run after loading credentials and before serving.
// Invalid files leave runtime state untouched; callers may warn and start cold.
func (m *Manager) RestoreSessionAffinity(path string) error {
	m.affinityPersistMu.Lock()
	defer m.affinityPersistMu.Unlock()
	info, errStat := os.Lstat(path)
	if errors.Is(errStat, os.ErrNotExist) {
		return nil
	}
	if errStat != nil {
		return errStat
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maxAffinityStateBytes {
		return errors.New("session affinity state must be private, regular and within size limits")
	}
	file, errOpen := os.Open(path)
	if errOpen != nil {
		return errOpen
	}
	data, errRead := io.ReadAll(io.LimitReader(file, maxAffinityStateBytes+1))
	errClose := file.Close()
	if errRead != nil || errClose != nil || len(data) > maxAffinityStateBytes {
		return errors.New("failed to read session affinity state")
	}
	var state sessionAffinityStateFile
	if json.Unmarshal(data, &state) != nil || state.Version != 1 ||
		len(state.Bindings) > defaultMaxSessionEntries || len(state.Prefixes) > 4096 {
		return errors.New("invalid session affinity state format")
	}
	m.selectorMu.Lock()
	defer m.selectorMu.Unlock()
	m.mu.RLock()
	selector, ok := m.selector.(*SessionAffinitySelector)
	allowed := make(map[string]bool, len(m.auths))
	for id, auth := range m.auths {
		allowed[id] = auth != nil && !auth.Disabled
	}
	m.mu.RUnlock()
	if ok {
		acceptAuth := func(id string) bool { return allowed[id] }
		selector.cache.restore(state.Bindings, acceptAuth)
		selector.matcher.Restore(state.Prefixes, acceptAuth)
	}
	return nil
}
