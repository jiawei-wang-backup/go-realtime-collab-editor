package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

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

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }
