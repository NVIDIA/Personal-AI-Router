// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package jsonrpc

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	assert.True(t, req.IsRequest(), "request misclassified")
	assert.False(t, req.IsNotification(), "request misclassified")
	assert.False(t, req.IsResponse(), "request misclassified")
	assert.True(t, note.IsNotification(), "notification misclassified")
	assert.False(t, note.IsRequest(), "notification misclassified")
	assert.False(t, note.IsResponse(), "notification misclassified")
	assert.True(t, resp.IsResponse(), "response misclassified")
	assert.False(t, resp.IsRequest(), "response misclassified")
	assert.False(t, resp.IsNotification(), "response misclassified")
}

func TestNotifyRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeCodec(&buf).Notify("node/discovered", map[string]string{"id": "n1"}), "Notify")
	assert.True(t, bytes.HasSuffix(buf.Bytes(), []byte("\n")), "frame not newline-terminated")
	msg, err := readCodec(&buf).Read()
	require.NoError(t, err, "Read")
	assert.True(t, msg.IsNotification(), "unexpected frame")
	assert.Equal(t, "node/discovered", msg.Method, "unexpected frame")
	var params map[string]string
	{
		err := json.Unmarshal(msg.Params, &params)
		require.NoError(t, err, "params round-trip failed")
		assert.Equal(t, "n1", params["id"], "params round-trip failed")
	}
}

func TestRespondAndError(t *testing.T) {
	id := json.RawMessage("7")

	var okBuf bytes.Buffer
	require.NoError(t, writeCodec(&okBuf).Respond(&id, map[string]int{"level": 1}), "Respond")
	msg, err := readCodec(&okBuf).Read()
	require.NoError(t, err, "bad ok response")
	assert.True(t, msg.IsResponse(), "bad ok response")
	require.Nil(t, msg.Error, "bad ok response")

	var errBuf bytes.Buffer
	require.NoError(t, writeCodec(&errBuf).RespondError(&id, -32601, "method not found"), "RespondError")
	emsg, err := readCodec(&errBuf).Read()
	require.NoError(t, err, "Read err response")
	require.NotNil(t, emsg.Error, "bad error response")
	assert.Equal(t, -32601, emsg.Error.Code, "bad error response")
	assert.Equal(t, "method not found", emsg.Error.Message, "bad error response")
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
	require.NoError(t, writeCodec(&w).Notify("m", map[string]string{"k": "value-that-spans-many-writes"}), "Notify")
	msg, err := readCodec(bytes.NewReader(w.buf.Bytes())).Read()
	require.NoError(t, err, "Read after short writes")
	var params map[string]string
	{
		err := json.Unmarshal(msg.Params, &params)
		require.NoError(t, err, "short-write corrupted frame")
		assert.Equal(t, "value-that-spans-many-writes", params["k"], "short-write corrupted frame")
	}
}

func TestReadRejectsBadVersionAndEOF(t *testing.T) {
	{
		_, err := readCodec(strings.NewReader(`{"jsonrpc":"1.0","method":"m"}` + "\n")).Read()
		require.Error(t, err, "expected error for unsupported jsonrpc version")
	}
	{
		_, err := readCodec(strings.NewReader("")).Read()
		assert.True(t, err == io.EOF, "expected io.EOF on empty stream,")
	}
}

func TestReadMalformedFrameIsRecoverableDecodeError(t *testing.T) {
	// Both bad JSON and a bad version are recoverable *DecodeError so a read
	// loop can continue rather than treating them as terminal.
	for _, frame := range []string{"{not json}\n", `{"jsonrpc":"1.0"}` + "\n"} {
		_, err := readCodec(strings.NewReader(frame)).Read()
		var de *DecodeError
		assert.ErrorAs(t, err, &de, "frame")
	}
}

func TestRespondErrorDataRoundTrip(t *testing.T) {
	id := json.RawMessage("3")
	var buf bytes.Buffer
	require.NoError(t, writeCodec(&buf).RespondErrorData(&id, -32602, "bad", map[string]string{"field": "port"}), "RespondErrorData")
	msg, err := readCodec(&buf).Read()
	require.NoError(t, err, "Read")
	require.NotNil(t, msg.Error, "bad error")
	assert.Equal(t, -32602, msg.Error.Code, "bad error")
	var data map[string]string
	{
		err := json.Unmarshal(msg.Error.Data, &data)
		require.NoError(t, err, "error data round-trip failed")
		assert.Equal(t, "port", data["field"], "error data round-trip failed")
	}
	// nil data omits the field entirely.
	var buf2 bytes.Buffer
	_ = writeCodec(&buf2).RespondErrorData(&id, -32603, "boom", nil)
	m2, _ := readCodec(&buf2).Read()
	require.Empty(t, m2.Error.Data, "expected empty data for nil,")
}

func TestNewCodecMaxFrameAcceptsLargeFrame(t *testing.T) {
	big := strings.Repeat("x", 2<<20) // 2 MiB, over the 1 MiB default
	var buf bytes.Buffer
	require.NoError(t, writeCodec(&buf).Notify("m", map[string]string{"blob": big}), "Notify")
	c := NewCodecMaxFrame(rw{Reader: bytes.NewReader(buf.Bytes()), Writer: io.Discard}, 8<<20)
	msg, err := c.Read()
	require.NoError(t, err, "Read large frame")
	var params map[string]string
	{
		err := json.Unmarshal(msg.Params, &params)
		require.NoError(t, err, "large frame round-trip failed")
		require.Len(t, params["blob"], len(big), "large frame round-trip failed")
	}
}
