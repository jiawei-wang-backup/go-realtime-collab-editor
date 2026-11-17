package room

import "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/protocol"

// Connection is the transport-independent connection used by a Room. TrySend must
// be nonblocking, and Close must be safe to call more than once.
type Connection interface {
	Identity() string
	TrySend(protocol.Message) bool
	Close(error)
}

// PresenceClient is an optional capability. Existing QUIC protocol consumers
// keep their original message ordering, while browser transports can opt in.
type PresenceClient interface {
	Connection
	SupportsPresence() bool
}

// CursorClient is implemented by transports that render ephemeral cursor
// presence. DisplayName is derived from authenticated server-side identity.
type CursorClient interface {
	PresenceClient
	SupportsCursor() bool
	DisplayName() string
}
