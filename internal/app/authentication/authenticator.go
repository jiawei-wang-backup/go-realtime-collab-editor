package authentication

import (
	"context"
	"net/http"
	"strings"

	identity "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/authentication"
)

type Authenticator interface {
	AuthenticateToken(context.Context, string) (identity.Principal, error)
	AuthenticateHTTP(*http.Request) (identity.Principal, error)
	ValidatePrincipal(context.Context, identity.Principal) error
}

// PrincipalUseAuthenticator pins a validated session for one complete
// application message. Logout takes the matching exclusive gate so it cannot
// return while an already-authorized operation is still running.
type PrincipalUseAuthenticator interface {
	Authenticator
	AcquirePrincipal(context.Context, identity.Principal) (release func(), err error)
}

type DevelopmentAuthenticator struct{}

func (DevelopmentAuthenticator) AuthenticateToken(_ context.Context, token string) (identity.Principal, error) {
	return developmentPrincipal(token)
}
func (DevelopmentAuthenticator) AuthenticateHTTP(request *http.Request) (identity.Principal, error) {
	return developmentPrincipal(request.Header.Get("X-User-ID"))
}
func (DevelopmentAuthenticator) ValidatePrincipal(_ context.Context, principal identity.Principal) error {
	if principal.UserID == "" {
		return identity.ErrMissingIdentity
	}
	return nil
}
func developmentPrincipal(userID string) (identity.Principal, error) {
	if userID == "" {
		return identity.Principal{}, identity.ErrMissingIdentity
	}
	return identity.Principal{UserID: userID, Username: userID, Trusted: false}, nil
}

func BearerToken(request *http.Request) (string, error) {
	parts := strings.Fields(request.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", identity.ErrMissingIdentity
	}
	return parts[1], nil
}

type contextKey struct{}

func WithPrincipal(ctx context.Context, principal identity.Principal) context.Context {
	return context.WithValue(ctx, contextKey{}, principal)
}
func PrincipalFromContext(ctx context.Context) (identity.Principal, bool) {
	principal, ok := ctx.Value(contextKey{}).(identity.Principal)
	return principal, ok
}
func UserIDFromContext(ctx context.Context) string {
	principal, _ := PrincipalFromContext(ctx)
	return principal.UserID
}
