package browser

import (
	"context"
	"errors"
	"io"
	"strconv"
	"sync"
	"testing"
	"time"

	memory "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/adapter/memory/documents"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/app/room"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/cluster"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/collab"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/protocol"
)

func TestFinishDrainsTerminalMessageBeforeClosingConnection(t *testing.T) {
	connection := &recordingMessageConn{}
	client := &sessionClient{
		connection: connection,
		send:       make(chan protocol.Message, 1),
		writeDone:  make(chan struct{}),
	}
	go client.writeLoop(context.Background())
	if !client.TrySend(protocol.Error(protocol.CodePermissionDenied, "invalid session")) {
		t.Fatal("terminal error was not queued")
	}

	client.finish()

	connection.mu.Lock()
	defer connection.mu.Unlock()
	if len(connection.messages) != 1 || connection.messages[0].Code != protocol.CodePermissionDenied {
		t.Fatalf("written messages = %#v", connection.messages)
	}
	if !connection.closed {
		t.Fatal("connection remained open after writer drained")
	}
}

func TestConcurrentCloseStopsBrowserWriter(t *testing.T) {
	connection := &recordingMessageConn{}
	client := &sessionClient{
		connection: connection,
		send:       make(chan protocol.Message, 1),
		writeDone:  make(chan struct{}),
		config:     Config{WriteTimeout: time.Second},
	}
	go client.writeLoop(context.Background())
	var callers sync.WaitGroup
	for range 16 {
		callers.Add(1)
		go func() {
			defer callers.Done()
			client.Close(nil)
		}()
	}
	callers.Wait()
	select {
	case <-client.writeDone:
	case <-time.After(time.Second):
		t.Fatal("browser writer survived concurrent Close")
	}
	if client.TrySend(protocol.Message{Type: protocol.TypeEditApplied}) {
		t.Fatal("closed browser session accepted a message")
	}
	connection.mu.Lock()
	closed := connection.closed
	connection.mu.Unlock()
	if !closed {
		t.Fatal("transport remained open")
	}
}

type recordingMessageConn struct {
	mu       sync.Mutex
	messages []protocol.Message
	closed   bool
}

func (connection *recordingMessageConn) Read(context.Context, *protocol.Message) error {
	return io.EOF
}

func (connection *recordingMessageConn) Write(_ context.Context, message protocol.Message) error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if connection.closed {
		return io.ErrClosedPipe
	}
	connection.messages = append(connection.messages, message)
	return nil
}

func (connection *recordingMessageConn) CloseNow() error {
	connection.mu.Lock()
	connection.closed = true
	connection.mu.Unlock()
	return nil
}

type changingOwner struct {
	candidate cluster.Owner
	resolved  cluster.Owner
	err       error
}

func (r changingOwner) EnsureOwner(context.Context, string) (cluster.Owner, error) {
	return r.candidate, nil
}
func (r changingOwner) Resolve(context.Context, string) (cluster.Owner, error) {
	return r.resolved, r.err
}

func TestColdRoomRequiresActiveLocalOwnerAfterRecovery(t *testing.T) {
	ctx := context.Background()
	store := memory.NewMemory()
	if _, err := store.CreateDocument(ctx, "doc", "private", "alice"); err != nil {
		t.Fatal(err)
	}
	manager := room.NewManager(store)
	t.Cleanup(func() { _ = manager.Shutdown(ctx) })
	local := cluster.Node{NodeID: "node", Incarnation: "first"}
	candidate := cluster.Owner{NodeID: local.NodeID, Incarnation: local.Incarnation, State: cluster.OwnerAcquiring}
	for _, tt := range []struct {
		name     string
		resolver changingOwner
	}{
		{"still recovering", changingOwner{candidate: candidate, resolved: cluster.Owner{NodeID: "node", Incarnation: "first", State: cluster.OwnerRecovering}}},
		{"new incarnation", changingOwner{candidate: candidate, resolved: cluster.Owner{NodeID: "node", Incarnation: "second", State: cluster.OwnerActive}}},
		{"ownership lost", changingOwner{candidate: candidate, err: errors.New("owner unavailable")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := sessionClient{config: Config{Manager: manager, LocalNode: local, OwnerResolver: tt.resolver}, send: make(chan protocol.Message, 1)}
			if client.selectDocument(ctx, protocol.Message{Type: protocol.TypeJoin, DocID: "doc"}) || client.joined || client.room != nil {
				t.Fatal("accepted selection without active local ownership")
			}
			select {
			case m := <-client.send:
				if m.Code != protocol.CodeServiceUnavailable {
					t.Fatalf("unexpected response %#v", m)
				}
			default:
				t.Fatal("missing selection error")
			}
		})
	}
}

