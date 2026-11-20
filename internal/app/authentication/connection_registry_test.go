package authentication

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestRegistryCloseRejectsRegistrationDuringShutdown(t *testing.T) {
	registry := NewConnectionRegistry()
	closing := make(chan struct{})
	continueClose := make(chan struct{})
	registry.Register("existing", "first", func() {
		close(closing)
		<-continueClose
	})
	closed := make(chan struct{})
	go func() {
		registry.Close()
		close(closed)
	}()
	select {
	case <-closing:
	case <-time.After(time.Second):
		t.Fatal("existing connection was not closed")
	}
	var rejected atomic.Int32
	unregister := registry.Register("new", "second", func() { rejected.Add(1) })
	unregister()
	if rejected.Load() != 1 || len(registry.SessionIDs()) != 0 {
		t.Fatal("new connection survived registry shutdown")
	}
	close(continueClose)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("registry did not finish shutdown")
	}
}

func TestRegistryCloseStopsPresenceHeartbeat(t *testing.T) {
	registry := newCoordinatedConnectionRegistry(context.Background(), &lifecycleSessionStore{}, "node", time.Second)
	registry.Register("session", "connection", func() {})
	registry.mu.Lock()
	heartbeat := registry.presence["session"]
	registry.mu.Unlock()
	if heartbeat == nil {
		t.Fatal("presence heartbeat was not started")
	}
	registry.Close()
	select {
	case <-heartbeat.done:
	case <-time.After(time.Second):
		t.Fatal("presence heartbeat survived registry shutdown")
	}
	if len(registry.SessionIDs()) != 0 {
		t.Fatal("connections remained registered")
	}
	registry.Close()
}
