// Package browser contains the transport-independent browser collaboration
// session. WebSocket and WebTransport adapters only provide framed messages;
// authentication, owner routing, Room access, recovery, and revocation live
// here so their behavior cannot drift between transports.
package browser

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/app/authentication"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/app/room"
	identity "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/authentication"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/cluster"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/protocol"
)

const cursorMessagesPerSecond = 30

type MessageConn interface {
	Read(context.Context, *protocol.Message) error
	Write(context.Context, protocol.Message) error
	CloseNow() error
}

type Config struct {
	Manager            *room.Manager
	Authenticator      authentication.Authenticator
	ConnectionRegistry *authentication.ConnectionRegistry
	AuthTimeout        time.Duration
	OperationTimeout   time.Duration
	WriteTimeout       time.Duration
	SendQueueSize      int
	OwnerResolver      cluster.OwnerResolver
	LocalNode          cluster.Node
	NodeDirectory      cluster.NodeDirectory
}

func (config Config) Validate() error {
	if config.Manager == nil || config.Authenticator == nil {
		return errors.New("browser realtime manager and authenticator are required")
	}
	if config.AuthTimeout <= 0 || config.OperationTimeout <= 0 || config.WriteTimeout <= 0 || config.SendQueueSize <= 0 {
		return errors.New("browser realtime timeouts and send queue size must be positive")
	}
	return nil
}

func Serve(parent context.Context, connection MessageConn, config Config) {
	client := &sessionClient{
		connection: connection, config: config,
		send:        make(chan protocol.Message, config.SendQueueSize),
		cursorReady: make(chan struct{}, 1), writeDone: make(chan struct{}),
	}
	client.run(parent)
}

type sessionClient struct {
	connection     MessageConn
	config         Config
	localCursors   bool
	principal      identity.Principal
	room           *room.Room
	docID          string
	send           chan protocol.Message
	writeDone      chan struct{}
	unregister     func()
	mu             sync.Mutex
	cursorReady    chan struct{}
	cursorOrder    []string
	pendingCursors map[string]pendingCursor
	closed         bool
	joined         bool
	cursorWindow   time.Time
	cursorMessages int
}

var _ room.Connection = (*sessionClient)(nil)

func (client *sessionClient) Identity() string       { return client.principal.UserID }
func (client *sessionClient) SupportsPresence() bool { return true }
func (client *sessionClient) SupportsCursor() bool   { return true }
func (client *sessionClient) DisplayName() string    { return client.principal.Username }

func (client *sessionClient) DerivesCursorPositions() bool { return client.localCursors }

func (client *sessionClient) TrySend(message protocol.Message) bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closed {
		return false
	}
	if message.Cursor != nil && (message.Type == protocol.TypeCursorChanged || message.Type == protocol.TypeCursorRemoved) {
		client.queueCursor(message)
		return true
	}
	select {
	case client.send <- message:
		// 可靠编辑可能越过排队中的主动移动。把尚未发送的快照也推进到同一版本，
		// 否则新客户端将再也收不到那次移动。
		if client.localCursors && message.Type == protocol.TypeEditApplied && message.Operation != nil {
			for id, cursor := range client.pendingCursors {
				if cursor.kind == protocol.TypeCursorChanged && cursor.state.Version+1 == message.Version {
					state := cursor.state.AfterEdit(*message.Operation, message.Version)
					cursor.state = &state
					client.pendingCursors[id] = cursor
				}
			}
		}
		return true
	default:
		return false
	}
}

type pendingCursor struct {
	kind  string
	docID string
	state *protocol.CursorState
}

// 同一用户只保留最后一个光标状态（包括离开），不同用户按首次待发送顺序处理。
// Room 在广播后不会修改该光标快照，所有接收方可以共享它。
func (client *sessionClient) queueCursor(message protocol.Message) {
	if client.pendingCursors == nil {
		client.pendingCursors = make(map[string]pendingCursor)
	}
	userID := message.Cursor.UserID
	if _, pending := client.pendingCursors[userID]; !pending {
		client.cursorOrder = append(client.cursorOrder, userID)
	}
	client.pendingCursors[userID] = pendingCursor{kind: message.Type, docID: message.DocID, state: message.Cursor}
	if len(client.cursorOrder) == 1 {
		select {
		case client.cursorReady <- struct{}{}:
		default:
		}
	}
}

func (client *sessionClient) Close(reason error) {
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		return
	}
	client.closed = true
	close(client.send)
	client.mu.Unlock()
	_ = reason
	_ = client.connection.CloseNow()
}

