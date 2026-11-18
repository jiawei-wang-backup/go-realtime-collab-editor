package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	clusteretcd "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/adapter/etcd"
	memory "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/adapter/memory/documents"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/cluster"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/collab"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/permission"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/storage"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/protocol"
)

type recoveryCountingStore struct {
	*memory.Memory
	loads atomic.Int32
}

func (s *recoveryCountingStore) LoadRecoveryState(ctx context.Context, id string, authority storage.WriteAuthority) (storage.RecoveryState, error) {
	s.loads.Add(1)
	return s.Memory.LoadRecoveryState(ctx, id, authority)
}

func openColdRoomSocket(t *testing.T, client *http.Client, endpoint, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "wss"+strings.TrimPrefix(endpoint, "https")+"/api/realtime", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	writeBrowserMessage(t, conn, protocol.Message{Type: protocol.TypeAuth, Token: token})
	expectColdRoomResponse(t, conn, protocol.TypeAuthOK)
	return conn
}

// Read one protocol response, allowing the asynchronous participant list to interleave.
func expectColdRoomResponse(t *testing.T, conn *websocket.Conn, want string) protocol.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var msg protocol.Message
		if err := wsjson.Read(ctx, conn, &msg); err != nil {
			t.Fatal(err)
		}
		if msg.Type == protocol.TypePresenceChanged {
			continue
		}
		if msg.Type != want {
			t.Fatalf("want %s, got %s %s %s", want, msg.Type, msg.Code, msg.Message)
		}
		return msg
	}
}

func TestBrowserColdRoomAfterIdleEviction(t *testing.T) {
	deps := clusterTestDependencies{
		etcdEndpoints: os.Getenv("ETCD_TEST_ENDPOINTS"), etcdCA: os.Getenv("ETCD_TEST_CA_FILE"),
		etcdCert: os.Getenv("ETCD_TEST_CERT_FILE"), etcdKey: os.Getenv("ETCD_TEST_KEY_FILE"),
	}
	if deps.etcdEndpoints == "" {
		t.Skip("requires real etcd via ETCD_TEST_ENDPOINTS")
	}
	acquireClusterTestLock(t, deps)
	fixture := newAuthFixture(t)
	token := fixture.registerLogin(t, "cold-owner").AccessToken
	writerToken := fixture.registerLogin(t, "cold-writer").AccessToken
	ownerID := principalUserID(t, fixture.service, token)
	writerID := principalUserID(t, fixture.service, writerToken)
	store := &recoveryCountingStore{Memory: memory.NewMemory()}
	ctx := context.Background()

	type testNode struct {
		client    *http.Client
		node      cluster.Node
		app       *Server
		registry  *clusteretcd.Registry
		ownership *clusteretcd.Ownership
	}
	start := func(id string) testNode {
		server := httptest.NewUnstartedServer(nil)
		url := "https://" + server.Listener.Addr().String()
		node := clusterTestNode(id, "process", url)
		node.BrowserEndpoint = url
		client := openClusterTestEtcd(t, deps)
		t.Cleanup(func() { _ = client.Close() })
		registry := startClusterTestRegistry(t, client, node)
		t.Cleanup(func() { closeClusterTestRegistry(registry) })
		owner, err := clusteretcd.NewOwnership(registry)
		if err != nil {
			t.Fatal(err)
		}
		app := NewServerWithConfig(Config{
			Store: store, Authenticator: fixture.service, RoomIdleTimeout: 300 * time.Millisecond,
			OwnerResolver: owner, Ownership: owner, LocalNode: node, NodeDirectory: registry.Membership(),
		})
		server.Config.Handler = app
		server.StartTLS()
		t.Cleanup(func() { server.Close(); _ = app.Shutdown(ctx) })
		return testNode{server.Client(), node, app, registry, owner}
	}
	a, b := start("cold-a"), start("cold-b")
	waitForClusterTest(t, func() bool {
		na, ok := a.registry.Membership().Lookup(b.node.NodeID, b.node.Incarnation)
		nb, ok2 := b.registry.Membership().Lookup(a.node.NodeID, a.node.Incarnation)
		return ok && ok2 && na.IsReady() && nb.IsReady()
	})
	for _, kind := range []string{protocol.TypeJoin, protocol.TypeResume} {
		t.Run(kind, func(t *testing.T) {
			doc := documentOwnedBy(t, a.registry.Membership(), b.node)
			if _, err := store.CreateDocument(ctx, doc, "abc", ownerID); err != nil {
				t.Fatal(err)
			}
			// Give the writer access, record an edit, then wait for eviction to release ownership.
			target, err := b.app.manager.RoomContext(ctx, doc)
			if err != nil {
				t.Fatal(err)
			}
			if err := target.ChangePermissionContext(ctx, ownerID, writerID, permission.RoleWrite); err != nil {
				t.Fatal(err)
			}
			initial := openColdRoomSocket(t, b.client, b.node.BrowserEndpoint, token)
			writeBrowserMessage(t, initial, protocol.Message{Type: protocol.TypeJoin, DocID: doc})
			expectColdRoomResponse(t, initial, protocol.TypeJoined)
			edit := protocol.Message{Type: protocol.TypeEdit, DocID: doc, RequestID: "first", Operation: &collab.Operation{Kind: collab.OperationInsert, Text: "X"}}
			writeBrowserMessage(t, initial, edit)
			expectColdRoomResponse(t, initial, protocol.TypeEditApplied)
			initial.CloseNow()
			waitForClusterTest(t, func() bool { _, err := b.ownership.Resolve(ctx, doc); return errors.Is(err, cluster.ErrOwnerNotFound) })

			request := protocol.Message{Type: kind, DocID: doc}
			seed := openColdRoomSocket(t, a.client, a.node.BrowserEndpoint, token)
			writeBrowserMessage(t, seed, request)
			redirect := expectColdRoomResponse(t, seed, protocol.TypeOwnerRedirect)
			if redirect.Endpoint != b.node.BrowserEndpoint {
				t.Fatalf("unexpected endpoint %q", redirect.Endpoint)
			}
			loadsBefore := store.loads.Load()
			first := openColdRoomSocket(t, a.client, redirect.Endpoint, token)
			second := openColdRoomSocket(t, a.client, redirect.Endpoint, writerToken)
			for _, conn := range []*websocket.Conn{first, second} {
				writeBrowserMessage(t, conn, request)
			}
			for _, conn := range []*websocket.Conn{first, second} {
				if kind == protocol.TypeJoin {
					if m := expectColdRoomResponse(t, conn, protocol.TypeJoined); m.Content != "Xabc" || m.Version != 1 {
						t.Fatalf("unexpected joined document %#v", m)
					}
				} else if m := expectColdRoomResponse(t, conn, protocol.TypeSync); m.ToVersion != 1 || len(m.Operations) != 1 || m.Operations[0].RequestID != "first" {
					t.Fatalf("unexpected sync %#v", m)
				}
			}
			if count := store.loads.Load() - loadsBefore; count != 1 {
				t.Fatalf("cold recovery loaded storage %d times", count)
			}
			edit.BaseVersion = 1
			edit.RequestID = "after-recovery"
			writeBrowserMessage(t, second, edit)
			for _, conn := range []*websocket.Conn{first, second} {
				if m := expectColdRoomResponse(t, conn, protocol.TypeEditApplied); m.Version != 2 || m.RequestID != edit.RequestID {
					t.Fatalf("unexpected edit %#v", m)
				}
			}
		})
	}
}
