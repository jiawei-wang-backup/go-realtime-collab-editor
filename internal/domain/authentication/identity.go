package authentication

import "errors"

var (
	ErrMissingIdentity     = errors.New("authentication required")
	ErrInvalidToken        = errors.New("invalid access token")
	ErrInvalidCredentials  = errors.New("invalid username or password")
	ErrSessionInvalid      = errors.New("session is not active")
	ErrUsernameExists      = errors.New("username already exists")
	ErrUserNotFound        = errors.New("user not found")
	ErrUserDisabled        = errors.New("user is disabled")
	ErrInvalidRefreshToken = errors.New("invalid refresh token")
	ErrLogoutIncomplete    = errors.New("logout could not confirm all in-flight operations completed")
)

type Principal struct {
	UserID    string
	Username  string
	SessionID string
	Trusted   bool
}