func (client *sessionClient) run(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go client.writeLoop(ctx)
	defer func() {
		if client.joined && client.room != nil {
			leaveCtx, leaveCancel := context.WithTimeout(context.Background(), client.config.OperationTimeout)
			_ = client.room.LeaveContext(leaveCtx, client)
			leaveCancel()
		}
		if client.unregister != nil {
			client.unregister()
		}
		client.finish()
	}()
	if !client.authenticate(ctx) {
		return
	}
	for {
		var message protocol.Message
		if err := client.connection.Read(ctx, &message); err != nil || !client.process(ctx, message) {
			return
		}
	}
}

// finish drains messages already accepted by the session before closing the
// transport. This preserves terminal protocol errors while Close remains the
// immediate path used for revocation, slow clients, and transport failures.
func (client *sessionClient) finish() {
	client.mu.Lock()
	if !client.closed {
		client.closed = true
		close(client.send)
	}
	client.mu.Unlock()
	<-client.writeDone
	_ = client.connection.CloseNow()
}

func (client *sessionClient) authenticate(parent context.Context) bool {
	ctx, cancel := context.WithTimeout(parent, client.config.AuthTimeout)
	defer cancel()
	var message protocol.Message
	if err := client.connection.Read(ctx, &message); err != nil || message.Type != protocol.TypeAuth || message.Token == "" {
		client.sendError(protocol.CodeBadMessage, "AUTH with a bearer token is required")
		return false
	}
	principal, err := client.config.Authenticator.AuthenticateToken(ctx, message.Token)
	if err != nil || !principal.Trusted {
		client.sendError(protocol.CodePermissionDenied, "invalid or inactive session")
		return false
	}
	client.principal = principal
	client.localCursors = message.LocalCursorTransform
	if client.config.ConnectionRegistry != nil {
		client.unregister = client.config.ConnectionRegistry.Register(principal.SessionID, client, func() { client.Close(identity.ErrSessionInvalid) })
	}
	return client.TrySend(protocol.Message{Type: protocol.TypeAuthOK, LocalCursorTransform: client.localCursors})
}

func (client *sessionClient) process(parent context.Context, message protocol.Message) bool {
	ctx, cancel := context.WithTimeout(parent, client.config.OperationTimeout)
	defer cancel()
	release := func() {}
	var err error
	if guarded, ok := client.config.Authenticator.(authentication.PrincipalUseAuthenticator); ok {
		release, err = guarded.AcquirePrincipal(ctx, client.principal)
	} else {
		err = client.config.Authenticator.ValidatePrincipal(ctx, client.principal)
	}
	if err != nil {
		client.sendError(protocol.CodePermissionDenied, "session is not active")
		return false
	}
	defer release()
	switch message.Type {
	case protocol.TypeJoin, protocol.TypeResume:
		return client.selectDocument(ctx, message)
	case protocol.TypeEdit:
		if !client.joined || message.DocID != client.docID || message.RequestID == "" || message.Operation == nil {
			client.sendError(protocol.CodeBadMessage, "joined document, request_id, and operation are required")
			return true
		}
		operation := *message.Operation
		operation.BaseVersion = message.BaseVersion
		_ = client.room.SubmitEditContext(ctx, client, message.RequestID, operation)
	case protocol.TypeCursorUpdate:
		client.processCursor(ctx, message)
	default:
		client.sendError(protocol.CodeBadMessage, "unsupported message type")
	}
	return true
}

func (client *sessionClient) selectDocument(ctx context.Context, message protocol.Message) bool {
	if client.joined || message.DocID == "" {
		client.sendError(protocol.CodeBadMessage, "a valid document may only be selected once")
		return true
	}
	resolver := client.config.OwnerResolver
	if resolver != nil {
		owner, err := resolver.EnsureOwner(ctx, message.DocID)
		if err != nil {
			client.sendError(protocol.CodeServiceUnavailable, "document owner is unavailable")
			return false
		}
		// A cold document has a preferred node before it has an active owner.
		switch owner.State {
		case cluster.OwnerAcquiring, cluster.OwnerRecovering, cluster.OwnerActive:
		default:
			client.sendError(protocol.CodeServiceUnavailable, "document owner is unavailable")
			return false
		}
		if !client.isLocalOwner(owner) {
			return client.redirectToOwner(ctx, message.DocID, owner)
		}
	}
	target, err := client.config.Manager.RoomContext(ctx, message.DocID)
	if err != nil {
		client.sendRoomError(err, "could not open document")
		return false
	}
	if resolver != nil {
		owner, err := resolver.Resolve(ctx, message.DocID)
		if err != nil || owner.State != cluster.OwnerActive || !client.isLocalOwner(owner) {
			client.sendError(protocol.CodeServiceUnavailable, "document owner is unavailable")
			return false
		}
	}
	client.room, client.docID = target, message.DocID
	if message.Type == protocol.TypeJoin {
		err = target.JoinContext(ctx, client)
	} else {
		err = target.ResumeContext(ctx, client, message.LastVersion)
	}
	if err != nil {
		client.sendRoomError(err, "could not join document")
		return !errors.Is(err, room.ErrSlowClient)
	}
	client.joined = true
	return true
}

