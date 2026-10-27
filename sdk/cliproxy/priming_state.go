package cliproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	primingEpisode    = 5 * time.Hour
	primingSpacing    = 5 * time.Minute
	primingMaxRecords = 1024
	primingMaxBytes   = 1 << 20
)

type primingRecord struct {
	Key      string    `json:"key"`
	First    time.Time `json:"first_attempt"`
	Last     time.Time `json:"last_attempt"`
	Attempts int       `json:"attempts"`
	Until    time.Time `json:"suppress_until,omitempty"`
}

type primingStateFile struct {
	Version   int             `json:"version"`
	HighWater time.Time       `json:"high_water"`
	Records   []primingRecord `json:"records"`
}

// primingState owns path.lock until Close. A crash leaves a fail-closed marker;
// operators may remove it only after ensuring no process still owns this store.
// Keys must be caller-generated lowercase SHA-256 subscription digests.
type primingState struct {
	mu     sync.Mutex
	path   string
	lock   *os.File
	data   primingStateFile
	failed error
}

func openPrimingState(path string) (*primingState, error) {
	if path == "" || filepath.Clean(path) == "." {
		return nil, errors.New("invalid priming state path")
	}
	lock, err := os.OpenFile(path+".lock", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("acquire priming state owner (stale locks require operator recovery): %w", err)
	}
	s := &primingState{path: path, lock: lock, data: primingStateFile{Version: 1}}
	if err := s.load(); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	return s, nil
}

func validPrimingKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for _, c := range key {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validPrimingTime(t time.Time) bool { return !t.IsZero() && t.Year() >= 1 && t.Year() < 9999 }

func (s *primingState) load() error {
	info, err := os.Lstat(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() <= 0 || info.Size() > primingMaxBytes {
		return errors.New("invalid private priming state file")
	}
	f, err := os.Open(s.path)
	if err != nil {
		return err
	}
	opened, errStat := f.Stat()
	if errStat != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0o077 != 0 || opened.Size() > primingMaxBytes {
		return errors.Join(errors.New("priming state changed while opening"), errStat, f.Close())
	}
	data, errRead := io.ReadAll(io.LimitReader(f, primingMaxBytes+1))
	if err := errors.Join(errRead, f.Close()); err != nil {
		return err
	}
	if len(data) > primingMaxBytes {
		return errors.New("priming state exceeds size limit")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	s.data = primingStateFile{}
	if err := dec.Decode(&s.data); err != nil {
		return fmt.Errorf("decode priming state: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("trailing priming state data")
	}
	if s.data.Version != 1 || len(s.data.Records) > primingMaxRecords || !validPrimingTime(s.data.HighWater) {
		return errors.New("invalid priming state envelope")
	}
	seen := make(map[string]bool, len(s.data.Records))
	for _, r := range s.data.Records {
		if !validPrimingKey(r.Key) || seen[r.Key] || !validPrimingTime(r.First) || !validPrimingTime(r.First.Add(primingEpisode)) || !validPrimingTime(r.Last) || r.Last.Before(r.First) || !r.Last.Before(r.First.Add(primingEpisode)) || s.data.HighWater.Before(r.Last) ||
			(r.Attempts != 1 && r.Attempts != 2) || (r.Attempts == 1 && !r.Last.Equal(r.First)) || (r.Attempts == 2 && r.Last.Before(r.First.Add(primingSpacing))) ||
			(!r.Until.IsZero() && (!validPrimingTime(r.Until) || r.Until.Before(r.First.Add(primingEpisode)))) {
			return errors.New("invalid priming state record")
		}
		seen[r.Key] = true
	}
	return nil
}

// Reserve durably consumes an attempt before the caller may perform inference.
// A backward clock blocks all reservations until the last persisted time returns.
func (s *primingState) Reserve(key string, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(key); err != nil {
		return false, err
	}
	if !validPrimingTime(now) || !validPrimingTime(now.Add(primingEpisode)) {
		return false, errors.New("invalid priming reservation time")
	}
	if now.Before(s.data.HighWater) {
		return false, nil
	}
	kept := make([]primingRecord, 0, len(s.data.Records)+1)
	var current *primingRecord
	for _, r := range s.data.Records {
		if !now.Before(r.First.Add(primingEpisode)) && !now.Before(r.Last.Add(primingSpacing)) && !now.Before(r.Until) {
			continue
		}
		kept = append(kept, r)
		if r.Key == key {
			current = &kept[len(kept)-1]
		}
	}
	if current != nil {
		if current.Attempts >= 2 || now.Before(current.Until) || now.Before(current.Last.Add(primingSpacing)) {
			return false, nil
		}
		current.Attempts++
		current.Last = now.UTC()
	} else {
		if len(kept) >= primingMaxRecords {
			return false, errors.New("priming state record limit reached")
		}
		kept = append(kept, primingRecord{Key: key, First: now.UTC(), Last: now.UTC(), Attempts: 1})
	}
	s.data.Records = kept
	s.data.HighWater = now.UTC()
	if err := s.save(); err != nil {
		return false, err
	}
	return true, nil
}

// Confirm never shortens a reserved key's episode or reset guard.
// Active-window observations for keys never reserved are a no-op.
func (s *primingState) Confirm(key string, reset time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(key); err != nil {
		return err
	}
	for i := range s.data.Records {
		r := &s.data.Records[i]
		if r.Key != key {
			continue
		}
		if !validPrimingTime(reset) {
			return errors.New("invalid priming reset time")
		}
		until := r.First.Add(primingEpisode)
		if reset.After(until) {
			until = reset.UTC()
		}
		if !until.After(r.Until) {
			return nil
		}
		r.Until = until
		return s.save()
	}
	return nil
}

func (s *primingState) ready(key string) error {
	if s.lock == nil {
		return errors.New("priming state closed")
	}
	if s.failed != nil {
		return s.failed
	}
	if !validPrimingKey(key) {
		return errors.New("priming key must be a lowercase SHA-256 digest")
	}
	return nil
}

func (s *primingState) save() (err error) {
	defer func() { s.failed = err }()
	data, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	if len(data) > primingMaxBytes {
		return errors.New("priming state exceeds size limit")
	}
	// Refuse replacement of a file that became public, a symlink or nonregular.
	if info, err := os.Lstat(s.path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return errors.New("invalid private priming state file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".claude-window-priming-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, errWrite := f.Write(data)
	errSync := f.Sync()
	if err := errors.Join(errWrite, errSync, f.Close()); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(s.path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func (s *primingState) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lock == nil {
		return nil
	}
	err := s.lock.Close()
	s.lock = nil
	return errors.Join(err, os.Remove(s.path+".lock"))
}
