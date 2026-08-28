// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"nvpair-shared/errors"
	"nvpair-shared/jsonrpc"
)

// TestErrorsBrokerPipeline_EndToEnd spawns the real nvpair-errors binary
// and drives it through the entire broker-side workflow over real
// stdio (so it exercises the JSON-RPC framing, codec writes, and
// notification/response demultiplexing that the in-process unit
// tests in nvpair-errors/manager_test.go cannot reach).
//
// The test plays the BROKER role itself: writes requests/notifications
// to nvpair-errors's stdin, reads responses + pushes off its stdout.
// Walks the lifecycle the plan calls out:
//   - report  →  update broadcast (response form)
//   - older-timestamp report  →  dropped, NO update
//   - newer-timestamp report  →  update broadcast (notification form)
//   - get-initial round-trip carries the current entry
//   - clear   →  update broadcast with empty list
//   - re-report after clear  →  resurrect (ack-until-reemit)
func TestErrorsBrokerPipeline_EndToEnd(t *testing.T) {
	cmd := exec.Command(errorsBin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start nvpair-errors: %v", err)
	}
	// Defer-close stdin so that even on t.Fatal mid-test the
	// subprocess sees EOF and exits cleanly rather than orphaning.
	defer func() {
		stdin.Close()
		_ = cmd.Wait()
	}()

	out := startBrokerStream(stdout)

	// First frame: the subprocess emits "ready" right after startup.
	waitForMethodOrTimeout(t, out, "ready", 5*time.Second)

	// The broker stamps every local report/clear with the local node
	// id (os.Hostname()), which is exactly what nvpair-errors resolves as
	// its own localNodeID. We mirror that here so the clear step (now
	// scoped to local-origin entries for cross-node correctness)
	// resolves against the reports — a fixed fake nodeId would no
	// longer match the binary's real hostname.
	localNode, _ := os.Hostname()

	// 1. errors:report (REQUEST form, id=1). Expect a null response,
	//    then an errors:update notification with the new entry.
	const upstreamID = "ollama-proxy:upstream-unreachable:peer-A"
	first := errors.ServiceError{
		ID:        upstreamID,
		Message:   "first emit",
		Timestamp: 1000,
		NodeID:    localNode,
	}
	writeRequest(t, stdin, 1, "errors:report", first)
	resp1 := waitForResponseOrTimeout(t, out, 5*time.Second)
	if string(*resp1.ID) != "1" {
		t.Fatalf("expected response id=1, got %s", string(*resp1.ID))
	}
	got1 := waitForUpdate(t, out, 5*time.Second)
	if len(got1) != 1 || got1[0].ID != upstreamID || got1[0].Message != "first emit" {
		t.Fatalf("first update payload = %+v, want single entry matching first emit", got1)
	}

	// 2. errors:report (NOTIFICATION form) with an OLDER timestamp.
	//    Must be dropped — no errors:update push.
	writeNotification(t, stdin, "errors:report", errors.ServiceError{
		ID: upstreamID, Message: "stale", Timestamp: 500, NodeID: localNode,
	})
	assertNoUpdate(t, out, 250*time.Millisecond)

	// 3. errors:report (NOTIFICATION form) with a NEWER timestamp.
	//    Replaces the prior entry; update fires.
	writeNotification(t, stdin, "errors:report", errors.ServiceError{
		ID: upstreamID, Message: "second emit", Timestamp: 2000, NodeID: localNode,
	})
	got3 := waitForUpdate(t, out, 5*time.Second)
	if len(got3) != 1 || got3[0].Message != "second emit" {
		t.Fatalf("second update payload = %+v, want second emit", got3)
	}

	// 4. errors:get-initial round-trip. Returns the current full list.
	writeRequest(t, stdin, 2, "errors:get-initial", nil)
	resp4 := waitForResponseOrTimeout(t, out, 5*time.Second)
	if string(*resp4.ID) != "2" {
		t.Fatalf("expected response id=2, got %s", string(*resp4.ID))
	}
	var initial []errors.ServiceError
	if err := json.Unmarshal(resp4.Result, &initial); err != nil {
		t.Fatalf("decode get-initial result: %v", err)
	}
	if len(initial) != 1 || initial[0].ID != upstreamID || initial[0].Message != "second emit" {
		t.Fatalf("get-initial = %+v, want single entry matching second emit", initial)
	}

	// 5. errors:clear (REQUEST form). Response + update with empty list.
	//    ClearedBy is the broker-stamped field — nvpair-errors stores
	//    nothing from it (the field exists for cross-node clear
	//    propagation) but accepts and processes the clear.
	writeRequest(t, stdin, 3, "errors:clear", errors.ClearParams{
		ID: upstreamID, ClearedBy: localNode,
	})
	resp5 := waitForResponseOrTimeout(t, out, 5*time.Second)
	if string(*resp5.ID) != "3" {
		t.Fatalf("expected response id=3, got %s", string(*resp5.ID))
	}
	got5 := waitForUpdate(t, out, 5*time.Second)
	if len(got5) != 0 {
		t.Fatalf("post-clear update = %+v, want empty list", got5)
	}

	// 6. Re-emit the same id with a NEWER timestamp than the cleared
	//    one. Ack-until-reemit: the user's clear is in-memory only,
	//    so a fresh producer emit resurrects the entry.
	writeNotification(t, stdin, "errors:report", errors.ServiceError{
		ID: upstreamID, Message: "resurrected", Timestamp: 3000, NodeID: localNode,
	})
	got6 := waitForUpdate(t, out, 5*time.Second)
	if len(got6) != 1 || got6[0].Message != "resurrected" {
		t.Fatalf("post-reemit update = %+v, want resurrected entry", got6)
	}

	// 7. Add a SECOND id to confirm the update payload is always the
	//    full list, sorted by id. Multi-id ordering is a unit-test
	//    concern too, but exercising it through real IPC catches any
	//    framing bug that drops or reorders frames.
	const probeID = "manual-nodes:probe-failed:peer-Z"
	writeNotification(t, stdin, "errors:report", errors.ServiceError{
		ID: probeID, Message: "manual node down", Timestamp: 4000, NodeID: localNode,
	})
	got7 := waitForUpdate(t, out, 5*time.Second)
	if len(got7) != 2 {
		t.Fatalf("two-id update len = %d, want 2", len(got7))
	}
	if !sort.SliceIsSorted(got7, func(i, j int) bool { return got7[i].ID < got7[j].ID }) {
		t.Fatalf("update payload not sorted by id: %+v", got7)
	}
}