func (client *sessionClient) isLocalOwner(owner cluster.Owner) bool {
	return owner.NodeID == client.config.LocalNode.NodeID && owner.Incarnation == client.config.LocalNode.Incarnation
}

func (client *sessionClient) redirectToOwner(ctx context.Context, docID string, owner cluster.Owner) bool {
	if client.config.NodeDirectory == nil {
		client.sendError(protocol.CodeServiceUnavailable, "document owner is unavailable")
		return false
	}
	node, found := client.config.NodeDirectory.Lookup(owner.NodeID, owner.Incarnation)
	if !found || !node.IsReady() || node.BrowserEndpoint == "" {
		client.sendError(protocol.CodeServiceUnavailable, "document owner is unavailable")
		return false
	}
	slog.Info("browser session redirected to document owner",
		"component", "browser_transport",
		"event", "owner_redirect",
		"doc_id", docID,
		"from_node_id", client.config.LocalNode.NodeID,
		"owner_node_id", owner.NodeID,
		"owner_incarnation", owner.Incarnation,
		"browser_endpoint", node.BrowserEndpoint,
	)
	writeCtx, cancel := context.WithTimeout(ctx, client.config.WriteTimeout)
	err := client.connection.Write(writeCtx, protocol.Message{Type: protocol.TypeOwnerRedirect, DocID: docID, Endpoint: node.BrowserEndpoint})
	cancel()
	if err != nil {
		client.Close(err)
	}
	return false
}

func (client *sessionClient) processCursor(ctx context.Context, message protocol.Message) {
	if !client.joined || message.DocID != client.docID || message.Cursor == nil {
		client.sendError(protocol.CodeBadMessage, "joined document and cursor are required")
		return
	}
	if !client.allowCursorMessage() {
		return
	}
	if err := client.room.SubmitCursorContext(ctx, client, *message.Cursor); err != nil {
		var coded *room.CodedError
		if errors.As(err, &coded) && coded.Code == protocol.CodeBadMessage {
			return
		}
		client.sendRoomError(err, "invalid cursor update")
	}
}

func (client *sessionClient) allowCursorMessage() bool {
	now := time.Now()
	if client.cursorWindow.IsZero() || now.Sub(client.cursorWindow) >= time.Second {
		client.cursorWindow = now
		client.cursorMessages = 0
	}
	client.cursorMessages++
	return client.cursorMessages <= cursorMessagesPerSecond
}

// 唯一的写协程先取可靠消息，再取光标。取消息时与入队共用锁，
// 避免某个新版本的光标抢在对应的 edit_applied 前面发出。
func (client *sessionClient) nextMessage(ctx context.Context) (protocol.Message, bool) {
	for ctx.Err() == nil {
		client.mu.Lock()
		select {
		case message, ok := <-client.send:
			client.mu.Unlock()
			return message, ok
		default:
		}
		if len(client.cursorOrder) > 0 {
			userID := client.cursorOrder[0]
			client.cursorOrder[0] = ""
			client.cursorOrder = client.cursorOrder[1:]
			cursor := client.pendingCursors[userID]
			delete(client.pendingCursors, userID)
			client.mu.Unlock()
			return protocol.Message{Type: cursor.kind, DocID: cursor.docID, Cursor: cursor.state}, true
		}
		client.mu.Unlock()
		select {
		case message, ok := <-client.send:
			return message, ok
		case <-client.cursorReady:
		case <-ctx.Done():
			return protocol.Message{}, false
		}
	}
	return protocol.Message{}, false
}

func (client *sessionClient) writeLoop(ctx context.Context) {
	defer close(client.writeDone)
	var editVersion uint64
	for {
		message, ok := client.nextMessage(ctx)
		if !ok {
			return
		}
		// 编辑可能越过尚未发出的旧坐标。Room 会随后发布该用户的新坐标或离开通知。
		if message.Type == protocol.TypeCursorChanged && message.Cursor != nil && message.Cursor.Version < editVersion {
			continue
		}
		if message.Type == protocol.TypeEditApplied && message.Version > editVersion {
			editVersion = message.Version
		}
		writeCtx, cancel := context.WithTimeout(ctx, client.config.WriteTimeout)
		err := client.connection.Write(writeCtx, message)
		cancel()
		if err != nil {
			client.Close(err)
			return
		}
	}
}

func (client *sessionClient) sendRoomError(err error, fallback string) {
	var coded *room.CodedError
	if errors.As(err, &coded) {
		client.sendError(coded.Code, coded.Message)
	} else if !errors.Is(err, room.ErrSlowClient) {
		client.sendError(protocol.CodeInternalError, fallback)
	}
}

func (client *sessionClient) sendError(code, message string) {
	if !client.TrySend(protocol.Error(code, message)) {
		client.Close(room.ErrSlowClient)
	}
}
