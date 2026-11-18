package room

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/cluster"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/storage"
	"go.opentelemetry.io/otel"
)

const defaultIdleTimeout = 30 * time.Second

const (
	defaultSnapshotInterval = 100
	defaultMaxHistory       = 1000
	defaultSnapshotTimeout  = 5 * time.Second
)

type roomState uint8

const (
	roomOpening roomState = iota
	roomActive
	roomStopping
)

type roomEntry struct {
	state     roomState
	room      *Room
	handle    *cluster.OwnershipHandle
	ready     chan struct{}
	stopped   chan struct{}
	openErr   error
	readyOnce sync.Once
	stopOnce  sync.Once
}

type ManagerOption func(*Manager)

type Observer interface {
	SlowClientDisconnected()
	SnapshotCompleted(success bool)
	RecoveryCompleted(duration time.Duration, success bool)
}

func WithIdleTimeout(timeout time.Duration) ManagerOption {
	return func(manager *Manager) {
		if timeout > 0 {
			manager.idleTimeout = timeout
		}
	}
}

func WithContext(ctx context.Context) ManagerOption {
	return func(manager *Manager) {
		if ctx != nil {
			manager.parent = ctx
		}
	}
}

func WithSnapshotPolicy(interval, maxHistory int, timeout time.Duration) ManagerOption {
	return func(manager *Manager) {
		if interval > 0 {
			manager.snapshotInterval = uint64(interval)
		}
		if maxHistory > 0 {
			manager.maxHistory = maxHistory
		}
		if timeout > 0 {
			manager.snapshotTimeout = timeout
		}
	}
}

func WithWriteAuthority(authority storage.WriteAuthority) ManagerOption {
	return func(manager *Manager) {
		manager.authority = authority
	}
}

func WithOwnership(coordinator cluster.OwnershipCoordinator) ManagerOption {
	return func(manager *Manager) {
		manager.ownership = coordinator
		manager.authority = storage.WriteAuthority{}
	}
}

func WithObserver(observer Observer) ManagerOption {
	return func(manager *Manager) { manager.observer = observer }
}

type Manager struct {
	mu               sync.Mutex
	rooms            map[string]*roomEntry
	storage          storage.Store
	parent           context.Context
	ctx              context.Context
	cancel           context.CancelFunc
	idleTimeout      time.Duration
	snapshotInterval uint64
	maxHistory       int
	snapshotTimeout  time.Duration
	authority        storage.WriteAuthority
	ownership        cluster.OwnershipCoordinator
	observer         Observer
	failover         sync.WaitGroup
	recovery         recoveryAdmission
	recoveryWorkers  sync.WaitGroup
}

type Stats struct {
	ActiveRooms     int
	ActiveClients   int64
	EventQueueDepth int
}

func (manager *Manager) Stats() Stats {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	var stats Stats
	for _, entry := range manager.rooms {
		if entry.state != roomActive || entry.room == nil {
			continue
		}
		stats.ActiveRooms++
		stats.ActiveClients += entry.room.ClientCount()
		stats.EventQueueDepth += entry.room.EventQueueDepth()
	}
	return stats
}

func NewManager(store storage.Store, options ...ManagerOption) *Manager {
	manager := &Manager{
		rooms:            make(map[string]*roomEntry),
		storage:          store,
		parent:           context.Background(),
		idleTimeout:      defaultIdleTimeout,
		snapshotInterval: defaultSnapshotInterval,
		maxHistory:       defaultMaxHistory,
		snapshotTimeout:  defaultSnapshotTimeout,
		authority:        storage.LocalAuthority,
		recovery:         recoveryAdmission{timeout: defaultRecoveryTimeout},
	}
	for _, option := range options {
		option(manager)
	}
	manager.ctx, manager.cancel = context.WithCancel(manager.parent)
	if source, ok := manager.ownership.(cluster.FailoverSource); ok {
		manager.failover.Add(1)
		go manager.runFailover(source.FailoverCandidates())
	}
	return manager
}

func (manager *Manager) runFailover(candidates <-chan string) {
	defer manager.failover.Done()
	for {
		select {
		case <-manager.ctx.Done():
			return
		case documentID, open := <-candidates:
			if !open {
				return
			}
			if _, err := manager.Room(documentID); err != nil &&
				!errors.Is(err, cluster.ErrOwnerExists) &&
				!errors.Is(err, cluster.ErrNotPreferredNode) &&
				!errors.Is(err, ErrManagerClosed) {
				slog.Error("automatic ownership recovery failed", "component", "room_manager", "event", "recovery_failed", "doc_id", documentID, "error", err)
			}
		}
	}
}

