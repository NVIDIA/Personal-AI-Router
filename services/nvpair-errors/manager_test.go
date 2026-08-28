// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"testing"
	"time"
)

// captureRW splits the manager's outbound stream into two channels at
// write time: frames that carry a JSON-RPC `id` (responses, including
// errors) land in `responses`, frames without an `id` (notifications,
// e.g. errors:update) land in `notifications`. The split lets each test
// assert on exactly the side it cares about without racing the other.
//
// Read() returns EOF so the manager's readLoop exits cleanly when we
// drive the manager via handleMessage instead of feeding bytes.
type captureRW struct {
	responses     chan []byte
	notifications chan []byte
}

func newCaptureRW() *captureRW {
	return &captureRW{
		responses:     make(chan []byte, 64),
		notifications: make(chan []byte, 64),
	}
}

func (rw *captureRW) Read(_ []byte) (int, error) {
	return 0, io.EOF
}

func (rw *captureRW) Write(p []byte) (int, error) {
	cp := append([]byte(nil), p...)
	var head struct {
		ID *json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(cp, &head); err != nil {
		panic(fmt.Sprintf("captureRW: malformed frame from manager: %v\nframe: %s", err, cp))
	}
	if head.ID != nil {
		rw.responses <- cp
	} else {
		rw.notifications <- cp
	}
	return len(p), nil
}

func newTestManager(t *testing.T) (*Manager, *captureRW) {
	t.Helper()
	rw := newCaptureRW()
	m := NewManager(NewCodec(rw))
	// Pin the local node id to match the fixtures' NodeID so clear
	// (now scoped to local-origin entries) resolves against the
	// errors these tests report. Without this the manager's localNodeID
	// is the host's real hostname and clear-by-id would never match the
	// "test-node"-stamped fixtures.
	m.localNodeID = "test-node"
	return m, rw
}

func requestMessage(id int, method string, params any) *Message {
	idData, _ := json.Marshal(id)
	idRaw := json.RawMessage(idData)
	paramsRaw, _ := json.Marshal(params)
	return &Message{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: paramsRaw}
}

func notificationMessage(method string, params any) *Message {
	paramsRaw, _ := json.Marshal(params)
	return &Message{JSONRPC: "2.0", Method: method, Params: paramsRaw}
}

func readResponseFrame(t *testing.T, rw *captureRW) Message {
	t.Helper()
	select {
	case data := <-rw.responses:
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("decode response frame %q: %v", data, err)
		}
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for response frame")
		return Message{}
	}
}

func readNotificationFrame(t *testing.T, rw *captureRW) Message {
	t.Helper()
	select {
	case data := <-rw.notifications:
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("decode notification frame %q: %v", data, err)
		}
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for notification frame")
		return Message{}
	}
}

func expectNotification(t *testing.T, rw *captureRW, method string) Message {
	t.Helper()
	msg := readNotificationFrame(t, rw)
	if msg.Method != method {
		t.Fatalf("notification method = %q, want %q", msg.Method, method)
	}
	return msg
}

// assertNoNotification asserts no notification has been queued. Used to
// verify that a no-op operation (clear of absent id, report dropped
// because of older timestamp) does NOT broadcast errors:update.
func assertNoNotification(t *testing.T, rw *captureRW) {
	t.Helper()
	select {
	case data := <-rw.notifications:
		t.Fatalf("unexpected notification: %s", data)
	case <-time.After(50 * time.Millisecond):
	}
}

func assertNoResponse(t *testing.T, rw *captureRW) {
	t.Helper()
	select {
	case data := <-rw.responses:
		t.Fatalf("unexpected response: %s", data)
	case <-time.After(50 * time.Millisecond):
	}
}

func decodeResult[T any](t *testing.T, msg Message) T {
	t.Helper()
	if msg.Error != nil {
		t.Fatalf("unexpected RPC error: %+v", msg.Error)
	}
	var result T
	if err := json.Unmarshal(msg.Result, &result); err != nil {
		t.Fatalf("decode result %q: %v", msg.Result, err)
	}
	return result
}

func responseWithID(id int) func(Message) bool {
	return func(msg Message) bool {
		if msg.ID == nil {
			return false
		}
		var got int
		return json.Unmarshal(*msg.ID, &got) == nil && got == id
	}
}

