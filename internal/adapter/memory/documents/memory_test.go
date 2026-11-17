package memory

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/collab"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/permission"
	storage "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/storage"
)

func TestMemoryCreateDocumentAtomicallyCreatesOwner(t *testing.T) {
	store := NewMemory()
	if _, err := store.CreateDocument(context.Background(), "doc", "abc", "owner"); err != nil {
		t.Fatal(err)
	}
	role, err := store.GetPermission(context.Background(), "doc", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if role != permission.RoleOwner {
		t.Fatalf("owner role = %q", role)
	}
	snapshot, err := store.GetLatestSnapshot(context.Background(), "doc")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Version != 0 || snapshot.Content != "abc" {
		t.Fatalf("initial snapshot = %#v", snapshot)
	}
}

func TestMemoryRecoveryLoadsOnlyOperationsAfterSnapshot(t *testing.T) {
	store := NewMemory()
	_, _ = store.CreateDocument(context.Background(), "doc", "", "owner")
	activateMemoryAuthority(t, store, "doc", storage.LocalAuthority)
	if err := store.CommitEdit(context.Background(), testCommit("doc", "one", 0, "A")); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveSnapshot(context.Background(), storage.SnapshotCommit{Authority: storage.LocalAuthority, Snapshot: storage.Snapshot{DocumentID: "doc", Version: 1, Content: "A"}}); err != nil {
		t.Fatal(err)
	}
	second := testCommit("doc", "two", 1, "BA")
	second.Operation.Position = 0
	second.Operation.Text = "B"
	if err := store.CommitEdit(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	state, err := store.LoadRecoveryState(context.Background(), "doc", storage.LocalAuthority)
	if err != nil {
		t.Fatal(err)
	}
	if state.Snapshot.Version != 1 || len(state.Operations) != 1 || state.Operations[0].Version != 2 {
		t.Fatalf("recovery state = %#v", state)
	}
	if err := storage.ValidateRecoveryState(state); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryCommitEditIsAtomicAndIdempotent(t *testing.T) {
	store := NewMemory()
	if _, err := store.CreateDocument(context.Background(), "doc", "abc", "owner"); err != nil {
		t.Fatal(err)
	}
	activateMemoryAuthority(t, store, "doc", storage.LocalAuthority)
	commit := testCommit("doc", "request-1", 0, "Xabc")
	if err := store.CommitEdit(context.Background(), commit); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitEdit(context.Background(), commit); !errors.Is(err, storage.ErrDuplicateRequest) {
		t.Fatalf("duplicate commit error = %v", err)
	}
	document, err := store.GetDocument(context.Background(), "doc")
	if err != nil {
		t.Fatal(err)
	}
	if document.Content != "Xabc" || document.Version != 1 {
		t.Fatalf("document = %#v", document)
	}
	operations, err := store.LoadOperations(context.Background(), "doc")
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 {
		t.Fatalf("operation count = %d", len(operations))
	}
}

func TestMemoryConcurrentVersionUpdateDoesNotOverwrite(t *testing.T) {
	store := NewMemory()
	if _, err := store.CreateDocument(context.Background(), "doc", "", "owner"); err != nil {
		t.Fatal(err)
	}
	activateMemoryAuthority(t, store, "doc", storage.LocalAuthority)
	commits := []storage.EditCommit{
		testCommit("doc", "request-a", 0, "A"),
		testCommit("doc", "request-b", 0, "B"),
	}
	results := make(chan error, len(commits))
	var wait sync.WaitGroup
	for _, commit := range commits {
		wait.Add(1)
		go func(commit storage.EditCommit) {
			defer wait.Done()
			results <- store.CommitEdit(context.Background(), commit)
		}(commit)
	}
	wait.Wait()
	close(results)
	var successes, conflicts int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, storage.ErrVersionConflict):
			conflicts++
		default:
			t.Fatalf("unexpected commit error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
}

func TestMemoryPermissionRequiresPersistedOwner(t *testing.T) {
	store := NewMemory()
	if _, err := store.CreateDocument(context.Background(), "doc", "", "owner"); err != nil {
		t.Fatal(err)
	}
	activateMemoryAuthority(t, store, "doc", storage.LocalAuthority)
	if err := store.SetPermission(context.Background(), storage.PermissionCommit{Authority: storage.LocalAuthority, DocumentID: "doc", ActorID: "writer", UserID: "reader", Role: permission.RoleRead}); !errors.Is(err, storage.ErrPermissionDenied) {
		t.Fatalf("writer permission error = %v", err)
	}
	if err := store.SetPermission(context.Background(), storage.PermissionCommit{Authority: storage.LocalAuthority, DocumentID: "doc", ActorID: "owner", UserID: "writer", Role: permission.RoleWrite}); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryOwnershipActivationAndFencing(t *testing.T) {
	store := NewMemory()
	if _, err := store.CreateDocument(context.Background(), "doc", "", "owner"); err != nil {
		t.Fatal(err)
	}
	nodeA := storage.WriteAuthority{NodeID: "node-a", FencingToken: 1}
	nodeB := storage.WriteAuthority{NodeID: "node-b", FencingToken: 2}
	current, err := store.GetWriteAuthority(context.Background(), "doc")
	if err != nil {
		t.Fatal(err)
	}
	if !current.IsZero() {
		t.Fatalf("new document authority = %#v, want unowned", current)
	}
	unownedEdit := testCommit("doc", "before-activation", 0, "A")
	unownedEdit.Authority = nodeA
	if err := store.CommitEdit(context.Background(), unownedEdit); !errors.Is(err, storage.ErrFenced) {
		t.Fatalf("edit before activation = %v", err)
	}
	if err := store.ActivateOwnership(context.Background(), "doc", nodeA); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateOwnership(context.Background(), "doc", nodeA); !errors.Is(err, storage.ErrOwnershipConflict) {
		t.Fatalf("same token activation = %v", err)
	}
	if err := store.ActivateOwnership(context.Background(), "doc", storage.WriteAuthority{NodeID: "node-b", FencingToken: 1}); !errors.Is(err, storage.ErrOwnershipConflict) {
		t.Fatalf("same token different node activation = %v", err)
	}
	if err := store.ActivateOwnership(context.Background(), "doc", nodeB); err != nil {
		t.Fatal(err)
	}
	if err := store.ActivateOwnership(context.Background(), "doc", nodeA); !errors.Is(err, storage.ErrOwnershipConflict) {
		t.Fatalf("old token activation = %v", err)
	}

	oldEdit := testCommit("doc", "old", 0, "A")
	oldEdit.Authority = nodeA
	if err := store.CommitEdit(context.Background(), oldEdit); !errors.Is(err, storage.ErrFenced) {
		t.Fatalf("old edit = %v", err)
	}
	if err := store.SetPermission(context.Background(), storage.PermissionCommit{Authority: nodeA, DocumentID: "doc", ActorID: "owner", UserID: "writer", Role: permission.RoleWrite}); !errors.Is(err, storage.ErrFenced) {
		t.Fatalf("old permission = %v", err)
	}
	if err := store.SaveSnapshot(context.Background(), storage.SnapshotCommit{Authority: nodeA, Snapshot: storage.Snapshot{DocumentID: "doc", Version: 0}}); !errors.Is(err, storage.ErrFenced) {
		t.Fatalf("old snapshot = %v", err)
	}
}

func TestMemoryConcurrentOwnershipActivationHasOneWinner(t *testing.T) {
	store := NewMemory()
	if _, err := store.CreateDocument(context.Background(), "doc", "", "owner"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, nodeID := range []string{"node-a", "node-b"} {
		wait.Add(1)
		go func(nodeID string) {
			defer wait.Done()
			<-start
			results <- store.ActivateOwnership(context.Background(), "doc", storage.WriteAuthority{NodeID: nodeID, FencingToken: 1})
		}(nodeID)
	}
	close(start)
	wait.Wait()
	close(results)
	var successes, conflicts int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, storage.ErrOwnershipConflict):
			conflicts++
		default:
			t.Fatalf("activation error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
}

func testCommit(docID, requestID string, expectedVersion uint64, content string) storage.EditCommit {
	nextVersion := expectedVersion + 1
	operation := collab.Operation{
		ID: requestID, UserID: "owner", BaseVersion: expectedVersion,
		Kind: collab.OperationInsert, Position: 0, Text: content,
	}
	return storage.EditCommit{
		Authority:       storage.LocalAuthority,
		ExpectedVersion: expectedVersion,
		Document:        collab.Document{ID: docID, Content: content, Version: nextVersion},
		Operation:       collab.AppliedOperation{Operation: operation, Version: nextVersion},
	}
}

func activateMemoryAuthority(t *testing.T, store *Memory, docID string, authority storage.WriteAuthority) {
	t.Helper()
	if err := store.ActivateOwnership(context.Background(), docID, authority); err != nil {
		t.Fatal(err)
	}
}
