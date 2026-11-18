package room

import (
	"context"
	"testing"
	"time"

	memory "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/adapter/memory/documents"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/collab"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/permission"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/storage"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/protocol"
)

type cursorTestClient struct {
	*Client
	username string
}

func (client *cursorTestClient) SupportsPresence() bool { return true }
func (client *cursorTestClient) SupportsCursor() bool   { return true }
func (client *cursorTestClient) DisplayName() string    { return client.username }

func TestCursorStateTransformsWithEditsWithoutChangingDocumentVersion(t *testing.T) {
	store := memory.NewMemory()
	if _, err := store.CreateDocument(context.Background(), "doc", "abc", "owner"); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateOwnership(context.Background(), "doc", storage.LocalAuthority); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPermission(context.Background(), storage.PermissionCommit{
		Authority: storage.LocalAuthority, DocumentID: "doc", ActorID: "owner", UserID: "peer", Role: permission.RoleWrite,
	}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(store)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	target, err := manager.Room("doc")
	if err != nil {
		t.Fatal(err)
	}
	owner := &cursorTestClient{Client: &Client{UserID: "owner", Send: make(chan protocol.Message, 32)}, username: "alice"}
	peer := &cursorTestClient{Client: &Client{UserID: "peer", Send: make(chan protocol.Message, 32)}, username: "bob"}
	if err := target.Join(owner); err != nil {
		t.Fatal(err)
	}
	if err := target.Join(peer); err != nil {
		t.Fatal(err)
	}
	drainMessages(owner.Send)
	drainMessages(peer.Send)

	if err := target.SubmitCursorContext(context.Background(), owner, protocol.CursorState{Anchor: 1, Head: 2, Version: 0, Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	changed := waitForMessageType(t, peer.Send, protocol.TypeCursorChanged)
	if changed.Cursor == nil || changed.Cursor.UserID != "owner" || changed.Cursor.Username != "alice" || changed.Cursor.Anchor != 1 || changed.Cursor.Head != 2 {
		t.Fatalf("cursor changed = %#v", changed)
	}
	if snapshot := target.Snapshot(); snapshot.Version != 0 || snapshot.Content != "abc" {
		t.Fatalf("cursor changed document = %#v", snapshot)
	}

	operation := collab.Operation{Kind: collab.OperationInsert, Position: 0, Text: "X", BaseVersion: 0}
	if err := target.SubmitEditContext(context.Background(), peer, "edit-1", operation); err != nil {
		t.Fatal(err)
	}
	_ = waitForMessageType(t, peer.Send, protocol.TypeEditApplied)
	transformed := waitForMessageType(t, peer.Send, protocol.TypeCursorChanged)
	if transformed.Cursor == nil || transformed.Cursor.Anchor != 2 || transformed.Cursor.Head != 3 || transformed.Cursor.Version != 1 {
		t.Fatalf("transformed cursor = %#v", transformed)
	}
	if snapshot := target.Snapshot(); snapshot.Version != 1 || snapshot.Content != "Xabc" {
		t.Fatalf("edit result = %#v", snapshot)
	}

	if err := target.LeaveContext(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	removed := waitForMessageType(t, peer.Send, protocol.TypeCursorRemoved)
	if removed.Cursor == nil || removed.Cursor.UserID != "owner" {
		t.Fatalf("cursor removed = %#v", removed)
	}
}

func TestCursorRejectsInvalidVersionAndIgnoresOldSequence(t *testing.T) {
	store := memory.NewMemory()
	_, _ = store.CreateDocument(context.Background(), "doc", "abc", "owner")
	_ = store.ActivateOwnership(context.Background(), "doc", storage.LocalAuthority)
	_ = store.SetPermission(context.Background(), storage.PermissionCommit{
		Authority: storage.LocalAuthority, DocumentID: "doc", ActorID: "owner", UserID: "peer", Role: permission.RoleWrite,
	})
	manager := NewManager(store)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	target, _ := manager.Room("doc")
	owner := &cursorTestClient{Client: &Client{UserID: "owner", Send: make(chan protocol.Message, 16)}, username: "alice"}
	peer := &cursorTestClient{Client: &Client{UserID: "peer", Send: make(chan protocol.Message, 16)}, username: "bob"}
	if err := target.Join(owner); err != nil {
		t.Fatal(err)
	}
	if err := target.Join(peer); err != nil {
		t.Fatal(err)
	}
	drainMessages(owner.Send)
	drainMessages(peer.Send)
	if err := target.SubmitCursorContext(context.Background(), owner, protocol.CursorState{Anchor: 1, Head: 1, Version: 0, Sequence: 2}); err != nil {
		t.Fatal(err)
	}
	first := waitForMessageType(t, peer.Send, protocol.TypeCursorChanged)
	if first.Cursor == nil || first.Cursor.Anchor != 1 || first.Cursor.Sequence != 2 {
		t.Fatalf("first cursor = %#v", first)
	}
	if err := target.SubmitCursorContext(context.Background(), owner, protocol.CursorState{Anchor: 3, Head: 3, Version: 0, Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-peer.Send:
		t.Fatalf("old sequence produced message: %#v", message)
	case <-time.After(20 * time.Millisecond):
	}
	if err := target.SubmitCursorContext(context.Background(), owner, protocol.CursorState{Anchor: 4, Head: 4, Version: 0, Sequence: 3}); err == nil {
		t.Fatal("out-of-range cursor was accepted")
	}
	if err := target.SubmitCursorContext(context.Background(), owner, protocol.CursorState{Anchor: 1, Head: 1, Version: 9, Sequence: 4}); err == nil {
		t.Fatal("future cursor version was accepted")
	}
}

func drainMessages(messages <-chan protocol.Message) {
	for {
		select {
		case <-messages:
		default:
			return
		}
	}
}

func waitForMessageType(t *testing.T, messages <-chan protocol.Message, messageType string) protocol.Message {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case message := <-messages:
			if message.Type == messageType {
				return message
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", messageType)
		}
	}
}

func TestCursorBoundsFollowCommittedUnicodeEdits(t *testing.T) {
	target, editor := newTestRoom(t, "甲🙂乙")
	cursor := &cursorTestClient{Client: &Client{UserID: "owner", Send: make(chan protocol.Message, 32)}}
	if err := target.Join(cursor); err != nil {
		t.Fatal(err)
	}
	var sequence uint64
	checkEnd := func(version uint64, end int) {
		t.Helper()
		sequence++
		position := protocol.CursorState{Version: version, Anchor: end, Head: end, Sequence: sequence}
		if err := target.SubmitCursorContext(context.Background(), cursor, position); err != nil {
			t.Fatalf("valid end at version %d: %v", version, err)
		}
		position.Sequence++
		sequence++
		position.Head++
		assertCode(t, target.SubmitCursorContext(context.Background(), cursor, position), protocol.CodeBadMessage)
	}
	checkEnd(0, 3)
	insert := collab.Operation{Kind: collab.OperationInsert, Position: 1, Text: "好🚀"}
	if err := target.SubmitEdit(editor, "insert", insert); err != nil {
		t.Fatal(err)
	}
	checkEnd(1, 5)

	// 旧版本删除的范围跨过新插入的两个字符，实际删除长度由 OT 决定。
	if err := target.SubmitEdit(editor, "delete", collab.Operation{BaseVersion: 0, Kind: collab.OperationDelete, Length: 2}); err != nil {
		t.Fatal(err)
	}
	checkEnd(2, 1)
}
