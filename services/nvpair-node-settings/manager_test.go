// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nvpair-shared/applog"
)

// captureRW is the write-only fake every manager test runs against.
// It splits the manager's outbound stream into two channels at write
// time: frames that carry a JSON-RPC `id` (responses, including
// errors) land in `responses`, and frames without an `id`
// (notifications) land in `notifications`. The split keeps the
// request/response tests oblivious to the cluster/* push
// notifications the manager emits after set-cluster-id and
// set-cluster-auto-sync — they just keep reading responses — while
// letting notification-focused tests inspect the push side directly
// without racing the response.
//
// Read() returns EOF so the manager's readLoop exits cleanly when we
// drive the manager via handleMessage instead of feeding it bytes.
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
	// Peek at the JSON envelope to decide which channel this frame
	// belongs to. We never produce malformed JSON from the codec, so
	// a decode error here is a test-infra bug, not a manager bug —
	// fail loudly rather than silently misroute.
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

func newTestManager(t *testing.T) (*Manager, *captureRW, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	rw := newCaptureRW()
	m, err := NewManager(NewCodec(rw), path)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m, rw, path
}

func requestMessage(id int, method string, params any) *Message {
	idData, _ := json.Marshal(id)
	idRaw := json.RawMessage(idData)
	paramsRaw, _ := json.Marshal(params)
	return &Message{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: paramsRaw}
}

func requestMessageRaw(id int, method string, params json.RawMessage) *Message {
	idData, _ := json.Marshal(id)
	idRaw := json.RawMessage(idData)
	return &Message{JSONRPC: "2.0", ID: &idRaw, Method: method, Params: params}
}

func notificationMessage(method string, params any) *Message {
	paramsRaw, _ := json.Marshal(params)
	return &Message{JSONRPC: "2.0", Method: method, Params: paramsRaw}
}

// readCaptureFrame pulls the next response frame the manager wrote.
// Notification frames go to a separate channel and are NOT consumed
// here — see readNotificationFrame. Keeping the two streams split
// matches the contract callers expect from JSON-RPC: a request gets
// exactly one response, but notifications can be emitted at any time
// (and after a single set-cluster-* call the manager fires both).
func readCaptureFrame(t *testing.T, rw *captureRW) Message {
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

// readNotificationFrame pulls the next notification (no `id`) the
// manager wrote. Use this when a test specifically wants to assert
// that the manager pushed a notification — tests that don't care
// about notifications can ignore them entirely, since they sit in a
// separate buffered channel that won't back-pressure responses.
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

// expectNotification reads the next notification and asserts the
// method matches. Returns the decoded message so callers can dig
// into params.
func expectNotification(t *testing.T, rw *captureRW, method string) Message {
	t.Helper()
	msg := readNotificationFrame(t, rw)
	if msg.Method != method {
		t.Fatalf("notification method = %q, want %q", msg.Method, method)
	}
	return msg
}

// assertNoNotification asserts that no notification has been
// queued, useful for verifying that the no-push setters (today:
// set-force-ports, set-cluster-friendly-name) do NOT fire a
// connection/* push.
func assertNoNotification(t *testing.T, rw *captureRW) {
	t.Helper()
	select {
	case data := <-rw.notifications:
		t.Fatalf("unexpected notification: %s", data)
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

// callAndDecode is the workhorse helper for the get/set round-trip
// tests: it submits one request, reads exactly one response frame
// (every request emits exactly one), and decodes the `result` into T.
// Anything that produces an RPC error fails the test via decodeResult.
func callAndDecode[T any](t *testing.T, m *Manager, rw *captureRW, id int, method string, params any) T {
	t.Helper()
	m.handleMessage(requestMessage(id, method, params))
	resp := readCaptureFrame(t, rw)
	if !responseWithID(id)(resp) {
		t.Fatalf("response id mismatch: got %+v want id=%d", resp, id)
	}
	return decodeResult[T](t, resp)
}

func callExpectError(t *testing.T, m *Manager, rw *captureRW, id int, method string, params any, wantCode int) *RPCError {
	t.Helper()
	m.handleMessage(requestMessage(id, method, params))
	resp := readCaptureFrame(t, rw)
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

func TestClusterAutoSyncRoundTrip(t *testing.T) {
	m, rw, _ := newTestManager(t)

	got := callAndDecode[map[string]bool](t, m, rw, 1, "settings/get-cluster-auto-sync", nil)
	if got["value"] != false {
		t.Fatalf("default cluster-auto-sync = %v, want false", got["value"])
	}

	_ = callAndDecode[map[string]bool](t, m, rw, 2, "settings/set-cluster-auto-sync", map[string]bool{"value": true})
	got = callAndDecode[map[string]bool](t, m, rw, 3, "settings/get-cluster-auto-sync", nil)
	if got["value"] != true {
		t.Fatalf("after set, value = %v, want true", got["value"])
	}
}

// TestClusterIDRoundTrip locks down the basic get/set contract for
// the cluster identifier. Empty is the default; the round-trip is
// byte-for-byte (no normalization, no trimming) so a future
// migration of the id format (UUID vs hash vs whatever security
// picks) doesn't have to coordinate with the datastore.
func TestClusterIDRoundTrip(t *testing.T) {
	m, rw, _ := newTestManager(t)

	got := callAndDecode[map[string]string](t, m, rw, 1, "settings/get-cluster-id", nil)
	if got["value"] != "" {
		t.Fatalf("default cluster-id = %q, want empty string", got["value"])
	}

	_ = callAndDecode[map[string]bool](t, m, rw, 2, "settings/set-cluster-id", map[string]string{"value": "cluster-abc-123"})
	got = callAndDecode[map[string]string](t, m, rw, 3, "settings/get-cluster-id", nil)
	if got["value"] != "cluster-abc-123" {
		t.Fatalf("after set, cluster-id = %q, want %q", got["value"], "cluster-abc-123")
	}

	// Setting back to empty returns to the unset state.
	_ = callAndDecode[map[string]bool](t, m, rw, 4, "settings/set-cluster-id", map[string]string{"value": ""})
	got = callAndDecode[map[string]string](t, m, rw, 5, "settings/get-cluster-id", nil)
	if got["value"] != "" {
		t.Fatalf("after clear, cluster-id = %q, want empty string", got["value"])
	}
}

// TestClusterFriendlyNameRoundTrip covers the display-only label.
// Same string get/set shape as cluster-id, but no push side — the
// notification assertions live in TestNoPushSettersDoNotEmit.
func TestClusterFriendlyNameRoundTrip(t *testing.T) {
	m, rw, _ := newTestManager(t)

	got := callAndDecode[map[string]string](t, m, rw, 1, "settings/get-cluster-friendly-name", nil)
	if got["value"] != "" {
		t.Fatalf("default cluster-friendly-name = %q, want empty string", got["value"])
	}

	_ = callAndDecode[map[string]bool](t, m, rw, 2, "settings/set-cluster-friendly-name", map[string]string{"value": "Lab 3 desks"})
	got = callAndDecode[map[string]string](t, m, rw, 3, "settings/get-cluster-friendly-name", nil)
	if got["value"] != "Lab 3 desks" {
		t.Fatalf("after set, cluster-friendly-name = %q, want %q", got["value"], "Lab 3 desks")
	}
}

// TestConnectionEndpointsAreNotRequestMethods locks down the
// contract that connection/cluster-identity and
// connection/cluster-auto-sync are PUSH NOTIFICATIONS, not request
// handlers. A peer that mistakenly tries to call them as requests
// must see a clean -32601 method-not-found, not a stale alias
// response, so any drift between this manager and an older client
// surfaces loudly.
func TestConnectionEndpointsAreNotRequestMethods(t *testing.T) {
	m, rw, _ := newTestManager(t)

	callExpectError(t, m, rw, 1, "connection/cluster-identity", nil, -32601)
	callExpectError(t, m, rw, 2, "connection/cluster-auto-sync", nil, -32601)
	// The set-shaped variants stay -32601 as well — they never
	// existed but we test them for symmetry to catch any future
	// accidental aliasing.
	callExpectError(t, m, rw, 3, "connection/set-cluster-identity", map[string]string{"value": "x"}, -32601)
	callExpectError(t, m, rw, 4, "connection/set-cluster-auto-sync", map[string]bool{"value": true}, -32601)
}

// TestRemovedLegacyMethodsReturnMethodNotFound is the regression
// guard for the schema change: the old auto-join-invites and
// cluster-secret RPC methods must not have survived as ghosts
// (e.g. via a forgotten case branch that mutates a now-deleted
// field). They should return -32601 just like any other unknown
// method.
func TestRemovedLegacyMethodsReturnMethodNotFound(t *testing.T) {
	m, rw, _ := newTestManager(t)

	for i, method := range []string{
		"settings/get-auto-join-invites",
		"settings/set-auto-join-invites",
		"settings/get-cluster-secret",
		"settings/set-cluster-secret",
	} {
		callExpectError(t, m, rw, i+1, method, map[string]any{"value": "ignored"}, -32601)
	}
}

// TestSetClusterIDEmitsConnectionIdentityNotification verifies the
// manager pushes connection/cluster-identity after every successful
// settings/set-cluster-id. The payload carries the raw id; consumers
// derive "are we clustered?" locally from `id != ""` because the
// real membership predicate is still owned by the security model
// and embedding a boolean here would freeze the wrong answer.
func TestSetClusterIDEmitsConnectionIdentityNotification(t *testing.T) {
	m, rw, _ := newTestManager(t)

	_ = callAndDecode[map[string]bool](t, m, rw, 1, "settings/set-cluster-id", map[string]string{"value": "cluster-xyz"})
	n := expectNotification(t, rw, "connection/cluster-identity")
	var p ClusterIdentityParams
	if err := json.Unmarshal(n.Params, &p); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if p.ID != "cluster-xyz" {
		t.Errorf("id = %q, want %q", p.ID, "cluster-xyz")
	}

	// Clearing the id must produce another push with the empty
	// value so the UI flips any "in a cluster" affordance off.
	_ = callAndDecode[map[string]bool](t, m, rw, 2, "settings/set-cluster-id", map[string]string{"value": ""})
	n = expectNotification(t, rw, "connection/cluster-identity")
	if err := json.Unmarshal(n.Params, &p); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if p.ID != "" {
		t.Errorf("post-clear id = %q, want empty", p.ID)
	}
}

// TestSetClusterAutoSyncEmitsConnectionAutoSyncNotification verifies
// the auto-sync push side. Same lifecycle as cluster-identity: one
// notification per successful set, carrying the post-set value.
func TestSetClusterAutoSyncEmitsConnectionAutoSyncNotification(t *testing.T) {
	m, rw, _ := newTestManager(t)

	_ = callAndDecode[map[string]bool](t, m, rw, 1, "settings/set-cluster-auto-sync", map[string]bool{"value": true})
	n := expectNotification(t, rw, "connection/cluster-auto-sync")
	var p ClusterAutoSyncParams
	if err := json.Unmarshal(n.Params, &p); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if p.Value != true {
		t.Errorf("value = %v, want true", p.Value)
	}

	_ = callAndDecode[map[string]bool](t, m, rw, 2, "settings/set-cluster-auto-sync", map[string]bool{"value": false})
	n = expectNotification(t, rw, "connection/cluster-auto-sync")
	if err := json.Unmarshal(n.Params, &p); err != nil {
		t.Fatalf("decode params: %v", err)
	}
	if p.Value != false {
		t.Errorf("value = %v, want false", p.Value)
	}
}

// TestNoPushSettersDoNotEmit pins down the scoping of the
// notification side: only the cluster-id and cluster-auto-sync
// setters fire pushes. Touching force-ports or cluster-friendly-name
// must not produce a connection/* frame, because those settings have
// no live-connection consumers.
func TestNoPushSettersDoNotEmit(t *testing.T) {
	m, rw, _ := newTestManager(t)

	_ = callAndDecode[map[string]bool](t, m, rw, 1, "settings/set-force-ports", map[string]bool{"value": true})
	assertNoNotification(t, rw)

	_ = callAndDecode[map[string]bool](t, m, rw, 2, "settings/set-cluster-friendly-name", map[string]string{"value": "anything"})
	assertNoNotification(t, rw)
}

// TestRunEmitsOnlyReadyOnStartup pins the "no startup blast" contract
// for the connection/* pushes. The spec is explicit: those
// notifications fire on CHANGE, with getters covering React-init /
// late-attach. Pre-seeding non-default cluster values would have
// produced visible pushes under a startup-emit design; the test
// asserts nothing arrives beyond `ready`.
func TestRunEmitsOnlyReadyOnStartup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	seed := `{"cluster_id":"cluster-xyz","cluster_friendly_name":"Lab 3","cluster_auto_sync":true}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}
	rw := newCaptureRW()
	m, err := NewManager(NewCodec(rw), path)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = m.Run(ctx)
		close(done)
	}()

	ready := expectNotification(t, rw, "ready")
	var rp ReadyParams
	if err := json.Unmarshal(ready.Params, &rp); err != nil {
		t.Fatalf("decode ready: %v", err)
	}
	if rp.Version == "" {
		t.Errorf("ready.version is empty")
	}

	assertNoNotification(t, rw)

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestForcePortsRoundTrip locks down the (boolean) force-ports setting.
// This setting is a single boolean policy switch for managed port ownership.
// The actual safe claim/move logic lives in the broker and engine manager.
func TestForcePortsRoundTrip(t *testing.T) {
	m, rw, path := newTestManager(t)

	got := callAndDecode[map[string]bool](t, m, rw, 1, "settings/get-force-ports", nil)
	if got["value"] != true {
		t.Fatalf("default force-ports = %v, want true", got["value"])
	}

	ok := callAndDecode[map[string]bool](t, m, rw, 2, "settings/set-force-ports", map[string]bool{"value": true})
	if !ok["ok"] {
		t.Fatalf("set result = %+v", ok)
	}

	got = callAndDecode[map[string]bool](t, m, rw, 3, "settings/get-force-ports", nil)
	if got["value"] != true {
		t.Fatalf("after set, value = %v, want true", got["value"])
	}

	// And flipping back to false must persist too — it's the
	// difference between the default and an explicitly-set "off",
	// which on-disk both serialize as `false` but exercise the
	// save path either way.
	_ = callAndDecode[map[string]bool](t, m, rw, 4, "settings/set-force-ports", map[string]bool{"value": false})
	got = callAndDecode[map[string]bool](t, m, rw, 5, "settings/get-force-ports", nil)
	if got["value"] != false {
		t.Fatalf("after clear, value = %v, want false", got["value"])
	}

	// A saved opt-out must beat the default-on policy after restart.
	rw2 := newCaptureRW()
	m2, err := NewManager(NewCodec(rw2), path)
	if err != nil {
		t.Fatalf("reload manager: %v", err)
	}
	got = callAndDecode[map[string]bool](t, m2, rw2, 6, "settings/get-force-ports", nil)
	if got["value"] != false {
		t.Fatalf("reloaded force-ports = %v, want explicit false", got["value"])
	}
}

// TestSaveFailureRejectsValueAndDoesNotPersist locks down the
// copy-then-save-then-commit contract of the setters: when save()
// fails, the attempted value must NOT be observable in-memory (a
// follow-up getter still sees the old value) and must NOT have
// landed on disk (a later, successful set proves the rejected key
// is still at its default in the persisted file).
//
// We simulate the save failure at the right layer by pre-occupying
// `<path>.tmp` with a directory, which makes the inner os.WriteFile
// fail with EISDIR regardless of platform or umask. Removing the
// blocker restores normal save() semantics for the rest of the
// test.
func TestSaveFailureRejectsValueAndDoesNotPersist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	// Block the temp-file slot save() writes through. Both 0o755 and
	// 0o700 work here; the dir's mode doesn't matter, only its
	// existence-as-a-directory at exactly the path WriteFile is
	// about to use.
	if err := os.MkdirAll(path+".tmp", 0o700); err != nil {
		t.Fatalf("seed blocker dir: %v", err)
	}

	rw := newCaptureRW()
	m, err := NewManager(NewCodec(rw), path)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Set must be rejected: handleMessage returns -32603 when
	// save() errors out, BEFORE the in-memory state is touched.
	callExpectError(t, m, rw, 1, "settings/set-cluster-id",
		map[string]string{"value": "cluster-zzz"}, -32603)

	// In-memory state still reflects the default — the rejected
	// value must not be visible to a subsequent getter, even on the
	// same manager instance.
	got := callAndDecode[map[string]string](t, m, rw, 2, "settings/get-cluster-id", nil)
	if got["value"] != "" {
		t.Errorf("rejected cluster-id leaked into in-memory state: got %q, want \"\"", got["value"])
	}

	// Remove the blocker so the next save() can succeed. We use a
	// different setter (force-ports) so the persisted file proves
	// "cluster_id was never written" rather than "the file happens
	// to record cluster_id: \"\" (the default)".
	if err := os.RemoveAll(path + ".tmp"); err != nil {
		t.Fatalf("clear blocker: %v", err)
	}
	_ = callAndDecode[map[string]bool](t, m, rw, 3, "settings/set-force-ports",
		map[string]bool{"value": true})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var onDisk Settings
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatalf("decode settings: %v\nraw: %s", err, data)
	}
	if onDisk.ClusterID != "" {
		t.Errorf("rejected cluster_id was persisted to disk: %+v", onDisk)
	}
	if !onDisk.ForcePorts {
		t.Errorf("subsequent successful set was not persisted: %+v", onDisk)
	}
}

func TestPersistenceRoundTripAcrossManagerInstances(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	rw1 := newCaptureRW()
	m1, err := NewManager(NewCodec(rw1), path)
	if err != nil {
		t.Fatalf("NewManager (first): %v", err)
	}
	_ = callAndDecode[map[string]bool](t, m1, rw1, 1, "settings/set-cluster-auto-sync", map[string]bool{"value": true})
	_ = callAndDecode[map[string]bool](t, m1, rw1, 2, "settings/set-cluster-id", map[string]string{"value": "cluster-abc"})
	_ = callAndDecode[map[string]bool](t, m1, rw1, 3, "settings/set-cluster-friendly-name", map[string]string{"value": "Lab 3 desks"})
	_ = callAndDecode[map[string]bool](t, m1, rw1, 4, "settings/set-force-ports", map[string]bool{"value": true})

	// A second Manager pointed at the same file must see everything
	// the first one wrote — proves load() correctly hydrates settings.
	rw2 := newCaptureRW()
	m2, err := NewManager(NewCodec(rw2), path)
	if err != nil {
		t.Fatalf("NewManager (second): %v", err)
	}

	sync := callAndDecode[map[string]bool](t, m2, rw2, 1, "settings/get-cluster-auto-sync", nil)
	if sync["value"] != true {
		t.Fatalf("cluster-auto-sync lost: %v", sync)
	}
	id := callAndDecode[map[string]string](t, m2, rw2, 2, "settings/get-cluster-id", nil)
	if id["value"] != "cluster-abc" {
		t.Fatalf("cluster-id lost or mangled: %q", id["value"])
	}
	name := callAndDecode[map[string]string](t, m2, rw2, 3, "settings/get-cluster-friendly-name", nil)
	if name["value"] != "Lab 3 desks" {
		t.Fatalf("cluster-friendly-name lost or mangled: %q", name["value"])
	}
	ports := callAndDecode[map[string]bool](t, m2, rw2, 4, "settings/get-force-ports", nil)
	if ports["value"] != true {
		t.Fatalf("force-ports lost: %v", ports["value"])
	}
}

// TestSchemaEvolutionIgnoresUnknownKeysIncludingRemovedSettings
// pins the on-disk schema-evolution contract: unknown keys on load
// are silently dropped (Go's default unmarshal behavior), and
// they're also NOT re-emitted on the next save. Three classes
// are exercised:
//
//   - "cluster_secret" / "auto_join_invites" — explicitly removed
//     settings. They must round-trip out of an older settings.json
//     without resurfacing.
//   - "cluster_identity" — a hypothetical earlier-draft field that
//     never actually shipped; tested defensively to catch any
//     accidental aliasing.
//   - "future_setting" — any unknown future key. Same fate.
func TestSchemaEvolutionIgnoresUnknownKeysIncludingRemovedSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	raw := []byte(`{
  "auto_join_invites": true,
  "cluster_secret": "shh",
  "cluster_identity": "some-old-derivation",
  "future_setting": 42,
  "cluster_id": "kept",
  "cluster_friendly_name": "also kept"
}`)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	rw := newCaptureRW()
	m, err := NewManager(NewCodec(rw), path)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Known fields survived.
	id := callAndDecode[map[string]string](t, m, rw, 1, "settings/get-cluster-id", nil)
	if id["value"] != "kept" {
		t.Fatalf("cluster-id = %q, want %q", id["value"], "kept")
	}
	name := callAndDecode[map[string]string](t, m, rw, 2, "settings/get-cluster-friendly-name", nil)
	if name["value"] != "also kept" {
		t.Fatalf("cluster-friendly-name = %q, want %q", name["value"], "also kept")
	}

	// Force a save by toggling something unrelated, then assert the
	// re-serialized file no longer contains any of the unknown keys.
	_ = callAndDecode[map[string]bool](t, m, rw, 3, "settings/set-cluster-auto-sync", map[string]bool{"value": false})

	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read file: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(saved, &asMap); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	for _, removed := range []string{"auto_join_invites", "cluster_secret", "cluster_identity", "future_setting"} {
		if _, ok := asMap[removed]; ok {
			t.Errorf("removed/unknown key %q survived save(): %s", removed, saved)
		}
	}
}

// TestLoadMalformedFileRenamesAsideAndStartsWithDefaults locks down
// the degraded-but-responsive load path: a corrupt settings.json must
// NOT block the subprocess from starting, must get renamed aside so
// the user can recover it, and the in-memory state must be defaults.
// Without this, a torn file (e.g. crash mid-save on an earlier
// version) wedges every settings call in the UI behind a parse error
// until the user finds and deletes the file by hand.
func TestLoadMalformedFileRenamesAsideAndStartsWithDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("not valid json {{{"), 0o644); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	rw := newCaptureRW()
	m, err := NewManager(NewCodec(rw), path)
	if err != nil {
		t.Fatalf("NewManager must not error on malformed file, got %v", err)
	}

	// In-memory state is the zero-value defaults, not whatever was
	// in the bad file.
	id := callAndDecode[map[string]string](t, m, rw, 1, "settings/get-cluster-id", nil)
	if id["value"] != "" {
		t.Errorf("expected default cluster-id to be empty, got %q", id["value"])
	}
	fp := callAndDecode[map[string]bool](t, m, rw, 2, "settings/get-force-ports", nil)
	if fp["value"] != true {
		t.Errorf("expected default force-ports to be true, got %v", fp["value"])
	}

	// The corrupt file got renamed aside with a .corrupt-<ts>
	// suffix. The original path is gone (the next save will
	// recreate it from defaults).
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	foundBackup := false
	foundOriginal := false
	for _, e := range entries {
		if e.Name() == "settings.json" {
			foundOriginal = true
		}
		if strings.HasPrefix(e.Name(), "settings.json.corrupt-") {
			foundBackup = true
		}
	}
	if !foundBackup {
		t.Errorf("expected a settings.json.corrupt-* backup in %s, got %v", dir, entries)
	}
	if foundOriginal {
		t.Errorf("expected the bad settings.json to have been renamed away, but it's still present")
	}

	// A subsequent save MUST succeed and re-create the file with
	// valid JSON — proving the degraded path is genuinely
	// recoverable, not just non-fatal-on-load.
	_ = callAndDecode[map[string]bool](t, m, rw, 3, "settings/set-cluster-id", map[string]string{"value": "cluster-zzz"})
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read recovered file: %v", err)
	}
	var asMap map[string]any
	if err := json.Unmarshal(saved, &asMap); err != nil {
		t.Fatalf("recovered file is not valid JSON: %v\nraw: %s", err, saved)
	}
	if asMap["cluster_id"] != "cluster-zzz" {
		t.Errorf("recovered file missing the new setting: %s", saved)
	}
}

func TestTypeValidationRejectsWrongTypeAndMissingValue(t *testing.T) {
	m, rw, _ := newTestManager(t)

	// Wrong type for a bool field.
	m.handleMessage(requestMessageRaw(1, "settings/set-cluster-auto-sync",
		json.RawMessage(`{"value":"not-a-bool"}`)))
	resp := readCaptureFrame(t, rw)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("wrong-type bool error = %+v", resp.Error)
	}

	// Missing `value` field.
	m.handleMessage(requestMessageRaw(2, "settings/set-cluster-auto-sync",
		json.RawMessage(`{}`)))
	resp = readCaptureFrame(t, rw)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("missing-value error = %+v", resp.Error)
	}

	// Wrong type for cluster-id (number where string expected).
	m.handleMessage(requestMessageRaw(3, "settings/set-cluster-id",
		json.RawMessage(`{"value":123}`)))
	resp = readCaptureFrame(t, rw)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("wrong-type cluster-id error = %+v", resp.Error)
	}

	// Wrong type for cluster-friendly-name (bool where string expected).
	m.handleMessage(requestMessageRaw(4, "settings/set-cluster-friendly-name",
		json.RawMessage(`{"value":true}`)))
	resp = readCaptureFrame(t, rw)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("wrong-type cluster-friendly-name error = %+v", resp.Error)
	}

	// Wrong type for the force-ports bool (a string).
	m.handleMessage(requestMessageRaw(5, "settings/set-force-ports",
		json.RawMessage(`{"value":"on"}`)))
	resp = readCaptureFrame(t, rw)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("wrong-type force-ports error = %+v", resp.Error)
	}
}

func TestUnknownMethodReturnsMethodNotFound(t *testing.T) {
	m, rw, _ := newTestManager(t)
	callExpectError(t, m, rw, 1, "bogus/method", nil, -32601)
}

func TestLogSetLevelRequestAndNotification(t *testing.T) {
	m, rw, _ := newTestManager(t)

	// Request form: replies with {"level": "debug"}.
	m.handleMessage(requestMessage(1, applog.SetLevelMethod, applog.SetLevelParams{Level: "debug"}))
	resp := readCaptureFrame(t, rw)
	result := decodeResult[map[string]string](t, resp)
	if result["level"] != "debug" {
		t.Fatalf("log/set-level result = %#v", result)
	}

	// Invalid level: -32602 in request form.
	m.handleMessage(requestMessage(2, applog.SetLevelMethod, applog.SetLevelParams{Level: "shouty"}))
	resp = readCaptureFrame(t, rw)
	if resp.Error == nil || resp.Error.Code != -32602 {
		t.Fatalf("invalid log/set-level error = %+v", resp.Error)
	}

	// Notification form (no id) — applied silently, no response frame.
	m.handleMessage(notificationMessage(applog.SetLevelMethod, applog.SetLevelParams{Level: "info"}))
	select {
	case f := <-rw.responses:
		t.Fatalf("log/set-level notification should not respond, got: %s", f)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestNotificationIsIgnored(t *testing.T) {
	m, rw, _ := newTestManager(t)

	m.handleMessage(notificationMessage("settings/set-cluster-id", map[string]string{"value": "cluster-zzz"}))

	// State should be unchanged (no response, and a get returns the default).
	got := callAndDecode[map[string]string](t, m, rw, 1, "settings/get-cluster-id", nil)
	if got["value"] != "" {
		t.Fatalf("notification mutated state: %v", got)
	}
}

func TestShutdownRequestCancelsRun(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	mgr, err := NewManager(NewCodec(server), path)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- mgr.Run(ctx)
	}()

	reader := bufio.NewReader(client)
	ready := readPipeFrame(t, client, reader)
	if ready.Method != "ready" {
		t.Fatalf("first frame = %+v", ready)
	}

	writePipeRequest(t, client, 7, "shutdown", nil)
	resp := readPipeFrame(t, client, reader)
	if !responseWithID(7)(resp) || resp.Error != nil {
		t.Fatalf("shutdown response = %+v", resp)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
}

func readPipeFrame(t *testing.T, conn net.Conn, reader *bufio.Reader) Message {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read pipe frame: %v", err)
	}
	var msg Message
	if err := json.Unmarshal(line, &msg); err != nil {
		t.Fatalf("decode pipe frame %q: %v", line, err)
	}
	return msg
}

func writePipeRequest(t *testing.T, conn net.Conn, id int, method string, params any) {
	t.Helper()
	var raw json.RawMessage
	if params != nil {
		var err error
		raw, err = json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
	}
	msg := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      int             `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  raw,
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	data = append(data, '\n')
	if _, err := conn.Write(data); err != nil {
		t.Fatalf("write request: %v", err)
	}
}