// callAndDecode submits one request, reads exactly one response frame
// (every request emits exactly one), and decodes the `result` into T.
// Anything that produces an RPC error fails via decodeResult.
func callAndDecode[T any](t *testing.T, m *Manager, rw *captureRW, id int, method string, params any) T {
	t.Helper()
	m.handleMessage(requestMessage(id, method, params))
	resp := readResponseFrame(t, rw)
	if !responseWithID(id)(resp) {
		t.Fatalf("response id mismatch: got %+v want id=%d", resp, id)
	}
	return decodeResult[T](t, resp)
}

func callExpectError(t *testing.T, m *Manager, rw *captureRW, id int, method string, params any, wantCode int) *RPCError {
	t.Helper()
	m.handleMessage(requestMessage(id, method, params))
	resp := readResponseFrame(t, rw)
	if !responseWithID(id)(resp) {
		t.Fatalf("response id mismatch: got %+v want id=%d", resp, id)
	}
	if resp.Error == nil {
		t.Fatalf("expected error %d, got result: %s", wantCode, string(resp.Result))
	}
	if resp.Error.Code != wantCode {
		t.Fatalf("error code = %d, want %d (message=%q)", resp.Error.Code, wantCode, resp.Error.Message)
	}
	return resp.Error
}

func sampleError(id string, ts int64, msg string) ServiceError {
	return ServiceError{
		ID:        id,
		Message:   msg,
		Timestamp: ts,
		NodeID:    "test-node",
	}
}

// TestGetInitialEmpty: the freshly-constructed manager reports an empty
// list, not nil. A nil result would JSON-encode as `null` and the
// frontend would have to special-case it; explicit `[]` is the contract.
func TestGetInitialEmpty(t *testing.T) {
	m, rw := newTestManager(t)
	got := callAndDecode[[]ServiceError](t, m, rw, 1, "errors:get-initial", nil)
	if len(got) != 0 {
		t.Fatalf("get-initial on empty manager = %+v, want empty", got)
	}
}

// TestReportRequestUpsertsAndEmitsUpdate: an errors:report REQUEST gets
// a null response AND triggers an errors:update push carrying the new
// state. Response is sent first; the test relies on the captureRW split
// so reading the response side first does not race the update push.
func TestReportRequestUpsertsAndEmitsUpdate(t *testing.T) {
	m, rw := newTestManager(t)
	e := sampleError("ollama-proxy:upstream-unreachable:node-a", 1000, "node-a unreachable")

	m.handleMessage(requestMessage(1, "errors:report", e))

	resp := readResponseFrame(t, rw)
	if resp.Error != nil {
		t.Fatalf("errors:report returned error: %+v", resp.Error)
	}

	update := expectNotification(t, rw, "errors:update")
	got := decodeResult[[]ServiceError](t, Message{Result: update.Params})
	if len(got) != 1 || got[0].ID != e.ID || got[0].Message != e.Message {
		t.Fatalf("errors:update payload = %+v, want single entry matching reported error", got)
	}

	// And get-initial returns the same shape.
	list := callAndDecode[[]ServiceError](t, m, rw, 2, "errors:get-initial", nil)
	if len(list) != 1 || list[0].ID != e.ID {
		t.Fatalf("get-initial after report = %+v, want single entry", list)
	}
}

// TestReportNotificationUpsertsAndEmitsUpdate: the notification form
// (no id) shares all the upsert + push plumbing with the request form,
// but sends NO response frame.
func TestReportNotificationUpsertsAndEmitsUpdate(t *testing.T) {
	m, rw := newTestManager(t)
	e := sampleError("manual-nodes:probe-failed:peer-1", 5000, "peer-1 probe failed")

	m.handleMessage(notificationMessage("errors:report", e))

	expectNotification(t, rw, "errors:update")
	assertNoResponse(t, rw)

	list := callAndDecode[[]ServiceError](t, m, rw, 1, "errors:get-initial", nil)
	if len(list) != 1 || list[0].ID != e.ID {
		t.Fatalf("get-initial after notify-form report = %+v, want single entry", list)
	}
}

// TestUpsertHighestTimestampWins: a later-arriving report with an OLDER
// timestamp is dropped — protects against out-of-order delivery once
// cross-node propagation lands. A no-op upsert must NOT emit
// errors:update (otherwise the UI would re-render on every dropped
// stale frame).
func TestUpsertHighestTimestampWins(t *testing.T) {
	m, rw := newTestManager(t)
	id := "ollama-local:not-running"

	// Initial report at ts=1000 wins.
	m.handleMessage(notificationMessage("errors:report", sampleError(id, 1000, "v1")))
	expectNotification(t, rw, "errors:update")

	// Older ts=500 must be dropped — no update push.
	m.handleMessage(notificationMessage("errors:report", sampleError(id, 500, "v0-stale")))
	assertNoNotification(t, rw)

	// Newer ts=2000 wins — update push fires.
	m.handleMessage(notificationMessage("errors:report", sampleError(id, 2000, "v2")))
	expectNotification(t, rw, "errors:update")

	list := callAndDecode[[]ServiceError](t, m, rw, 1, "errors:get-initial", nil)
	if len(list) != 1 || list[0].Message != "v2" {
		t.Fatalf("after stale-then-newer, list = %+v, want v2 only", list)
	}
}

