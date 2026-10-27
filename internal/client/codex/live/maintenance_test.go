package live

import "testing"

func TestActiveSessionsIncludesFinalResourceCleanup(t *testing.T) {
	handler := &Handler{sessions: newSessionStore()}
	started, finish, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	resources := &liveSessionResources{}
	resources.add(func() error {
		close(started)
		<-finish
		return nil
	})
	session := handler.sessions.put("call-maintenance", liveSession{resources: resources})
	go func() {
		defer close(done)
		handler.sessions.complete(session, "test")
	}()
	<-started
	if got := handler.ActiveSessions(); got != 1 {
		t.Errorf("active during final cleanup = %d", got)
	}
	close(finish)
	<-done
	if got := handler.ActiveSessions(); got != 0 {
		t.Fatalf("active after cleanup = %d", got)
	}
}
