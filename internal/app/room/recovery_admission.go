package room

import (
	"container/list"
	"context"
	"errors"
	"time"

	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/protocol"
)

const defaultRecoveryTimeout = 10 * time.Second

// recoveryAdmission is protected by Manager.mu. Only distinct documents in
// the opening state take slots; active Rooms and duplicate waiters bypass it.
type recoveryAdmission struct {
	limit   int
	maxWait int
	timeout time.Duration
	active  int
	waiting list.List
}

type recoveryJob struct {
	documentID string
	entry      *roomEntry
	turn       chan struct{}
	queued     *list.Element
	started    bool
}

// WithRecoveryAdmission bounds concurrent cold opens and their FIFO backlog.
// A limit of zero keeps the unlimited mode used as the benchmark control.
func WithRecoveryAdmission(limit, maxWait int, timeout time.Duration) ManagerOption {
	return func(manager *Manager) {
		manager.recovery.limit = max(0, limit)
		manager.recovery.maxWait = max(0, maxWait)
		if timeout > 0 {
			manager.recovery.timeout = timeout
		}
	}
}

func (manager *Manager) entry(documentID string) (*roomEntry, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.ctx.Err() != nil {
		return nil, ErrManagerClosed
	}
	if existing := manager.rooms[documentID]; existing != nil {
		return existing, nil
	}
	gate := &manager.recovery
	canStart := gate.limit == 0 || gate.active < gate.limit
	if !canStart && gate.waiting.Len() >= gate.maxWait {
		return nil, recoveryUnavailable("room recovery queue is full")
	}
	entry := &roomEntry{state: roomOpening, ready: make(chan struct{}), stopped: make(chan struct{})}
	manager.rooms[documentID] = entry
	job := &recoveryJob{documentID: documentID, entry: entry, turn: make(chan struct{})}
	ctx, cancel := context.WithTimeout(manager.ctx, gate.timeout)
	if canStart {
		gate.start(job)
	} else {
		job.queued = gate.waiting.PushBack(job)
	}
	manager.recoveryWorkers.Add(1)
	go manager.runRecovery(ctx, cancel, job)
	return entry, nil
}

func (gate *recoveryAdmission) start(job *recoveryJob) {
	job.started = true
	gate.active++
	close(job.turn)
}

func (manager *Manager) runRecovery(ctx context.Context, cancel context.CancelFunc, job *recoveryJob) {
	defer manager.recoveryWorkers.Done()
	defer cancel()
	defer manager.finishRecovery(job)
	select {
	case <-job.turn:
	case <-ctx.Done():
	}
	// A queued job may receive a slot at the same time its deadline expires.
	if err := ctx.Err(); err != nil {
		manager.failOpen(job.documentID, job.entry, manager.recoveryError(ctx, err))
		return
	}
	manager.open(ctx, job.documentID, job.entry)
}

func (manager *Manager) finishRecovery(job *recoveryJob) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	gate := &manager.recovery
	if job.queued != nil {
		gate.waiting.Remove(job.queued)
	}
	if !job.started {
		return
	}
	gate.active--
	if next := gate.waiting.Front(); next != nil && manager.ctx.Err() == nil {
		nextJob := next.Value.(*recoveryJob)
		gate.waiting.Remove(next)
		nextJob.queued = nil
		gate.start(nextJob)
	}
}

func (manager *Manager) recoveryError(ctx context.Context, err error) error {
	if manager.ctx.Err() != nil {
		return ErrManagerClosed
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return recoveryUnavailable("room recovery timed out")
	}
	return err
}

func recoveryUnavailable(message string) error {
	return &CodedError{Code: protocol.CodeServiceUnavailable, Message: message}
}
