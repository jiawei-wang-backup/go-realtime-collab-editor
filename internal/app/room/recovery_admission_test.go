package room

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/adapter/memory/documents"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/storage"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/protocol"
)

type blockingRecoveryStore struct {
	storage.Store
	starts chan string
	gates  map[string]chan struct{}
}

func (store *blockingRecoveryStore) LoadRecoveryState(ctx context.Context, id string, authority storage.WriteAuthority) (storage.RecoveryState, error) {
	store.starts <- id
	select {
	case <-store.gates[id]:
		return store.Store.LoadRecoveryState(ctx, id, authority)
	case <-ctx.Done():
		return storage.RecoveryState{}, ctx.Err()
	}
}

func recoveryFixture(t *testing.T, limit, queue int, timeout time.Duration) (*Manager, *blockingRecoveryStore) {
	t.Helper()
	base := memory.NewMemory()
	store := &blockingRecoveryStore{Store: base, starts: make(chan string, 8), gates: map[string]chan struct{}{}}
	for _, id := range []string{"warm", "one", "two", "three", "overflow"} {
		if _, err := base.CreateDocument(context.Background(), id, id, "owner"); err != nil {
			t.Fatal(err)
		}
		store.gates[id] = make(chan struct{})
	}
	close(store.gates["warm"])
	manager := NewManager(store, WithRecoveryAdmission(limit, queue, timeout))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := manager.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return manager, store
}

func expectRecoveryStart(t *testing.T, store *blockingRecoveryStore, want string) {
	t.Helper()
	select {
	case got := <-store.starts:
		if got != want {
			t.Fatalf("recovery started for %s, want %s", got, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("recovery for %s did not start", want)
	}
}

func TestRecoveryAdmissionFIFOOverflowAndWarmRoom(t *testing.T) {
	manager, store := recoveryFixture(t, 1, 2, 3*time.Second)
	warm, err := manager.Room("warm")
	if err != nil {
		t.Fatal(err)
	}
	expectRecoveryStart(t, store, "warm")

	first, err := manager.entry("one")
	if err != nil {
		t.Fatal(err)
	}
	expectRecoveryStart(t, store, "one")
	for _, id := range []string{"two", "three"} {
		if _, err := manager.entry(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.entry("overflow"); err == nil {
		t.Fatal("full queue accepted a fourth distinct document")
	} else {
		assertCode(t, err, protocol.CodeServiceUnavailable)
	}
	if duplicate, err := manager.entry("one"); err != nil || duplicate != first {
		t.Fatalf("duplicate recovery = (%p, %v), want %p", duplicate, err, first)
	}
	if existing, err := manager.Room("warm"); err != nil || existing != warm {
		t.Fatalf("warm Room did not bypass full queue: %v", err)
	}
	select {
	case id := <-store.starts:
		t.Fatalf("recovery for %s started before a slot was available", id)
	default:
	}

	close(store.gates["one"])
	expectRecoveryStart(t, store, "two")
	close(store.gates["two"])
	expectRecoveryStart(t, store, "three")
	close(store.gates["three"])
	if _, err := manager.Room("three"); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryAdmissionCallerCancellationDoesNotCancelSharedWork(t *testing.T) {
	manager, store := recoveryFixture(t, 1, 1, 3*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := manager.RoomContext(ctx, "one")
		result <- err
	}()
	expectRecoveryStart(t, store, "one")
	cancel()
	select {
	case err := <-result:
		assertCode(t, err, protocol.CodeServiceUnavailable)
	case <-time.After(time.Second):
		t.Fatal("request did not stop waiting after cancellation")
	}
	close(store.gates["one"])
	if _, err := manager.Room("one"); err != nil {
		t.Fatalf("shared recovery was canceled: %v", err)
	}
	select {
	case id := <-store.starts:
		t.Fatalf("document %s was recovered again", id)
	default:
	}
}

func TestRecoveryTimeoutReleasesSlotAndShutdownClearsBacklog(t *testing.T) {
	manager, store := recoveryFixture(t, 1, 1, 200*time.Millisecond)
	result := make(chan error, 1)
	go func() {
		_, err := manager.Room("one")
		result <- err
	}()
	expectRecoveryStart(t, store, "one")
	select {
	case err := <-result:
		assertCode(t, err, protocol.CodeServiceUnavailable)
	case <-time.After(2 * time.Second):
		t.Fatal("recovery exceeded its deadline")
	}

	close(store.gates["two"])
	if _, err := manager.Room("two"); err != nil {
		t.Fatalf("failed recovery did not free its slot: %v", err)
	}
	expectRecoveryStart(t, store, "two")

	if _, err := manager.entry("three"); err != nil {
		t.Fatal(err)
	}
	expectRecoveryStart(t, store, "three")
	if _, err := manager.entry("overflow"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := manager.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Room("overflow"); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("closed manager returned %v", err)
	}
	if count := manager.roomCount(); count != 0 {
		t.Fatalf("shutdown retained %d Rooms", count)
	}
}
