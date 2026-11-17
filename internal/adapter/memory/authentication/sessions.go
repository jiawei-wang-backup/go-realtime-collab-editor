package memoryauth

import (
	"context"
	"crypto/subtle"
	"sync"
	"time"

	identity "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/authentication"
)

type MemorySessions struct {
	mu       sync.Mutex
	sessions map[string]memorySession
	replays  map[string]memoryReplay
	now      func() time.Time
}
type memorySession struct {
	identity.Session
	expires time.Time
}
type memoryReplay struct {
	oldHash, newHash, encrypted string
	expires                     time.Time
}

func NewMemorySessions() *MemorySessions {
	return &MemorySessions{sessions: map[string]memorySession{}, replays: map[string]memoryReplay{}, now: time.Now}
}
func (m *MemorySessions) Create(ctx context.Context, id string, session identity.Session, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[id] = memorySession{Session: session, expires: m.now().Add(ttl)}
	return nil
}
func (m *MemorySessions) Get(ctx context.Context, id string) (identity.Session, error) {
	if err := ctx.Err(); err != nil {
		return identity.Session{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || !m.now().Before(s.expires) {
		delete(m.sessions, id)
		return identity.Session{}, identity.ErrSessionInvalid
	}
	return s.Session, nil
}
func (m *MemorySessions) Rotate(ctx context.Context, id, oldHash, newHash string, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok || !m.now().Before(s.expires) || subtle.ConstantTimeCompare([]byte(s.RefreshHash), []byte(oldHash)) != 1 {
		return identity.ErrInvalidRefreshToken
	}
	s.RefreshHash = newHash
	s.expires = m.now().Add(ttl)
	m.sessions[id] = s
	return nil
}

func (m *MemorySessions) RotateReplay(ctx context.Context, id, oldHash, newHash, requestID, encrypted string, sessionTTL, replayTTL time.Duration) (string, string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", "", false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	session, ok := m.sessions[id]
	if !ok || !now.Before(session.expires) {
		delete(m.sessions, id)
		return "", "", false, identity.ErrInvalidRefreshToken
	}
	key := id + "\x00" + requestID
	if replay, exists := m.replays[key]; exists && now.Before(replay.expires) {
		if replay.oldHash != oldHash || session.RefreshHash != replay.newHash {
			return "", "", false, identity.ErrInvalidRefreshToken
		}
		return replay.encrypted, replay.newHash, true, nil
	}
	if subtle.ConstantTimeCompare([]byte(session.RefreshHash), []byte(oldHash)) != 1 {
		return "", "", false, identity.ErrInvalidRefreshToken
	}
	session.RefreshHash = newHash
	session.expires = now.Add(sessionTTL)
	m.sessions[id] = session
	m.replays[key] = memoryReplay{oldHash: oldHash, newHash: newHash, encrypted: encrypted, expires: now.Add(replayTTL)}
	return encrypted, newHash, false, nil
}
func (m *MemorySessions) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	return nil
}
func (*MemorySessions) Close() error { return nil }