func TestCursorBurstKeepsReliableQueueAvailable(t *testing.T) {
	client := &sessionClient{send: make(chan protocol.Message, 2), cursorReady: make(chan struct{}, 1)}
	const peers = 128
	queue := func(kind, user string, version uint64) {
		t.Helper()
		if !client.TrySend(protocol.Message{Type: kind, DocID: "doc", Cursor: &protocol.CursorState{UserID: user, Version: version}}) {
			t.Fatalf("could not enqueue %s for %s", kind, user)
		}
	}
	for i := 0; i < peers; i++ {
		queue(protocol.TypeCursorChanged, strconv.Itoa(i), 1)
	}
	if !client.TrySend(protocol.Message{Type: protocol.TypeEditApplied, Version: 2}) ||
		!client.TrySend(protocol.Message{Type: protocol.TypePermissionChanged}) {
		t.Fatal("cursor burst displaced reliable messages")
	}
	if client.TrySend(protocol.Message{Type: protocol.TypeEditApplied}) {
		t.Fatal("full reliable queue accepted another edit")
	}
	for i := 0; i < peers; i++ {
		queue(protocol.TypeCursorChanged, strconv.Itoa(i), 2)
	}
	queue(protocol.TypeCursorRemoved, "0", 0)
	queue(protocol.TypeCursorRemoved, "1", 0)
	queue(protocol.TypeCursorChanged, "1", 2)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, want := range []string{protocol.TypeEditApplied, protocol.TypePermissionChanged} {
		if msg, ok := client.nextMessage(ctx); !ok || msg.Type != want {
			t.Fatalf("expected reliable message %s, got %s", want, msg.Type)
		}
	}
	for i := 0; i < peers; i++ {
		msg, ok := client.nextMessage(ctx)
		if !ok || msg.DocID != "doc" || msg.Cursor == nil || msg.Cursor.UserID != strconv.Itoa(i) {
			t.Fatalf("unexpected cursor for peer %d: %#v", i, msg)
		}
		if i == 0 {
			if msg.Type != protocol.TypeCursorRemoved {
				t.Fatal("departed cursor survived")
			}
		} else if msg.Type != protocol.TypeCursorChanged || msg.Cursor.Version != 2 {
			t.Fatalf("stale cursor for peer %d", i)
		}
	}
	close(client.send)
	if _, ok := client.nextMessage(ctx); ok {
		t.Fatal("unexpected extra cursor")
	}
}

func TestPresenceBurstKeepsReliableQueueAvailable(t *testing.T) {
	client := &sessionClient{send: make(chan protocol.Message, 3), presenceReady: make(chan struct{}, 1)}
	for _, kind := range []string{protocol.TypeJoined, protocol.TypeEditApplied, protocol.TypePermissionChanged} {
		if !client.TrySend(protocol.Message{Type: kind}) {
			t.Fatalf("could not queue reliable message %s", kind)
		}
	}
	for index := 0; index < 240; index++ {
		if !client.TrySend(protocol.Message{Type: protocol.TypePresenceChanged, UserIDs: []string{strconv.Itoa(index)}}) {
			t.Fatalf("presence update %d filled the reliable queue", index)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, kind := range []string{protocol.TypeJoined, protocol.TypeEditApplied, protocol.TypePermissionChanged} {
		message, ok := client.nextMessage(ctx)
		if !ok || message.Type != kind {
			t.Fatalf("reliable message = %#v, want %s", message, kind)
		}
	}
	message, ok := client.nextMessage(ctx)
	if !ok || message.Type != protocol.TypePresenceChanged || len(message.UserIDs) != 1 || message.UserIDs[0] != "239" {
		t.Fatalf("latest presence = %#v", message)
	}
	close(client.send)
	if _, ok := client.nextMessage(ctx); ok {
		t.Fatal("outdated presence remained queued")
	}
}

func TestWriterSkipsCursorOvertakenByEdit(t *testing.T) {
	conn := &recordingMessageConn{}
	client := &sessionClient{connection: conn, send: make(chan protocol.Message, 2), cursorReady: make(chan struct{}, 1), writeDone: make(chan struct{}), config: Config{WriteTimeout: time.Second}}
	client.TrySend(protocol.Message{Type: protocol.TypeCursorChanged, Cursor: &protocol.CursorState{UserID: "alice", Version: 1}})
	client.TrySend(protocol.Message{Type: protocol.TypeEditApplied, Version: 2})
	client.TrySend(protocol.Message{Type: protocol.TypeCursorRemoved, Cursor: &protocol.CursorState{UserID: "bob"}})
	// 结束标记排在光标之后；看到它就能确定前面的光标都已处理。
	client.TrySend(protocol.Message{Type: protocol.TypeCursorChanged, Cursor: &protocol.CursorState{UserID: "carol", Version: 2}})
	got := make(chan protocol.Message, 4)
	client.connection = &observedWrites{recordingMessageConn: conn, writes: got}
	go client.writeLoop(context.Background())
	defer client.finish()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, want := range []string{protocol.TypeEditApplied, protocol.TypeCursorRemoved, protocol.TypeCursorChanged} {
		select {
		case m := <-got:
			if m.Type != want || (m.Type == protocol.TypeCursorChanged && m.Cursor.UserID != "carol") {
				t.Fatalf("unexpected message %#v", m)
			}
		case <-ctx.Done():
			t.Fatal("writer failed to drain current cursor state")
		}
	}
}

type observedWrites struct {
	*recordingMessageConn
	writes chan protocol.Message
}

func (c *observedWrites) Write(ctx context.Context, m protocol.Message) error {
	select {
	case c.writes <- m:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestQueuedCursorTracksEditWithoutMutatingSharedSnapshot(t *testing.T) {
	client := sessionClient{localCursors: true, send: make(chan protocol.Message, 1), cursorReady: make(chan struct{}, 1)}
	original := protocol.CursorState{UserID: "alice", Anchor: 4, Head: 2, Version: 7}
	client.TrySend(protocol.Message{Type: protocol.TypeCursorChanged, Cursor: &original})
	op := collab.Operation{Kind: collab.OperationInsert, Position: 2, Text: "😀"}
	if !client.TrySend(protocol.Message{Type: protocol.TypeEditApplied, Operation: &op, Version: 8}) {
		t.Fatal("edit not enqueued")
	}
	got := client.pendingCursors["alice"].state
	if got.Version != 8 || got.Anchor != 5 || got.Head != 3 || original.Version != 7 || original.Anchor != 4 || original.Head != 2 {
		t.Fatalf("queued=%+v shared=%+v", got, original)
	}
}