func (manager *Manager) Room(documentID string) (*Room, error) {
	return manager.RoomContext(context.Background(), documentID)
}

// RoomContext bounds the caller's wait. Recovery itself belongs to the manager,
// so canceling one request does not interrupt other callers for that document.
func (manager *Manager) RoomContext(ctx context.Context, documentID string) (*Room, error) {
	for {
		if manager.ctx.Err() != nil {
			return nil, ErrManagerClosed
		}
		if ctx.Err() != nil {
			return nil, recoveryUnavailable("room wait canceled or timed out")
		}
		entry, err := manager.entry(documentID)
		if err != nil {
			return nil, err
		}
		manager.mu.Lock()
		state, target := entry.state, entry.room
		manager.mu.Unlock()
		switch state {
		case roomOpening:
			select {
			case <-entry.ready:
				manager.mu.Lock()
				openErr := entry.openErr
				manager.mu.Unlock()
				if openErr != nil {
					return nil, openErr
				}
			case <-ctx.Done():
				return nil, recoveryUnavailable("room wait canceled or timed out")
			case <-manager.ctx.Done():
				return nil, ErrManagerClosed
			}
		case roomActive:
			if target != nil && !target.Stopped() {
				return target, nil
			}
			// A stopped Room still needs to release its ownership before replacement.
			fallthrough
		case roomStopping:
			select {
			case <-entry.stopped:
			case <-ctx.Done():
				return nil, recoveryUnavailable("room wait canceled or timed out")
			case <-manager.ctx.Done():
				return nil, ErrManagerClosed
			}
		}
	}
}

