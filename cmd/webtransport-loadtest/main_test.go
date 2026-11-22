package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/collab"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/protocol"
)

type shortWriter struct {
	writer bytes.Buffer
	limit  int
}

func (w *shortWriter) Write(payload []byte) (int, error) {
	if len(payload) > w.limit {
		payload = payload[:w.limit]
	}
	return w.writer.Write(payload)
}

func TestMessageFrameRoundTripWithShortWrites(t *testing.T) {
	want := protocol.Message{Type: protocol.TypeJoin, DocID: "doc", LastVersion: 7}
	writer := &shortWriter{limit: 2}
	if err := writeMessage(writer, want); err != nil {
		t.Fatal(err)
	}
	got, err := readMessage(bytes.NewReader(writer.writer.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != want.Type || got.DocID != want.DocID || got.LastVersion != want.LastVersion {
		t.Fatalf("message = %#v, want %#v", got, want)
	}
}

func TestMessageFrameRejectsOversize(t *testing.T) {
	payload := make([]byte, 4)
	binary.BigEndian.PutUint32(payload, maxMessageSize+1)
	_, err := readMessage(bytes.NewReader(payload))
	if err == nil || err.Error() != "message exceeds 1 MiB" {
		t.Fatalf("error = %v", err)
	}
}

func TestMessageFrameRejectsTruncatedPayload(t *testing.T) {
	var frame bytes.Buffer
	frame.Write([]byte{0, 0, 0, 4})
	frame.Write([]byte("xy"))
	_, err := readMessage(&frame)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want unexpected EOF", err)
	}
}

func TestMessageFrameRejectsZeroProgressWriter(t *testing.T) {
	writer := zeroWriter{}
	err := writeMessage(writer, protocol.Message{Type: protocol.TypeAuth})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("error = %v, want short write", err)
	}
}

func TestApplyRecoveryMessageSync(t *testing.T) {
	session := &session{docID: "doc", content: "a"}
	message := protocol.Message{
		Type:        protocol.TypeSync,
		DocID:       "doc",
		FromVersion: 1,
		ToVersion:   2,
		Operations: []protocol.VersionedOperation{{
			Version:   2,
			Operation: collab.Operation{Kind: collab.OperationInsert, Position: 1, Text: "b"},
		}},
	}
	if err := applyRecoveryMessage(session, message, 1); err != nil {
		t.Fatal(err)
	}
	if session.version.Load() != 2 || session.content != "ab" {
		t.Fatalf("recovered state = version %d content %q", session.version.Load(), session.content)
	}
}

func TestApplyRecoveryMessageResync(t *testing.T) {
	session := &session{docID: "doc", content: "stale"}
	message := protocol.Message{
		Type:           protocol.TypeResyncRequired,
		DocID:          "doc",
		Snapshot:       &protocol.Snapshot{Version: 1, Content: "a"},
		CurrentVersion: 2,
		Operations: []protocol.VersionedOperation{{
			Version:   2,
			Operation: collab.Operation{Kind: collab.OperationInsert, Position: 1, Text: "b"},
		}},
	}
	if err := applyRecoveryMessage(session, message, 0); err != nil {
		t.Fatal(err)
	}
	if !session.resumeResync.Load() || session.version.Load() != 2 || session.content != "ab" {
		t.Fatalf("resync state = resync %v version %d content %q", session.resumeResync.Load(), session.version.Load(), session.content)
	}
}

func TestApplyRecoveryMessageRejectsNonContiguousOperations(t *testing.T) {
	session := &session{docID: "doc", content: "a"}
	err := applyRecoveryMessage(session, protocol.Message{
		Type:        protocol.TypeSync,
		DocID:       "doc",
		FromVersion: 1,
		ToVersion:   3,
		Operations: []protocol.VersionedOperation{{
			Version:   3,
			Operation: collab.Operation{Kind: collab.OperationInsert, Position: 1, Text: "b"},
		}},
	}, 1)
	if err == nil {
		t.Fatal("expected non-contiguous recovery operation error")
	}
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }
