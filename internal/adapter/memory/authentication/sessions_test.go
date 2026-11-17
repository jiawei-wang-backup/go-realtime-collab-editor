package memoryauth

import (
	"context"
	"errors"
	"testing"
	"time"

	jwtauth "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/adapter/jwt"
	authentication "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/app/authentication"
	identity "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/authentication"
	"golang.org/x/crypto/bcrypt"
)

func TestRefreshReplayExpiryAndWrongEncryptionKey(t *testing.T) {
	users := NewMemoryUsers()
	sessions := NewMemorySessions()
	tokens, err := jwtauth.NewTokenManager(jwtauth.JWTConfig{Secret: []byte("0123456789abcdef0123456789abcdef"), Issuer: "issuer", Audience: "audience", AccessTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	service := authentication.NewService(users, sessions, tokens, authentication.ServiceConfig{RefreshTTL: time.Hour, RefreshReplayTTL: time.Minute, BcryptCost: bcrypt.MinCost})
	user, _ := service.Register(context.Background(), "replay-expiry", "password1")
	pair, _ := service.Login(context.Background(), user.Username, "password1")
	if _, err := service.RefreshWithRequestID(context.Background(), pair.RefreshToken, "expiring"); err != nil {
		t.Fatal(err)
	}
	baseNow := time.Now()
	sessions.now = func() time.Time { return baseNow.Add(2 * time.Minute) }
	if _, err := service.RefreshWithRequestID(context.Background(), pair.RefreshToken, "expiring"); !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatalf("expired replay error = %v", err)
	}

	// A process configured with a different replay key cannot decrypt an
	// existing record and must reject instead of returning guessed credentials.
	sessions.now = time.Now
	secondPair, _ := service.Login(context.Background(), user.Username, "password1")
	if _, err := service.RefreshWithRequestID(context.Background(), secondPair.RefreshToken, "wrong-key"); err != nil {
		t.Fatal(err)
	}
	other := authentication.NewService(users, sessions, tokens, authentication.ServiceConfig{
		RefreshTTL: time.Hour, ReplayEncryptionKey: []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"), BcryptCost: bcrypt.MinCost,
	})
	if _, err := other.RefreshWithRequestID(context.Background(), secondPair.RefreshToken, "wrong-key"); !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatalf("wrong replay key error = %v", err)
	}
}
