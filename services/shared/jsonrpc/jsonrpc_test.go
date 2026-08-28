// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

// rw adapts a separate reader and writer into an io.ReadWriter for NewCodec.
type rw struct {
	io.Reader
	io.Writer
}

func writeCodec(w io.Writer) *Codec { return NewCodec(rw{Reader: bytes.NewReader(nil), Writer: w}) }
func readCodec(r io.Reader) *Codec  { return NewCodec(rw{Reader: r, Writer: io.Discard}) }

func TestClassifiers(t *testing.T) {
	id := json.RawMessage("1")
	req := &Message{JSONRPC: "2.0", ID: &id, Method: "m"}
	note := &Message{JSONRPC: "2.0", Method: "m"}
	resp := &Message{JSONRPC: "2.0", ID: &id}

	if !req.IsRequest() || req.IsNotification() || req.IsResponse() {
		t.Errorf("request misclassified")
	}
	if !note.IsNotification() || note.IsRequest() || note.IsResponse() {
		t.Errorf("notification misclassified")
	}
	if !resp.IsResponse() || resp.IsRequest() || resp.IsNotification() {
		t.Errorf("response misclassified")
	}
}

func TestNotifyRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := writeCodec(&buf).Notify("node/discovered", map[string]string{"id": "n1"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
		t.Fatalf("frame not newline-terminated: %q", buf.String())
	}
	msg, err := readCodec(&buf).Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !msg.IsNotification() || msg.Method != "node/discovered" {
		t.Fatalf("unexpected frame: %+v", msg)
	}
	var params map[string]string
	if err := json.Unmarshal(msg.Params, &params); err != nil || params["id"] != "n1" {
		t.Fatalf("params round-trip failed: %v %v", params, err)
	}
}

func TestRespondAndError(t *testing.T) {
	id := json.RawMessage("7")

	var okBuf bytes.Buffer
	if err := writeCodec(&okBuf).Respond(&id, map[string]int{"level": 1}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	msg, err := readCodec(&okBuf).Read()
	if err != nil || !msg.IsResponse() || msg.Error != nil {
		t.Fatalf("bad ok response: %+v err=%v", msg, err)
	}

	var errBuf bytes.Buffer
	if err := writeCodec(&errBuf).RespondError(&id, -32601, "method not found"); err != nil {
		t.Fatalf("RespondError: %v", err)
	}
	emsg, err := readCodec(&errBuf).Read()
	if err != nil {
		t.Fatalf("Read err response: %v", err)
	}
	if emsg.Error == nil || emsg.Error.Code != -32601 || emsg.Error.Message != "method not found" {
		t.Fatalf("bad error response: %+v", emsg.Error)
	}
}

// oneByteWriter accepts a single byte per Write, forcing the codec's
// short-write loop to run to completion.
type oneByteWriter struct{ buf bytes.Buffer }

func (w *oneByteWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	_ = w.buf.WriteByte(p[0])
	return 1, nil
}

func TestShortWriteWritesFullFrame(t *testing.T) {
	var w oneByteWriter
	if err := writeCodec(&w).Notify("m", map[string]string{"k": "value-that-spans-many-writes"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	msg, err := readCodec(bytes.NewReader(w.buf.Bytes())).Read()
	if err != nil {
		t.Fatalf("Read after short writes: %v", err)
	}
	var params map[string]string
	if err := json.Unmarshal(msg.Params, &params); err != nil || params["k"] != "value-that-spans-many-writes" {
		t.Fatalf("short-write corrupted frame: %v %v", params, err)
	}
}

func TestReadRejectsBadVersionAndEOF(t *testing.T) {
	if _, err := readCodec(strings.NewReader(`{"jsonrpc":"1.0","method":"m"}` + "\n")).Read(); err == nil {
		t.Fatalf("expected error for unsupported jsonrpc version")
	}
	if _, err := readCodec(strings.NewReader("")).Read(); err != io.EOF {
		t.Fatalf("expected io.EOF on empty stream, got %v", err)
	}
}

func TestReadMalformedFrameIsRecoverableDecodeError(t *testing.T) {
	// Both bad JSON and a bad version are recoverable *DecodeError so a read
	// loop can continue rather than treating them as terminal.
	for _, frame := range []string{"{not json}\n", `{"jsonrpc":"1.0"}` + "\n"} {
		_, err := readCodec(strings.NewReader(frame)).Read()
		var de *DecodeError
		if !errors.As(err, &de) {
			t.Fatalf("frame %q: want *DecodeError, got %T (%v)", frame, err, err)
		}
	}
}

func TestRespondErrorDataRoundTrip(t *testing.T) {
	id := json.RawMessage("3")
	var buf bytes.Buffer
	if err := writeCodec(&buf).RespondErrorData(&id, -32602, "bad", map[string]string{"field": "port"}); err != nil {
		t.Fatalf("RespondErrorData: %v", err)
	}
	msg, err := readCodec(&buf).Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if msg.Error == nil || msg.Error.Code != -32602 {
		t.Fatalf("bad error: %+v", msg.Error)
	}
	var data map[string]string
	if err := json.Unmarshal(msg.Error.Data, &data); err != nil || data["field"] != "port" {
		t.Fatalf("error data round-trip failed: %v %v", data, err)
	}
	// nil data omits the field entirely.
	var buf2 bytes.Buffer
	_ = writeCodec(&buf2).RespondErrorData(&id, -32603, "boom", nil)
	m2, _ := readCodec(&buf2).Read()
	if len(m2.Error.Data) != 0 {
		t.Fatalf("expected empty data for nil, got %q", string(m2.Error.Data))
	}
}

func TestNewCodecMaxFrameAcceptsLargeFrame(t *testing.T) {
	big := strings.Repeat("x", 2<<20) // 2 MiB, over the 1 MiB default
	var buf bytes.Buffer
	if err := writeCodec(&buf).Notify("m", map[string]string{"blob": big}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	c := NewCodecMaxFrame(rw{Reader: bytes.NewReader(buf.Bytes()), Writer: io.Discard}, 8<<20)
	msg, err := c.Read()
	if err != nil {
		t.Fatalf("Read large frame: %v", err)
	}
	var params map[string]string
	if err := json.Unmarshal(msg.Params, &params); err != nil || len(params["blob"]) != len(big) {
		t.Fatalf("large frame round-trip failed: %v", err)
	}
}
