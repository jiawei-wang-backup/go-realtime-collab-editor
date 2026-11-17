package authentication

import (
	"context"
	"regexp"
	"time"
)

type UserStatus string

const (
	UserActive   UserStatus = "active"
	UserDisabled UserStatus = "disabled"
)

type User struct {
	ID           string     `json:"id"`
	Username     string     `json:"username"`
	PasswordHash string     `json:"-"`
	Status       UserStatus `json:"status"`
	CreatedAt    time.Time  `json:"created_at,omitempty"`
	UpdatedAt    time.Time  `json:"updated_at,omitempty"`
}
type UserRepository interface {
	CreateUser(context.Context, User) error
	GetUserByID(context.Context, string) (User, error)
	GetUserByUsername(context.Context, string) (User, error)
	UpdatePassword(context.Context, string, string) error
	SetUserStatus(context.Context, string, UserStatus) error
}

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,32}$`)

type ValidationError string

func (e ValidationError) Error() string { return string(e) }
func ValidateUsername(username string) error {
	if !usernamePattern.MatchString(username) {
		return ValidationError("username must be 3-32 characters using letters, numbers, _, - or .")
	}
	return nil
}
func ValidatePassword(password string) error {
	length := len([]byte(password))
	if length < 8 {
		return ValidationError("password must be at least 8 bytes")
	}
	if length > 72 {
		return ValidationError("password must not exceed 72 bytes")
	}
	return nil
}
