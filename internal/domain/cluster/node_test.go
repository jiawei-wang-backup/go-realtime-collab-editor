package cluster

import (
	"testing"
	"time"
)

func TestNodeValidate(t *testing.T) {
	node := Node{NodeID: "node-a", Incarnation: "process-1", QUICEndpoint: "node-a:8443", HTTPEndpoint: "node-a:8080", Status: NodeStarting, StartedAt: time.Now()}
	if err := node.Validate(); err != nil {
		t.Fatalf("validate node: %v", err)
	}
	node.NodeID = "invalid/node"
	if err := node.Validate(); err == nil {
		t.Fatal("expected invalid node ID to be rejected")
	}
}
