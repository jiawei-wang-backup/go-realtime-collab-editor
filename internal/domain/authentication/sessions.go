package authentication

import (
	"context"
	"time"
)

type Session struct {
	UserID, RefreshHash string
	CreatedAt           time.Time
}
type SessionStore interface {
	Create(context.Context, string, Session, time.Duration) error
	Get(context.Context, string) (Session, error)
	Rotate(context.Context, string, string, string, time.Duration) error
	Delete(context.Context, string) error
	Close() error
}

type IdempotentSessionStore interface {
	RotateReplay(ctx context.Context, sessionID, oldHash, newHash, requestID, encryptedResult string, sessionTTL, replayTTL time.Duration) (storedResult, storedNewHash string, replayed bool, err error)
}

// DistributedSessionStore provides cross-node logout notifications, operation
// leases, and presence. Pub/Sub only accelerates connection closure; the
// session key remains authoritative for authentication.
type DistributedSessionStore interface {
	Revoke(context.Context, string) error
	AcquireOperation(context.Context, string, string, time.Duration) (release func(context.Context) error, err error)
	WaitForOperations(context.Context, string) error
	NewRevocationSubscription() RevocationSubscription
	TouchPresence(context.Context, string, string, time.Duration) error
	DeletePresence(context.Context, string, string) error
}

// RevocationSubscription owns one subscription connection, not its underlying
// Redis client. Service closes it explicitly to unblock network reads.
type RevocationSubscription interface {
	Run(context.Context, func(string), func()) error
	Close() error
}
