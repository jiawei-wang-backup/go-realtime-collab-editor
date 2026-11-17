package cluster

import (
	"testing"

	model "github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/cluster"
)

func TestRankNodesIsDeterministicAndIgnoresUnavailableNodes(t *testing.T) {
	nodes := []model.Node{
		{NodeID: "node-c", Incarnation: "1", Status: model.NodeUnavailable},
		{NodeID: "node-a", Incarnation: "1", Status: model.NodeReady},
		{NodeID: "node-b", Incarnation: "1", Status: model.NodeReady},
	}
	first := RankNodes("main.go", nodes)
	second := RankNodes("main.go", []model.Node{nodes[2], nodes[0], nodes[1]})
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("expected two ready nodes, got %d and %d", len(first), len(second))
	}
	for index := range first {
		if first[index].Identity() != second[index].Identity() {
			t.Fatalf("ranking differs at %d: %q != %q", index, first[index].Identity(), second[index].Identity())
		}
	}
}

func TestRankNodesHasLimitedMovementWhenNodeJoins(t *testing.T) {
	initial := []model.Node{{NodeID: "a", Incarnation: "1", Status: model.NodeReady}, {NodeID: "b", Incarnation: "1", Status: model.NodeReady}}
	expanded := append(append([]model.Node(nil), initial...), model.Node{NodeID: "c", Incarnation: "1", Status: model.NodeReady})
	for index := 0; index < 1000; index++ {
		documentID := string(rune(index))
		before := RankNodes(documentID, initial)[0].Identity()
		after := RankNodes(documentID, expanded)[0].Identity()
		if after != "c/1" && before != after {
			t.Fatalf("document %d moved between existing nodes: %q -> %q", index, before, after)
		}
	}
}

func TestMembershipPublishesIncrementalChanges(t *testing.T) {
	membership := NewMembership()
	updates, cancel := membership.Subscribe(4)
	defer cancel()
	node := model.Node{NodeID: "node-a", Incarnation: "1", Status: model.NodeStarting}
	membership.Put(node, 1)
	if event := <-updates; event.Type != NodeJoined {
		t.Fatalf("expected joined event, got %q", event.Type)
	}
	node.Status = model.NodeReady
	membership.Put(node, 2)
	if event := <-updates; event.Type != NodeUpdated {
		t.Fatalf("expected updated event, got %q", event.Type)
	}
	membership.Delete(node.Identity(), 3)
	if event := <-updates; event.Type != NodeLeft {
		t.Fatalf("expected left event, got %q", event.Type)
	}
}