// TestUpsertEqualTimestampReplaces: equal timestamps must NOT be a
// no-op — a producer re-emitting a steady-state error refreshes the
// stored message and metadata. The rationale lives in the upsert
// docstring; this test pins the behavior so a future "exact dedupe"
// refactor doesn't quietly regress it.
func TestUpsertEqualTimestampReplaces(t *testing.T) {
	m, rw := newTestManager(t)
	id := "ollama-local:install-failed"

	m.handleMessage(notificationMessage("errors:report", sampleError(id, 1000, "first")))
	expectNotification(t, rw, "errors:update")

	m.handleMessage(notificationMessage("errors:report", sampleError(id, 1000, "second")))
	update := expectNotification(t, rw, "errors:update")
	got := decodeResult[[]ServiceError](t, Message{Result: update.Params})
	if len(got) != 1 || got[0].Message != "second" {
		t.Fatalf("equal-ts upsert = %+v, want message=second", got)
	}
}

// TestClearRemovesEntryAndEmitsUpdate: an errors:clear removes the
// entry by id and fires an errors:update reflecting the new (empty)
// state.
func TestClearRemovesEntryAndEmitsUpdate(t *testing.T) {
	m, rw := newTestManager(t)
	id := "supervisor:subprocess-crashed:nvpair-node-info"

	m.handleMessage(notificationMessage("errors:report", sampleError(id, 1000, "crashed")))
	expectNotification(t, rw, "errors:update")

	m.handleMessage(requestMessage(1, "errors:clear", ClearParams{ID: id, ClearedBy: "node-self"}))
	resp := readResponseFrame(t, rw)
	if resp.Error != nil {
		t.Fatalf("errors:clear returned error: %+v", resp.Error)
	}

	update := expectNotification(t, rw, "errors:update")
	got := decodeResult[[]ServiceError](t, Message{Result: update.Params})
	if len(got) != 0 {
		t.Fatalf("after clear, update payload = %+v, want empty list", got)
	}

	list := callAndDecode[[]ServiceError](t, m, rw, 2, "errors:get-initial", nil)
	if len(list) != 0 {
		t.Fatalf("after clear, get-initial = %+v, want empty", list)
	}
}

// TestClearAbsentIdIsNoOp: clearing an id that was never reported (or
// was already cleared) must not push errors:update. The response is
// still a successful null — the UI fires defensive clears on success
// and treating "id not present" as an error would generate UI noise.
func TestClearAbsentIdIsNoOp(t *testing.T) {
	m, rw := newTestManager(t)

	_ = callAndDecode[any](t, m, rw, 1, "errors:clear", ClearParams{ID: "nothing:here"})
	assertNoNotification(t, rw)
}

// TestAckUntilReemit: clear is in-memory dismissal only. A subsequent
// report of the SAME id resurrects the entry. This is the "ack until
// re-emit" semantics from the plan: the producer is the source of
// truth, the user's clear is just "don't show me this until the
// producer says it's still broken."
func TestAckUntilReemit(t *testing.T) {
	m, rw := newTestManager(t)
	id := "ollama-proxy:upstream-unreachable:peer-x"

	m.handleMessage(notificationMessage("errors:report", sampleError(id, 1000, "first")))
	expectNotification(t, rw, "errors:update")

	m.handleMessage(notificationMessage("errors:clear", ClearParams{ID: id}))
	expectNotification(t, rw, "errors:update")

	// Re-emit must resurrect — newer timestamp.
	m.handleMessage(notificationMessage("errors:report", sampleError(id, 2000, "still broken")))
	update := expectNotification(t, rw, "errors:update")
	got := decodeResult[[]ServiceError](t, Message{Result: update.Params})
	if len(got) != 1 || got[0].Message != "still broken" {
		t.Fatalf("after clear+reemit, update payload = %+v, want resurrected entry", got)
	}
}

// TestReportMissingFieldsRejected: id and message are required;
// missing either returns -32602. Notifications log a warning but emit
// no errors:update.
func TestReportMissingFieldsRejected(t *testing.T) {
	m, rw := newTestManager(t)

	// Request form, missing id.
	callExpectError(t, m, rw, 1, "errors:report",
		ServiceError{Message: "no id", Timestamp: 1}, -32602)

	// Request form, missing message.
	callExpectError(t, m, rw, 2, "errors:report",
		ServiceError{ID: "x:y", Timestamp: 1}, -32602)

	// Notification form with no id: no response, no update push.
	m.handleMessage(notificationMessage("errors:report", ServiceError{Message: "no id", Timestamp: 1}))
	assertNoNotification(t, rw)
	assertNoResponse(t, rw)

	// And the store is still empty.
	list := callAndDecode[[]ServiceError](t, m, rw, 3, "errors:get-initial", nil)
	if len(list) != 0 {
		t.Fatalf("after invalid reports, list = %+v, want empty", list)
	}
}

// TestClearMissingIdRejected: clear without an id is -32602 in request
// form, warn-and-drop in notification form. Notifications must NOT
// emit errors:update on invalid input.
func TestClearMissingIdRejected(t *testing.T) {
	m, rw := newTestManager(t)

	callExpectError(t, m, rw, 1, "errors:clear", ClearParams{}, -32602)

	m.handleMessage(notificationMessage("errors:clear", ClearParams{}))
	assertNoNotification(t, rw)
	assertNoResponse(t, rw)
}

// TestUpdatePayloadIsFullSortedList: errors:update always carries the
// CURRENT FULL list, sorted by id. Two reports → one entry per id, in
// id order. The full-list contract means consumers can render-on-event
// without re-fetching, and the sort means cheap diff-by-position.
func TestUpdatePayloadIsFullSortedList(t *testing.T) {
	m, rw := newTestManager(t)

	// Emit in non-sorted order.
	m.handleMessage(notificationMessage("errors:report", sampleError("z:later", 1000, "z")))
	expectNotification(t, rw, "errors:update")
	m.handleMessage(notificationMessage("errors:report", sampleError("a:earlier", 2000, "a")))
	update := expectNotification(t, rw, "errors:update")

	got := decodeResult[[]ServiceError](t, Message{Result: update.Params})
	if len(got) != 2 {
		t.Fatalf("update payload len = %d, want 2 (full list)", len(got))
	}
	if got[0].ID != "a:earlier" || got[1].ID != "z:later" {
		t.Fatalf("update payload not sorted by id: %+v", got)
	}
}

// TestUnknownMethodReturns32601 mirrors node-settings: anything we
// haven't implemented is -32601 method-not-found, not a silent drop.
func TestUnknownMethodReturns32601(t *testing.T) {
	m, rw := newTestManager(t)
	callExpectError(t, m, rw, 1, "errors:does-not-exist", nil, -32601)
}

// TestClearedByPassedThroughIgnored: the broker stamps clearedBy for
// cross-node clear propagation; nvpair-errors must accept the field today
// (forward-compat unmarshal) and ignore it semantically (clear is just
// delete-by-id).
func TestClearedByPassedThroughIgnored(t *testing.T) {
	m, rw := newTestManager(t)
	id := "manual-nodes:probe-failed:peer-q"

	m.handleMessage(notificationMessage("errors:report", sampleError(id, 1000, "down")))
	expectNotification(t, rw, "errors:update")

	m.handleMessage(notificationMessage("errors:clear", ClearParams{ID: id, ClearedBy: "node-other"}))
	expectNotification(t, rw, "errors:update")

	list := callAndDecode[[]ServiceError](t, m, rw, 1, "errors:get-initial", nil)
	if len(list) != 0 {
		t.Fatalf("after foreign-clearedBy clear, list = %+v, want empty", list)
	}
}

// TestNodeIdPreserved: the producer-supplied nodeId is stored verbatim
// and round-trips through get-initial. This is the forward-compat
// contract that makes cross-node propagation additive — peer-stored
// errors keep their origin nodeId.
func TestNodeIdPreserved(t *testing.T) {
	m, rw := newTestManager(t)
	e := ServiceError{
		ID:        "peer:something",
		Message:   "remote error",
		Timestamp: 1000,
		NodeID:    "peer-node-id-123",
	}
	m.handleMessage(notificationMessage("errors:report", e))
	expectNotification(t, rw, "errors:update")

	list := callAndDecode[[]ServiceError](t, m, rw, 1, "errors:get-initial", nil)
	if len(list) != 1 || list[0].NodeID != "peer-node-id-123" {
		t.Fatalf("nodeId not preserved: list = %+v", list)
	}
}
