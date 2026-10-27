package session

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMerklePrefixStateRestoresForkAndCompactionLineage(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		turns []CanonicalTurn
	}{
		{"fork", turnsFromTexts("one", "two", "different", "four")},
		{"compaction", turnsFromTexts("summary", "four", "five", "six")},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			now := time.Now()
			cfg := MerklePrefixMatcherConfig{TTL: time.Hour, NowFunc: func() time.Time { return now }}
			original := NewMerklePrefixMatcherWithConfig(cfg)
			original.Bind("namespace", turnsFromTexts("one", "two", "three", "four", "five"), "account")
			original.BindWithResult("namespace", scenario.turns, "account")
			want, _ := original.Match("namespace", scenario.turns)
			data, err := json.Marshal(original.Snapshot())
			if err != nil {
				t.Fatal(err)
			}
			var saved []MerklePrefixState
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			restored := NewMerklePrefixMatcherWithConfig(cfg)
			restored.Restore(saved, func(id string) bool { return id == "account" })
			gotData, _ := json.Marshal(restored.Snapshot())
			if string(gotData) != string(data) {
				t.Fatal("restoration changed lineage, expiration or eviction order")
			}
			match, ok := restored.Match("namespace", scenario.turns)
			if !ok || match.AuthID != "account" || match.SessionID != want.SessionID ||
				match.ParentSessionID != want.ParentSessionID || match.IsFork != want.IsFork ||
				match.IsCompaction != want.IsCompaction || match.NodeKind != want.NodeKind {
				t.Fatalf("restored lineage = %+v; want %+v", match, want)
			}
			if match.AccessNumber <= uint64(len(saved)) {
				t.Fatal("access generation did not advance beyond restored groups")
			}
		})
	}
}

func TestMerklePrefixStateFiltersInvalidGroupsAndRebuildsBounds(t *testing.T) {
	now := time.Now()
	original := NewMerklePrefixMatcher(time.Hour)
	original.Bind("oldest", turnsFromTexts("first"), "account")
	original.Bind("newest", turnsFromTexts("second"), "account")
	saved := original.Snapshot()
	expired := saved[0]
	expired.ExpiresAt = now.Add(-time.Second)
	unknown := saved[0]
	unknown.AuthID = "removed"
	malformed := saved[0]
	malformed.Fingerprints = []string{"raw text must never be imported"}
	groups := append(saved, expired, unknown, malformed)
	restored := NewMerklePrefixMatcherWithConfig(MerklePrefixMatcherConfig{
		TTL: time.Hour, MaxGroups: 1, NowFunc: func() time.Time { return now },
	})
	restored.Restore(groups, func(id string) bool { return id == "account" })
	got := restored.Snapshot()
	if len(got) != 1 || got[0].Namespace != "newest" {
		t.Fatalf("invalid import or eviction order: %+v", got)
	}
	now = got[0].ExpiresAt
	if _, ok := restored.Match("newest", turnsFromTexts("second")); ok {
		t.Fatal("restore extended the original expiration")
	}
}
