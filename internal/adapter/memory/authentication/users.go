package memoryauth

import (
	"context"
	"strings"
	"sync"
	"time"

	identity "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/authentication"
)

type MemoryUsers struct {
	mu     sync.RWMutex
	byID   map[string]identity.User
	byName map[string]string
}

func NewMemoryUsers() *MemoryUsers {
	return &MemoryUsers{byID: map[string]identity.User{}, byName: map[string]string{}}
}
func (m *MemoryUsers) CreateUser(ctx context.Context, user identity.User) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := strings.ToLower(user.Username)
	if _, ok := m.byName[key]; ok {
		return identity.ErrUsernameExists
	}
	if _, ok := m.byID[user.ID]; ok {
		return identity.ErrUsernameExists
	}
	now := time.Now().UTC()
	user.CreatedAt = now
	user.UpdatedAt = now
	m.byID[user.ID] = user
	m.byName[key] = user.ID
	return nil
}
func (m *MemoryUsers) GetUserByID(ctx context.Context, id string) (identity.User, error) {
	if err := ctx.Err(); err != nil {
		return identity.User{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	user, ok := m.byID[id]
	if !ok {
		return identity.User{}, identity.ErrUserNotFound
	}
	return user, nil
}
func (m *MemoryUsers) GetUserByUsername(ctx context.Context, username string) (identity.User, error) {
	if err := ctx.Err(); err != nil {
		return identity.User{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	id, ok := m.byName[strings.ToLower(username)]
	if !ok {
		return identity.User{}, identity.ErrUserNotFound
	}
	return m.byID[id], nil
}
func (m *MemoryUsers) UpdatePassword(ctx context.Context, id, hash string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.byID[id]
	if !ok {
		return identity.ErrUserNotFound
	}
	user.PasswordHash = hash
	user.UpdatedAt = time.Now().UTC()
	m.byID[id] = user
	return nil
}
func (m *MemoryUsers) SetUserStatus(ctx context.Context, id string, status identity.UserStatus) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	user, ok := m.byID[id]
	if !ok {
		return identity.ErrUserNotFound
	}
	user.Status = status
	user.UpdatedAt = time.Now().UTC()
	m.byID[id] = user
	return nil
}
