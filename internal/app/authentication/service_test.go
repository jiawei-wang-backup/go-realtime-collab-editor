package authentication

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	jwtauth "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/adapter/jwt"
	memoryauth "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/adapter/memory/authentication"
	identity "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/authentication"
	"golang.org/x/crypto/bcrypt"
)

func testService(t *testing.T) (*Service, *memoryauth.MemoryUsers, *memoryauth.MemorySessions, *jwtauth.TokenManager) {
	t.Helper()
	users := memoryauth.NewMemoryUsers()
	sessions := memoryauth.NewMemorySessions()
	tokens, err := jwtauth.NewTokenManager(jwtauth.JWTConfig{Secret: []byte("0123456789abcdef0123456789abcdef"), Issuer: "issuer", Audience: "audience", AccessTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	return NewService(users, sessions, tokens, ServiceConfig{RefreshTTL: time.Hour, BcryptCost: bcrypt.MinCost}), users, sessions, tokens
}
func TestRegisterValidationHashAndDuplicate(t *testing.T) {
	service, users, _, _ := testService(t)
	if _, err := service.Register(context.Background(), "x!", "short"); err == nil {
		t.Fatal("invalid input accepted")
	}
	user, err := service.Register(context.Background(), "User.Name", "password1")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := users.GetUserByID(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.PasswordHash == "password1" || bcrypt.CompareHashAndPassword([]byte(stored.PasswordHash), []byte("password1")) != nil {
		t.Fatal("password was not bcrypt hashed")
	}
	if _, err := service.Register(context.Background(), "user.name", "password2"); !errors.Is(err, identity.ErrUsernameExists) {
		t.Fatalf("duplicate error=%v", err)
	}
	if identity.ValidatePassword(string(make([]byte, 73))) == nil {
		t.Fatal("password over 72 bytes accepted")
	}
}
func TestDisabledUserCannotLogin(t *testing.T) {
	service, users, _, _ := testService(t)
	user, err := service.Register(context.Background(), "disabled", "password1")
	if err != nil {
		t.Fatal(err)
	}
	if err := users.SetUserStatus(context.Background(), user.ID, identity.UserDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Login(context.Background(), "disabled", "password1"); !errors.Is(err, identity.ErrInvalidCredentials) {
		t.Fatalf("login error=%v", err)
	}
}

func TestDisabledUserExistingSessionIsRejected(t *testing.T) {
	service, users, _, _ := testService(t)
	user, err := service.Register(context.Background(), "disable-session", "password1")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := service.Login(context.Background(), user.Username, "password1")
	if err != nil {
		t.Fatal(err)
	}
	if err := users.SetUserStatus(context.Background(), user.ID, identity.UserDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuthenticateToken(context.Background(), pair.AccessToken); !errors.Is(err, identity.ErrUserDisabled) {
		t.Fatalf("disabled access error = %v", err)
	}
	if _, err := service.Refresh(context.Background(), pair.RefreshToken); !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatalf("disabled refresh error = %v", err)
	}
}

func TestLogoutWaitsForInFlightPrincipalUse(t *testing.T) {
	service, _, _, _ := testService(t)
	user, _ := service.Register(context.Background(), "logout-gate", "password1")
	pair, _ := service.Login(context.Background(), user.Username, "password1")
	principal, err := service.AuthenticateToken(context.Background(), pair.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	release, err := service.AcquirePrincipal(context.Background(), principal)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- service.Logout(context.Background(), principal) }()
	select {
	case err := <-done:
		t.Fatalf("logout returned before in-flight use completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("logout did not finish after principal use released")
	}
	if err := service.ValidatePrincipal(context.Background(), principal); !errors.Is(err, identity.ErrSessionInvalid) {
		t.Fatalf("principal remained valid after logout: %v", err)
	}
}
func TestJWTValidationCases(t *testing.T) {
	service, _, _, tokens := testService(t)
	user, err := service.Register(context.Background(), "jwt-user", "password1")
	if err != nil {
		t.Fatal(err)
	}
	pair, err := service.Login(context.Background(), user.Username, "password1")
	if err != nil {
		t.Fatal(err)
	}
	if principal, err := service.AuthenticateToken(context.Background(), pair.AccessToken); err != nil || principal.UserID != user.ID || !principal.Trusted {
		t.Fatalf("principal=%#v err=%v", principal, err)
	}
	wrong, _ := jwtauth.NewTokenManager(jwtauth.JWTConfig{Secret: []byte("xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"), Issuer: "issuer", Audience: "audience", AccessTTL: time.Minute})
	if _, err := wrong.Parse(pair.AccessToken); err == nil {
		t.Fatal("wrong signature accepted")
	}
	cases := []struct {
		name, issuer, audience, typ string
		method                      jwt.SigningMethod
		expired                     bool
	}{{"issuer", "wrong", "audience", "access", jwt.SigningMethodHS256, false}, {"audience", "issuer", "wrong", "access", jwt.SigningMethodHS256, false}, {"type", "issuer", "audience", "refresh", jwt.SigningMethodHS256, false}, {"algorithm", "issuer", "audience", "access", jwt.SigningMethodHS384, false}, {"expired", "issuer", "audience", "access", jwt.SigningMethodHS256, true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			exp := now.Add(time.Minute)
			if tc.expired {
				exp = now.Add(-time.Minute)
			}
			claims := jwtauth.Claims{SessionID: "sid", TokenType: tc.typ, RegisteredClaims: jwt.RegisteredClaims{Subject: "user", Issuer: tc.issuer, Audience: jwt.ClaimStrings{tc.audience}, ID: "jti", IssuedAt: jwt.NewNumericDate(now.Add(-time.Minute)), ExpiresAt: jwt.NewNumericDate(exp)}}
			raw, _ := jwt.NewWithClaims(tc.method, claims).SignedString([]byte("0123456789abcdef0123456789abcdef"))
			if _, err := tokens.Parse(raw); err == nil {
				t.Fatal("invalid token accepted")
			}
		})
	}
}
func TestRefreshRotationLogoutAndMultipleSessions(t *testing.T) {
	service, _, _, _ := testService(t)
	user, err := service.Register(context.Background(), "sessions", "password1")
	if err != nil {
		t.Fatal(err)
	}
	first, _ := service.Login(context.Background(), user.Username, "password1")
	second, _ := service.Login(context.Background(), user.Username, "password1")
	rotated, err := service.Refresh(context.Background(), first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Refresh(context.Background(), first.RefreshToken); err == nil {
		t.Fatal("old refresh reused")
	}
	principal, err := service.AuthenticateToken(context.Background(), rotated.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Logout(context.Background(), principal); err != nil {
		t.Fatal(err)
	}
	if _, err := service.AuthenticateToken(context.Background(), rotated.AccessToken); err == nil {
		t.Fatal("logged out access accepted")
	}
	if _, err := service.AuthenticateToken(context.Background(), second.AccessToken); err != nil {
		t.Fatal("independent session revoked")
	}
}
func TestConcurrentRefreshOnlyOneSucceeds(t *testing.T) {
	service, _, _, _ := testService(t)
	user, _ := service.Register(context.Background(), "concurrent", "password1")
	pair, _ := service.Login(context.Background(), user.Username, "password1")
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := service.Refresh(context.Background(), pair.RefreshToken)
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("refresh successes=%d", success)
	}
}

func TestIdempotentRefreshReturnsSameRotatedToken(t *testing.T) {
	service, _, _, _ := testService(t)
	user, _ := service.Register(context.Background(), "idempotent-refresh", "password1")
	pair, _ := service.Login(context.Background(), user.Username, "password1")
	first, err := service.RefreshWithRequestID(context.Background(), pair.RefreshToken, "stable-request")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.RefreshWithRequestID(context.Background(), pair.RefreshToken, "stable-request")
	if err != nil || replayed.RefreshToken != first.RefreshToken || replayed.AccessToken == "" {
		t.Fatalf("replay = %#v, %v", replayed, err)
	}
	if _, err := service.RefreshWithRequestID(context.Background(), pair.RefreshToken, "different-request"); !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatalf("different request error = %v", err)
	}
	second, err := service.RefreshWithRequestID(context.Background(), first.RefreshToken, "second-rotation")
	if err != nil || second.RefreshToken == first.RefreshToken {
		t.Fatalf("second rotation = %#v, %v", second, err)
	}
	if _, err := service.RefreshWithRequestID(context.Background(), pair.RefreshToken, "stable-request"); !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatalf("stale replay after later rotation error = %v", err)
	}
}

func TestConcurrentSameRefreshRequestIsReplayed(t *testing.T) {
	service, _, _, _ := testService(t)
	user, _ := service.Register(context.Background(), "concurrent-replay", "password1")
	pair, _ := service.Login(context.Background(), user.Username, "password1")
	results := make(chan TokenPair, 8)
	errorsChannel := make(chan error, 8)
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := service.RefreshWithRequestID(context.Background(), pair.RefreshToken, "same-request")
			results <- result
			errorsChannel <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("concurrent replay error = %v", err)
		}
	}
	refresh := ""
	for result := range results {
		if refresh == "" {
			refresh = result.RefreshToken
		}
		if result.RefreshToken != refresh {
			t.Fatalf("refresh tokens differ")
		}
	}
}

func TestRefreshReplayRejectedAfterLogoutOrUserDisable(t *testing.T) {
	service, users, _, _ := testService(t)
	logoutUser, _ := service.Register(context.Background(), "replay-logout", "password1")
	logoutPair, _ := service.Login(context.Background(), logoutUser.Username, "password1")
	rotated, err := service.RefreshWithRequestID(context.Background(), logoutPair.RefreshToken, "logout-replay")
	if err != nil {
		t.Fatal(err)
	}
	principal, err := service.AuthenticateToken(context.Background(), rotated.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Logout(context.Background(), principal); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RefreshWithRequestID(context.Background(), logoutPair.RefreshToken, "logout-replay"); !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatalf("logout replay error = %v", err)
	}

	disabledUser, _ := service.Register(context.Background(), "replay-disabled", "password1")
	disabledPair, _ := service.Login(context.Background(), disabledUser.Username, "password1")
	if _, err := service.RefreshWithRequestID(context.Background(), disabledPair.RefreshToken, "disabled-replay"); err != nil {
		t.Fatal(err)
	}
	if err := users.SetUserStatus(context.Background(), disabledUser.ID, identity.UserDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RefreshWithRequestID(context.Background(), disabledPair.RefreshToken, "disabled-replay"); !errors.Is(err, identity.ErrInvalidRefreshToken) {
		t.Fatalf("disabled replay error = %v", err)
	}
}
