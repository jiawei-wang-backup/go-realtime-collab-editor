package webtransport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	memory "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/adapter/memory/documents"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/app/authentication"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/app/room"
	identity "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/authentication"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/cluster"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/collab"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/protocol"
	browsertransport "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/transport/browser"
	webtransportgo "github.com/quic-go/webtransport-go"
)

type integrationAuthenticator struct{}

func (integrationAuthenticator) AuthenticateToken(context.Context, string) (identity.Principal, error) {
	return identity.Principal{UserID: "owner", Username: "owner", SessionID: "session", Trusted: true}, nil
}
func (integrationAuthenticator) AuthenticateHTTP(*http.Request) (identity.Principal, error) {
	return identity.Principal{}, nil
}
func (integrationAuthenticator) ValidatePrincipal(context.Context, identity.Principal) error {
	return nil
}

type integrationRemoteOwner struct{ node cluster.Node }

var (
	_ cluster.OwnerResolver = integrationRemoteOwner{}
	_ cluster.NodeDirectory = integrationRemoteOwner{}
)

func (owner integrationRemoteOwner) Resolve(context.Context, string) (cluster.Owner, error) {
	return cluster.Owner{NodeID: owner.node.NodeID, Incarnation: owner.node.Incarnation, State: cluster.OwnerActive}, nil
}

func (owner integrationRemoteOwner) EnsureOwner(ctx context.Context, docID string) (cluster.Owner, error) {
	return owner.Resolve(ctx, docID)
}

func (owner integrationRemoteOwner) Lookup(nodeID, incarnation string) (cluster.Node, bool) {
	return owner.node, nodeID == owner.node.NodeID && incarnation == owner.node.Incarnation
}