func (manager *Manager) open(ctx context.Context, documentID string, entry *roomEntry) {
	target, handle, err := manager.buildRoom(ctx, documentID, entry)
	if err == nil && manager.ownership != nil {
		err = manager.ownership.Activate(ctx, handle)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && target.Stopped() {
		err = cluster.ErrOwnershipLost
	}
	if err != nil {
		if target != nil {
			target.cancel()
			<-target.Done()
		} else if handle != nil {
			manager.release(handle)
		}
		manager.failOpen(documentID, entry, manager.recoveryError(ctx, err))
		return
	}

	manager.mu.Lock()
	if manager.rooms[documentID] == entry && manager.ctx.Err() == nil {
		entry.room = target
		entry.handle = handle
		entry.state = roomActive
		entry.readyOnce.Do(func() { close(entry.ready) })
		manager.mu.Unlock()
		return
	}
	manager.mu.Unlock()
	target.cancel()
	<-target.Done()
	manager.failOpen(documentID, entry, ErrManagerClosed)
}

func (manager *Manager) buildRoom(ctx context.Context, documentID string, entry *roomEntry) (*Room, *cluster.OwnershipHandle, error) {
	traceCtx, span := otel.Tracer("collab-editor/room").Start(ctx, "room.recovery")
	defer span.End()
	recoveryStarted := time.Now()
	recoverySucceeded := false
	defer func() {
		if manager.observer != nil {
			manager.observer.RecoveryCompleted(time.Since(recoveryStarted), recoverySucceeded)
		}
	}()
	slog.Info("room recovery started", "component", "room_manager", "event", "recovery_started", "doc_id", documentID)
	parent := manager.ctx
	recoveryCtx := traceCtx
	authority := manager.authority
	var handle *cluster.OwnershipHandle
	var err error
	if manager.ownership != nil {
		handle, err = manager.ownership.Acquire(traceCtx, documentID)
		if err != nil {
			return nil, nil, err
		}
		parent = handle.Context
		authority = handle.Authority()
		// The Room follows the ownership lease, while its initial database work
		// must stop on either a lost lease or the recovery deadline.
		ownedRecoveryCtx, cancel := context.WithCancel(traceCtx)
		stop := context.AfterFunc(handle.Context, cancel)
		defer stop()
		defer cancel()
		if handle.Context.Err() != nil {
			cancel()
		}
		recoveryCtx = ownedRecoveryCtx
	}
	if err := manager.prepareWriteAuthority(recoveryCtx, documentID, authority); err != nil {
		if handle != nil {
			manager.release(handle)
		}
		return nil, nil, err
	}
	if handle != nil {
		if err := manager.ownership.BeginRecovery(recoveryCtx, handle); err != nil {
			manager.release(handle)
			return nil, nil, err
		}
	}
	state, err := manager.storage.LoadRecoveryState(recoveryCtx, documentID, authority)
	if err != nil {
		if handle != nil {
			manager.release(handle)
		}
		return nil, nil, err
	}
	if err := storage.ValidateRecoveryStateContext(recoveryCtx, state); err != nil {
		if handle != nil {
			manager.release(handle)
		}
		return nil, nil, err
	}
	recoverySucceeded = true
	slog.Info("room recovery completed", "component", "room_manager", "event", "recovery_completed", "doc_id", documentID, "version", state.Document.Version, "duration", time.Since(recoveryStarted))
	target := newRoom(
		parent,
		state.Document,
		state.Operations,
		state.Snapshot,
		state.Permissions,
		manager.storage,
		authority,
		manager.idleTimeout,
		manager.snapshotInterval,
		manager.maxHistory,
		manager.snapshotTimeout,
		manager.observer,
		func(target *Room) { manager.roomStopped(documentID, entry, target) },
	)
	manager.mu.Lock()
	if manager.rooms[documentID] != entry || manager.ctx.Err() != nil {
		manager.mu.Unlock()
		target.cancel()
		if handle != nil {
			manager.release(handle)
		}
		return nil, nil, ErrManagerClosed
	}
	entry.handle = handle
	entry.room = target
	manager.mu.Unlock()
	go target.Run()
	return target, handle, nil
}

func (manager *Manager) prepareWriteAuthority(ctx context.Context, documentID string, authority storage.WriteAuthority) error {
	if err := storage.ValidateWriteAuthority(authority); err != nil {
		return err
	}
	current, err := manager.storage.GetWriteAuthority(ctx, documentID)
	if err != nil {
		return err
	}
	if current == authority {
		return nil
	}
	if authority.FencingToken <= current.FencingToken {
		return storage.ErrOwnershipConflict
	}
	return manager.storage.ActivateOwnership(ctx, documentID, authority)
}

func (manager *Manager) failOpen(documentID string, entry *roomEntry, err error) {
	manager.mu.Lock()
	entry.openErr = err
	entry.readyOnce.Do(func() { close(entry.ready) })
	entry.stopOnce.Do(func() { close(entry.stopped) })
	if manager.rooms[documentID] == entry {
		delete(manager.rooms, documentID)
	}
	manager.mu.Unlock()
}

func (manager *Manager) roomStopped(documentID string, entry *roomEntry, target *Room) {
	slog.Info("room closed", "component", "room_manager", "event", "room_closed", "doc_id", documentID)
	manager.mu.Lock()
	if manager.rooms[documentID] != entry || entry.room != target {
		manager.mu.Unlock()
		return
	}
	entry.state = roomStopping
	handle := entry.handle
	manager.mu.Unlock()

	if handle != nil {
		manager.release(handle)
	}
	manager.mu.Lock()
	if manager.rooms[documentID] == entry {
		delete(manager.rooms, documentID)
	}
	entry.stopOnce.Do(func() { close(entry.stopped) })
	manager.mu.Unlock()
}

func (manager *Manager) release(handle *cluster.OwnershipHandle) {
	if manager.ownership == nil || handle == nil {
		return
	}
	if handle.Context.Err() != nil {
		// Once the lease or owner key is gone, releasing through etcd is unsafe and
		// unnecessary. The old node only needs to stop its local Room.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = manager.ownership.BeginDraining(ctx, handle)
	err := manager.ownership.Release(ctx, handle)
	if err != nil && !errors.Is(err, cluster.ErrOwnershipLost) {
		handle.Cancel(err)
	}
}

// Shutdown prevents new Rooms, stops active Rooms, and waits for their
// goroutines to exit or for ctx to expire.
func (manager *Manager) Shutdown(ctx context.Context) error {
	manager.cancel()

	manager.mu.Lock()
	entries := make([]*roomEntry, 0, len(manager.rooms))
	for _, entry := range manager.rooms {
		entries = append(entries, entry)
		if entry.room != nil {
			entry.room.cancel()
		}
	}
	manager.mu.Unlock()

	for _, entry := range entries {
		select {
		case <-entry.stopped:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	manager.failover.Wait()
	manager.recoveryWorkers.Wait()
	return nil
}

// StopRoom is a synchronization barrier used before destructive document
// operations. It prevents the current authoritative goroutine from accepting
// more edits and waits until all clients have been detached.
func (manager *Manager) StopRoom(ctx context.Context, documentID string) error {
	manager.mu.Lock()
	entry := manager.rooms[documentID]
	if entry != nil && entry.room != nil {
		entry.room.cancel()
	}
	manager.mu.Unlock()
	if entry == nil {
		return nil
	}
	select {
	case <-entry.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (manager *Manager) roomCount() int {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	return len(manager.rooms)
}
