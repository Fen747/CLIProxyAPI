package cliproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var primingTestNow = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)

func primingTestKey(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

func primingTestOpen(t *testing.T, path string) *primingState {
	t.Helper()
	s, err := openPrimingState(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func primingTestReserve(t *testing.T, s *primingState, key string, now time.Time, want bool) {
	t.Helper()
	got, err := s.Reserve(key, now)
	if err != nil || got != want {
		t.Fatalf("Reserve(%s): got %v, %v; want %v", now, got, err, want)
	}
}

func TestPrimingStateAttemptsAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude-window-priming.state")
	key := primingTestKey("subscription")
	s := primingTestOpen(t, path)
	primingTestReserve(t, s, key, primingTestNow, true)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved primingStateFile
	if err := json.Unmarshal(data, &saved); err != nil || len(saved.Records) != 1 || saved.Records[0].Attempts != 1 {
		t.Fatalf("reservation not persisted before return: %s, %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state not private: %v, %v", info, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = primingTestOpen(t, path)
	primingTestReserve(t, s, key, primingTestNow.Add(-time.Second), false)
	primingTestReserve(t, s, key, primingTestNow.Add(primingSpacing-time.Nanosecond), false)
	primingTestReserve(t, s, key, primingTestNow.Add(primingSpacing), true)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = primingTestOpen(t, path)
	primingTestReserve(t, s, key, primingTestNow.Add(primingEpisode-time.Nanosecond), false)
	primingTestReserve(t, s, key, primingTestNow.Add(primingEpisode), true)
}

func TestPrimingStateConfirmNeverShortens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	s := primingTestOpen(t, path)
	key := primingTestKey("subscription")
	for _, reset := range []time.Time{primingTestNow, {}} {
		if err := s.Confirm(key, reset); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.data.Records) != 0 {
		t.Fatal("confirmation without reservation created an entry")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("confirmation without reservation wrote state: %v", err)
	}
	primingTestReserve(t, s, key, primingTestNow, true)
	reset := primingTestNow.Add(7 * time.Hour)
	for _, reset := range []time.Time{reset, primingTestNow.Add(time.Hour)} {
		if err := s.Confirm(key, reset); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = primingTestOpen(t, path)
	primingTestReserve(t, s, key, primingTestNow.Add(primingSpacing), false)
	primingTestReserve(t, s, key, reset.Add(-time.Nanosecond), false)
	primingTestReserve(t, s, key, reset, true)
	if err := s.Confirm(key, reset.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	primingTestReserve(t, s, key, reset.Add(primingEpisode-time.Nanosecond), false)
}

func TestPrimingStateSpacingAcrossEpisodes(t *testing.T) {
	s := primingTestOpen(t, filepath.Join(t.TempDir(), "state"))
	key := primingTestKey("subscription")
	primingTestReserve(t, s, key, primingTestNow, true)
	second := primingTestNow.Add(primingEpisode - time.Minute)
	primingTestReserve(t, s, key, second, true)
	primingTestReserve(t, s, key, primingTestNow.Add(primingEpisode), false)
	primingTestReserve(t, s, key, second.Add(primingSpacing), true)
}

func TestPrimingStateRejectsUnsafeFiles(t *testing.T) {
	valid := primingStateFile{Version: 1, HighWater: primingTestNow, Records: []primingRecord{{Key: primingTestKey("a"), First: primingTestNow, Last: primingTestNow, Attempts: 1}}}
	for _, name := range []string{"empty", "malformed", "public", "symlink", "directory", "oversize", "version", "missing-version", "attempts", "key", "duplicate", "timestamp", "unknown", "trailing"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state")
			envelope := valid
			envelope.Records = append([]primingRecord(nil), valid.Records...)
			switch name {
			case "version":
				envelope.Version = 2
			case "attempts":
				envelope.Records[0].Attempts = 3
			case "key":
				envelope.Records[0].Key = "raw-subscription"
			case "duplicate":
				envelope.Records = append(envelope.Records, envelope.Records[0])
			case "timestamp":
				envelope.Records[0].Last = primingTestNow.Add(time.Minute)
			}
			data, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "empty":
				data = nil
			case "malformed":
				data = []byte("{")
			case "oversize":
				data = []byte(strings.Repeat(" ", primingMaxBytes+1))
			case "missing-version":
				data = []byte(strings.Replace(string(data), `"version":1,`, "", 1))
			case "unknown":
				data = append([]byte(`{"other":1,`), data[1:]...)
			case "trailing":
				data = append(data, []byte(" {}")...)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "public":
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(path, path+"-link"); err != nil {
					t.Fatal(err)
				}
				path += "-link"
			case "directory":
				path = t.TempDir()
			}
			if s, err := openPrimingState(path); err == nil {
				_ = s.Close()
				t.Fatal("accepted unsafe state")
			}
			if _, err := os.Lstat(path + ".lock"); !os.IsNotExist(err) {
				t.Fatalf("failed open leaked owner: %v", err)
			}
		})
	}
}

func TestPrimingStateConcurrentReservationsAndOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	s := primingTestOpen(t, path)
	if other, err := openPrimingState(path); err == nil {
		_ = other.Close()
		t.Fatal("second owner accepted")
	}
	key := primingTestKey("subscription")
	for _, now := range []time.Time{primingTestNow, primingTestNow.Add(primingSpacing)} {
		var successes atomic.Int32
		var wg sync.WaitGroup
		for range 32 {
			wg.Go(func() {
				ok, err := s.Reserve(key, now)
				if err != nil {
					t.Error(err)
				}
				if ok {
					successes.Add(1)
				}
			})
		}
		wg.Wait()
		if successes.Load() != 1 {
			t.Fatalf("got %d concurrent reservations", successes.Load())
		}
	}
	primingTestReserve(t, s, primingTestKey("other"), primingTestNow, false)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Reserve(key, primingTestNow); ok || err == nil {
		t.Fatal("closed store allowed reservation")
	}
	if err := os.WriteFile(path+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if other, err := openPrimingState(path); err == nil {
		_ = other.Close()
		t.Fatal("stale owner did not fail closed")
	}
}

func TestPrimingStateLimitsAndWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	s := primingTestOpen(t, path)
	if ok, err := s.Reserve("raw", primingTestNow); ok || err == nil {
		t.Fatal("invalid key accepted")
	}
	if ok, err := s.Reserve(primingTestKey("a"), time.Time{}); ok || err == nil {
		t.Fatal("invalid time accepted")
	}
	for i := range primingMaxRecords {
		s.data.Records = append(s.data.Records, primingRecord{Key: primingTestKey(string(rune(i))), First: primingTestNow, Last: primingTestNow, Attempts: 1})
	}
	s.data.HighWater = primingTestNow
	if ok, err := s.Reserve(primingTestKey("new"), primingTestNow); ok || err == nil {
		t.Fatal("record bound exceeded")
	}
	primingTestReserve(t, s, primingTestKey("new"), primingTestNow.Add(primingEpisode), true)
	if len(s.data.Records) != 1 {
		t.Fatal("expired records not pruned")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Reserve(primingTestKey("failure"), primingTestNow.Add(primingEpisode)); ok || err == nil {
		t.Fatal("failed write allowed inference")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Reserve(primingTestKey("retry"), primingTestNow.Add(primingEpisode)); ok || err == nil {
		t.Fatal("failed store allowed inference")
	}
}