func TestRealWebTransportDrainsOwnerRedirect(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	manager := room.NewManager(memory.NewMemory())
	t.Cleanup(func() {
		if err := manager.Shutdown(context.Background()); err != nil {
			t.Error(err)
		}
	})
	owner := integrationRemoteOwner{node: cluster.Node{
		NodeID: "remote", Incarnation: "remote-1", Status: cluster.NodeReady,
		BrowserEndpoint: "https://owner.example.com",
	}}
	tlsConfig := integrationServerTLS(t)
	cert, err := x509.ParseCertificate(tlsConfig.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	const origin = "https://editor.example.com"
	const writeTimeout = time.Second
	server, err := Listen(Config{
		Address: "127.0.0.1:0", TLSConfig: tlsConfig,
		AllowedOrigins: []string{origin}, StreamTimeout: time.Second,
		IdleTimeout: 5 * time.Second, MaxMessageSize: 1 << 20,
		Session: browsertransport.Config{
			Manager: manager, Authenticator: integrationAuthenticator{},
			AuthTimeout: time.Second, OperationTimeout: time.Second, WriteTimeout: writeTimeout, SendQueueSize: 8,
			OwnerResolver: owner, NodeDirectory: owner,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx) }()
	t.Cleanup(func() {
		if err := server.Close(context.Background()); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
		if err := <-serveDone; err != nil {
			t.Error(err)
		}
	})
	dialer := &webtransportgo.Transport{TLSClientConfig: &tls.Config{RootCAs: roots}}
	t.Cleanup(func() {
		if err := dialer.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, tt := range []struct {
		name       string
		peerCloses bool
	}{
		{name: "peer closes after reading", peerCloses: true},
		{name: "server reclaims unresponsive peer"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, session, err := dialer.Dial(ctx, "https://"+server.Addr().String()+Path, http.Header{"Origin": []string{origin}})
			if err != nil {
				t.Fatal(err)
			}
			stream, err := session.OpenStreamSync(ctx)
			if err != nil {
				t.Fatal(err)
			}
			deadline, _ := ctx.Deadline()
			if err := stream.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			writeTestMessage(t, stream, protocol.Message{Type: protocol.TypeAuth, Token: "access"})
			if msg := readTestMessage(t, stream); msg.Type != protocol.TypeAuthOK {
				t.Fatalf("auth response = %q", msg.Type)
			}
			joinStarted := time.Now()
			writeTestMessage(t, stream, protocol.Message{Type: protocol.TypeJoin, DocID: "doc"})
			if msg := readTestMessage(t, stream); msg.Type != protocol.TypeOwnerRedirect || msg.Endpoint != owner.node.BrowserEndpoint {
				t.Fatalf("redirect response = %#v", msg)
			}
			var extra [1]byte
			if _, err := stream.Read(extra[:]); !errors.Is(err, io.EOF) {
				t.Fatalf("expected stream EOF before session shutdown, got %v", err)
			}
			if session.Context().Err() != nil {
				t.Fatal("session closed before peer could act on the redirect")
			}
			if tt.peerCloses {
				if err := session.CloseWithError(0, "redirect received"); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-session.Context().Done():
			case <-ctx.Done():
				t.Fatal("redirect session was not reclaimed")
			}
			if elapsed := time.Since(joinStarted); !tt.peerCloses && elapsed < writeTimeout {
				t.Fatalf("server closed idle redirect session after %s, before the %s drain timeout", elapsed, writeTimeout)
			}
		})
	}
}

func TestRealWebTransportUsesSharedRoomProtocol(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := memory.NewMemory()
	if _, err := store.CreateDocument(ctx, "doc", "abc", "owner"); err != nil {
		t.Fatal(err)
	}
	manager := room.NewManager(store)
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	registry := authentication.NewConnectionRegistry()
	server, err := Listen(Config{
		Address: "127.0.0.1:0", TLSConfig: integrationServerTLS(t),
		AllowedOrigins: []string{"https://editor.example.com"}, StreamTimeout: time.Second,
		IdleTimeout: 5 * time.Second, MaxMessageSize: 1 << 20,
		Session: browsertransport.Config{
			Manager: manager, Authenticator: integrationAuthenticator{}, ConnectionRegistry: registry,
			AuthTimeout: time.Second, OperationTimeout: time.Second, WriteTimeout: time.Second, SendQueueSize: 8,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(ctx) }()
	t.Cleanup(func() {
		_ = server.Close(context.Background())
		<-serveDone
	})

	dialer := &webtransportgo.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	t.Cleanup(func() { _ = dialer.Close() })
	header := http.Header{"Origin": []string{"https://editor.example.com"}}
	started := time.Now()
	defer func() { t.Logf("PERF_SAMPLE_NS=%d", time.Since(started).Nanoseconds()) }()
	response, session, err := dialer.Dial(ctx, "https://"+server.Addr().String()+Path, header)
	if err != nil {
		t.Fatalf("dial WebTransport: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("upgrade status = %d", response.StatusCode)
	}
	stream, err := session.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	writeTestMessage(t, stream, protocol.Message{Type: protocol.TypeAuth, Token: "access"})
	if message := readTestMessage(t, stream); message.Type != protocol.TypeAuthOK {
		t.Fatalf("auth response = %#v", message)
	}
	writeTestMessage(t, stream, protocol.Message{Type: protocol.TypeJoin, DocID: "doc"})
	if message := readTestMessage(t, stream); message.Type != protocol.TypeJoined || message.Content != "abc" {
		t.Fatalf("join response = %#v", message)
	}
	if message := readTestMessage(t, stream); message.Type != protocol.TypePresenceChanged {
		t.Fatalf("presence response = %#v", message)
	}
	operation := collab.Operation{Kind: collab.OperationInsert, Position: 1, Text: "X"}
	writeTestMessage(t, stream, protocol.Message{Type: protocol.TypeEdit, DocID: "doc", RequestID: "wt-1", BaseVersion: 0, Operation: &operation})
	if message := readTestMessage(t, stream); message.Type != protocol.TypeEditApplied || message.Version != 1 {
		t.Fatalf("edit response = %#v", message)
	}
	registry.CloseSession("session")
	if _, err := stream.Read(make([]byte, 1)); err == nil {
		t.Fatal("revoked session kept WebTransport stream open")
	}
}

func writeTestMessage(t *testing.T, writer io.Writer, message protocol.Message) {
	t.Helper()
	payload, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)
	if _, err := writer.Write(frame); err != nil {
		t.Fatal(err)
	}
}

func readTestMessage(t *testing.T, reader io.Reader) protocol.Message {
	t.Helper()
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, binary.BigEndian.Uint32(header[:]))
	if _, err := io.ReadFull(reader, payload); err != nil {
		t.Fatal(err)
	}
	var message protocol.Message
	if err := json.Unmarshal(payload, &message); err != nil {
		t.Fatal(err)
	}
	return message
}

func integrationServerTLS(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{certificateDER}, PrivateKey: key}}}
}
