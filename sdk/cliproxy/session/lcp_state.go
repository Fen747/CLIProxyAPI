package session

import (
	"container/list"
	"encoding/hex"
	"time"
)

// MerklePrefixState contains only affinity metadata and fingerprints, never turns.
// Groups are ordered from least to most recently accessed; indexes and access
// generations are rebuilt on restore rather than trusting serialized counters.
type MerklePrefixState struct {
	Namespace         string    `json:"namespace"`
	AuthID            string    `json:"auth_id"`
	SessionID         string    `json:"session_id"`
	ParentSessionID   string    `json:"parent_session_id,omitempty"`
	MinPrefixLength   int       `json:"min_prefix_length"`
	IsFork            bool      `json:"is_fork,omitempty"`
	IsCompaction      bool      `json:"is_compaction,omitempty"`
	NodeKind          string    `json:"node_kind,omitempty"`
	EnvironmentDigest string    `json:"environment_digest,omitempty"`
	Fingerprints      []string  `json:"fingerprints"`
	TailFingerprints  []string  `json:"tail_fingerprints,omitempty"`
	ExpiresAt         time.Time `json:"expires_at"`
}

// Snapshot copies live groups in eviction order without refreshing their TTLs.
func (m *MerklePrefixMatcher) Snapshot() []MerklePrefixState {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []MerklePrefixState
	now := m.now()
	for element := m.lru.Front(); element != nil; element = element.Next() {
		g := element.Value.(*lcpGroup)
		if !now.Before(g.expiresAt) {
			continue
		}
		result = append(result, MerklePrefixState{
			Namespace: g.namespace, AuthID: g.authID, SessionID: g.sessionID,
			ParentSessionID: g.parentSessionID, MinPrefixLength: g.minPrefixLength,
			IsFork: g.isFork, IsCompaction: g.isCompaction, NodeKind: g.nodeKind,
			EnvironmentDigest: g.environmentDigest,
			Fingerprints:      append([]string(nil), g.fingerprints...),
			TailFingerprints:  append([]string(nil), g.tailFingerprints...), ExpiresAt: g.expiresAt,
		})
	}
	return result
}

// Restore replaces groups, discarding expired, malformed or unknown credentials.
// It keeps absolute expirations and rebuilds the bounded prefix/tail/LRU indexes.
func (m *MerklePrefixMatcher) Restore(groups []MerklePrefixState, acceptAuth func(string) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.groups = make(map[string]*lcpNamespace)
	m.lru = list.New()
	m.lruElements = make(map[*lcpGroup]*list.Element)
	m.groupCount, m.prefixCount = 0, 0
	now := m.now()
	for _, g := range groups {
		if g.Namespace == "" || len(g.Namespace) > 8192 || g.AuthID == "" ||
			g.SessionID == "" || len(g.SessionID) > 8192 || len(g.ParentSessionID) > 8192 ||
			!now.Before(g.ExpiresAt) || !acceptAuth(g.AuthID) ||
			len(g.Fingerprints) > m.maxTurns || len(g.TailFingerprints) > maxCompactionProbeWindow ||
			g.MinPrefixLength <= 0 || g.MinPrefixLength > len(g.Fingerprints) ||
			!validFingerprints(g.Fingerprints) || !validFingerprints(g.TailFingerprints) ||
			(g.EnvironmentDigest != "" && !validFingerprints([]string{g.EnvironmentDigest})) {
			continue
		}
		key := sequenceKey(g.Fingerprints)
		if existing := m.namespaceLocked(g.Namespace).groups[key]; existing != nil {
			m.removeGroupLocked(existing)
		}
		m.addGroupLocked(&lcpGroup{
			key: key, namespace: g.Namespace, authID: g.AuthID, sessionID: g.SessionID,
			parentSessionID: g.ParentSessionID, minPrefixLength: g.MinPrefixLength,
			isFork: g.IsFork, isCompaction: g.IsCompaction, nodeKind: g.NodeKind,
			environmentDigest: g.EnvironmentDigest,
			fingerprints:      append([]string(nil), g.Fingerprints...),
			tailFingerprints:  append([]string(nil), g.TailFingerprints...), expiresAt: g.ExpiresAt,
		})
	}
}

func validFingerprints(values []string) bool {
	for _, value := range values {
		if len(value) != 64 {
			return false
		}
		if _, err := hex.DecodeString(value); err != nil {
			return false
		}
	}
	return true
}

// SetTTL changes future refresh durations without extending existing expirations.
func (m *MerklePrefixMatcher) SetTTL(ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ttl = ttl
}
