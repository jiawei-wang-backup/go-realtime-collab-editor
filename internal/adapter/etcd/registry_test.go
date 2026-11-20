package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestLeaseConfirmationWindowKeepsSafetyMargin(t *testing.T) {
	for _, ttlSeconds := range []int64{2, 3, 9} {
		ttl := time.Duration(ttlSeconds) * time.Second
		window := leaseConfirmationWindow(ttlSeconds)
		if window >= ttl || window <= ttl/2 {
			t.Fatalf("TTL %d confirmation window %s has no safe renewal margin", ttlSeconds, window)
		}
	}
}

type idleMembershipWatcher struct{ clientv3.Watcher }

func (idleMembershipWatcher) Watch(ctx context.Context, _ string, _ ...clientv3.OpOption) clientv3.WatchChan {
	events := make(chan clientv3.WatchResponse)
	go func() {
		<-ctx.Done()
		close(events)
	}()
	return events
}

func TestRegistryLogsLeaseLoss(t *testing.T) {
	for _, tc := range []struct {
		reason string
		ack    bool
	}{
		{"keepalive_closed", false},
		{"invalid_keepalive_response", true},
		{"confirmation_timeout", true},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			var buf bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			registry := Registry{client: &Client{client: &clientv3.Client{Watcher: idleMembershipWatcher{}}}, leaseTTL: 3, cancel: cancel, done: make(chan struct{}), lost: make(chan error, 1)}
			ch := make(chan *clientv3.LeaseKeepAliveResponse, 2)
			if tc.ack {
				ch <- &clientv3.LeaseKeepAliveResponse{TTL: 1}
			}
			switch tc.reason {
			case "keepalive_closed":
				close(ch)
			case "invalid_keepalive_response":
				ch <- &clientv3.LeaseKeepAliveResponse{TTL: 0}
			}
			go registry.run(ctx, ch, 1)
			select {
			case <-registry.done:
			case <-time.After(5 * time.Second):
				t.Fatal("registry did not exit")
			}
			select {
			case err := <-registry.lost:
				if !errors.Is(err, ErrLeaseLost) {
					t.Fatalf("unexpected signal: %v", err)
				}
			default:
				t.Fatal("missing lease loss signal")
			}
			var event struct {
				Reason   string
				Received bool   `json:"keepalive_received"`
				Age      *int64 `json:"since_last_keepalive_ms"`
				TTL      int64  `json:"lease_ttl_seconds"`
				Window   int64  `json:"confirmation_window_ms"`
			}
			if err := json.Unmarshal(buf.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			wantTTL := registry.leaseTTL
			if tc.ack {
				wantTTL = 1
			}
			if event.Reason != tc.reason || event.Received != tc.ack || (event.Age != nil) != tc.ack || event.TTL != wantTTL || event.Window != leaseConfirmationWindow(wantTTL).Milliseconds() {
				t.Fatalf("unexpected diagnostic: %s", buf.String())
			}
		})
	}
}