// --- helpers, scoped to this test file ---

// startBrokerStream is a jsonrpc.Message-emitting version of
// startMsgReader. Inline to avoid touching the shared helper, which
// other tests use with the slimmer jsonrpc.Message shape.
func startBrokerStream(r io.Reader) <-chan jsonrpc.Message {
	ch := make(chan jsonrpc.Message, 64)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)
		for scanner.Scan() {
			var msg jsonrpc.Message
			if json.Unmarshal(scanner.Bytes(), &msg) == nil {
				ch <- msg
			}
		}
		// Scanner exits on EOF, ErrTooLong, or an underlying
		// read error. Surface the read-error case so a hung
		// subprocess doesn't masquerade as a clean close — but
		// only as a log line, since failing the test from a
		// goroutine on EOF-vs-error confusion would mask the
		// real assertion failure.
		_ = scanner.Err()
	}()
	return ch
}

var writeMu sync.Mutex

func writeRequest(t *testing.T, w io.Writer, id int, method string, params any) {
	t.Helper()
	idData := json.RawMessage(strconv.Itoa(id))
	writeFrame(t, w, jsonrpc.Message{
		JSONRPC: "2.0",
		ID:      &idData,
		Method:  method,
		Params:  mustMarshal(t, params),
	})
}

func writeNotification(t *testing.T, w io.Writer, method string, params any) {
	t.Helper()
	writeFrame(t, w, jsonrpc.Message{
		JSONRPC: "2.0",
		Method:  method,
		Params:  mustMarshal(t, params),
	})
}

func writeFrame(t *testing.T, w io.Writer, msg jsonrpc.Message) {
	t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	if _, err := w.Write(append(data, '\n')); err != nil {
		t.Fatalf("write frame: %v", err)
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	return data
}

// waitForMethodOrTimeout reads frames until one with the given method
// arrives, or the timeout fires.
func waitForMethodOrTimeout(t *testing.T, ch <-chan jsonrpc.Message, method string, timeout time.Duration) jsonrpc.Message {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				t.Fatalf("stream closed before receiving %q", method)
			}
			if msg.Method == method {
				return msg
			}
		case <-timer.C:
			t.Fatalf("timed out (%s) waiting for method %q", timeout, method)
		}
	}
}

// waitForResponseOrTimeout reads frames until a JSON-RPC response
// arrives (id non-empty, method empty). Fails the test on RPC error
// — the broker pipeline test only sends inputs that should succeed.
func waitForResponseOrTimeout(t *testing.T, ch <-chan jsonrpc.Message, timeout time.Duration) jsonrpc.Message {
	t.Helper()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				t.Fatal("stream closed before receiving response")
			}
			if msg.ID != nil && msg.Method == "" {
				if msg.Error != nil {
					t.Fatalf("unexpected RPC error response: code=%d msg=%q", msg.Error.Code, msg.Error.Message)
				}
				return msg
			}
		case <-timer.C:
			t.Fatal("timed out waiting for JSON-RPC response")
		}
	}
}

// waitForUpdate reads frames until an errors:update notification
// arrives and decodes its payload. Skips any responses or other
// notifications in the meantime — the test asserts ordering by what
// it sends, not by stream position.
func waitForUpdate(t *testing.T, ch <-chan jsonrpc.Message, timeout time.Duration) []errors.ServiceError {
	t.Helper()
	msg := waitForMethodOrTimeout(t, ch, "errors:update", timeout)
	if len(msg.Params) == 0 || string(msg.Params) == "null" {
		return nil
	}
	var list []errors.ServiceError
	if err := json.Unmarshal(msg.Params, &list); err != nil {
		t.Fatalf("decode errors:update payload: %v (raw=%s)", err, msg.Params)
	}
	return list
}

// assertNoUpdate confirms that no errors:update arrives within the
// given window. The dropped-stale-report case relies on this: a
// no-op upsert must NOT broadcast or the UI would re-render on
// every dropped stale frame.
func assertNoUpdate(t *testing.T, ch <-chan jsonrpc.Message, window time.Duration) {
	t.Helper()
	timer := time.NewTimer(window)
	defer timer.Stop()
	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			if msg.Method == "errors:update" {
				t.Fatalf("unexpected errors:update during quiet window: %s", msg.Params)
			}
		case <-timer.C:
			return
		}
	}
}
