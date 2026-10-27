package maintenance

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestExistingControlAndDetachedWorkCannotRaceReadyState(t *testing.T) {
	g := New()
	_, _ = g.Drain("owner")
	var detached atomic.Int32
	detached.Store(1)
	checked, proceed := make(chan struct{}), make(chan struct{})
	ticket := make(chan func(), 1)
	go func() {
		done, err := g.BeginExisting(func() bool {
			exists := detached.Load() > 0
			close(checked)
			<-proceed
			return exists
		})
		if err != nil {
			t.Error(err)
		}
		ticket <- done
	}()
	<-checked
	detached.Store(0)
	snapshot := make(chan State, 1)
	go func() { snapshot <- g.StateWithActive(func() int { return int(detached.Load()) }) }()
	close(proceed)
	done := <-ticket
	state := <-snapshot
	if state.Ready || state.Active != 1 {
		t.Fatalf("ready raced admitted control: %+v", state)
	}
	done()
	if !g.StateWithActive(func() int { return int(detached.Load()) }).Ready {
		t.Fatal("completed control still blocks maintenance")
	}
}

func TestPersistentDrainSurvivesRestartUntilSameOwnerResumes(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".maintenance.state")
	g, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 32)
	if _, err := g.Drain(token); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if state := restarted.State(); !state.Ready || !state.Durable || state.Instance == g.State().Instance {
		t.Fatalf("restart = %+v", state)
	}
	if _, err := restarted.Begin(); err != ErrDraining {
		t.Fatal("restart reopened admission")
	}
	if _, err := restarted.Resume(token); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("marker remains: %v", err)
	}
	open, err := Open(path)
	if err != nil || open.State().Draining {
		t.Fatalf("resumed restart = %+v, %v", open, err)
	}
}

func TestPersistentDrainRejectsInvalidPublicAndSymlinkMarkers(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o644} {
		path := filepath.Join(t.TempDir(), "marker")
		body := strings.Repeat("a", 32)
		if mode == 0o600 {
			body = strings.Repeat("!", 32)
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(path); err == nil {
			t.Fatal("invalid/public marker accepted")
		}
	}
	path := filepath.Join(t.TempDir(), "symlink")
	if err := os.Symlink("missing", path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("symlink marker accepted")
	}
}

func TestDrainSerializesConcurrentAdmissionAndRelease(t *testing.T) {
	g := New()
	const workers = 100
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			<-start
			done, err := g.Begin()
			if err == nil {
				done()
				done()
			}
		})
	}
	close(start)
	if _, err := g.Drain("owner"); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if state := g.State(); !state.Ready || state.Active != 0 {
		t.Fatalf("state = %+v", state)
	}
	if _, err := g.Begin(); err != ErrDraining {
		t.Fatalf("admission error = %v", err)
	}
}

func TestDrainRetainsExistingWorkAndOnlyOwnerCanResume(t *testing.T) {
	g := New()
	done, _ := g.Begin()
	state, _ := g.Drain("owner")
	if state.Ready || state.Active != 1 {
		t.Fatalf("state = %+v", state)
	}
	if _, err := g.Resume("other"); err != ErrOwner {
		t.Fatalf("resume = %v", err)
	}
	if _, err := g.Drain("other"); err != ErrOwner {
		t.Fatalf("drain = %v", err)
	}
	done()
	if state, err := g.Drain("owner"); err != nil || !state.Ready {
		t.Fatalf("drained state = %+v, %v", state, err)
	}
	if _, err := g.Resume("owner"); err != nil {
		t.Fatal(err)
	}
	done, err := g.Begin()
	if err != nil {
		t.Fatal(err)
	}
	done()
}
