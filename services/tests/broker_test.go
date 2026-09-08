// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process test for nvpair-ui-broker's opt-in discovery notification
// stream (discovery:subscribe / discovery:unsubscribe).
//
// The broker spawns nvpair-node-scanner, whose promoted daemon browses
// _nvpair-node._tcp and folds each node into the broker's discovery store even
// when the /v1/node-info enrichment fetch fails. So we drive real discovery by
// registering a bare _nvpair-node record via zeroconf (ni=<port> in TXT, no HTTP
// node-info server required) and assert on the broker's stdout JSON-RPC frames:
//
//   - while unsubscribed: the node is reachable via discovery:get-nodes
//     but the broker pushes NO discovery:nodes-changed;
//   - on discovery:subscribe: an ack plus an immediate baseline
//     discovery:nodes-changed snapshot;
//   - while subscribed: a newly-advertised node produces a live
//     discovery:nodes-changed push;
//   - after discovery:unsubscribe: a further newly-advertised node is
//     still discovered (visible via get-nodes) but is NOT pushed.
//
// Both broker and scanner binaries are built by TestMain.
package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"nvpair-shared/jsonrpc"

	"github.com/grandcat/zeroconf"
)

// availableNode mirrors the broker's external camelCase wire shape (see
// AvailableNode in nvpair-ui-broker).
type availableNode struct {
	ID             string              `json:"id"`
	Name           string              `json:"name"`
	HostUUID       string              `json:"hostUuid"`
	IPAddress      string              `json:"ipAddress"`
	Port           int                 `json:"port"`
	LastSeen       int64               `json:"lastSeen"`
	Trusted        bool                `json:"trusted"`
	Clustered      bool                `json:"clustered"`
	Models         []string            `json:"models,omitempty"`
	ModelsByEngine map[string][]string `json:"modelsByEngine,omitempty"`
	LoadedByEngine map[string][]string `json:"loadedByEngine,omitempty"`
}

type availableNodesResult struct {
	Nodes []availableNode `json:"nodes"`
}

type subscriptionResult struct {
	Subscribed bool `json:"subscribed"`
}

func startBroker(t *testing.T) (stdin io.WriteCloser, msgs <-chan jsonrpc.Message, cleanup func()) {
	t.Helper()
	cmd := exec.Command(brokerBin, "--scanner-path", scannerBin, "--cluster-dir", t.TempDir())
	cmd.Stderr = os.Stderr

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("broker stdin pipe: %v", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("broker stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	t.Logf("broker started: pid=%d", cmd.Process.Pid)

	ch := startMsgReader(stdoutPipe)

	return stdinPipe, ch, func() {
		stdinPipe.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second): // broker also tears down the scanner (2s grace)
			cmd.Process.Kill()
			<-done
		}
	}
}

func sendReq(t *testing.T, w io.Writer, id int, method string) {
	t.Helper()
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q}`, id, method) + "\n"
	if _, err := w.Write([]byte(req)); err != nil {
		t.Fatalf("write %s request: %v", method, err)
	}
}

func registerNodeInfo(t *testing.T, instance string, port int) *zeroconf.Server {
	t.Helper()
	// One consolidated _nvpair-node record carrying ni=<port> (the node-info
	// service). A distinct uuid keeps each stub from colliding with this node's
	// own record or the others. The daemon browses this, folds it into its
	// directory, and pushes it to the broker store.
	txt := []string{"v=1", "uuid=" + instance + "-uuid", fmt.Sprintf("ni=%d", port)}
	srv, err := zeroconf.Register(instance, nodeRecordService, testDomain, port, txt, nil)
	if err != nil {
		t.Fatalf("register %s: %v", instance, err)
	}
	t.Cleanup(srv.Shutdown)
	t.Logf("advertising %s @ %s (ni=%d)", instance, nodeRecordService, port)
	return srv
}

func containsNode(nodes []availableNode, id string) bool {
	for _, n := range nodes {
		if n.ID == id {
			return true
		}
	}
	return false
}

// pollNodeListed repeatedly issues discovery:get-nodes until `instance`
// appears in the snapshot. Every discovery:nodes-changed seen in the
// meantime is passed to allowPush; if allowPush returns false the test
// fails (used to assert the stream is silent while unsubscribed).
func pollNodeListed(t *testing.T, stdin io.Writer, msgs <-chan jsonrpc.Message, instance string, baseReqID int, timeout time.Duration, allowPush func([]availableNode) bool) {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()

	id := baseReqID
	sendReq(t, stdin, id, "discovery:get-nodes")
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker stream closed unexpectedly")
			}
			switch {
			case msg.Method == "discovery:nodes-changed":
				var nodes []availableNode
				if err := json.Unmarshal(msg.Params, &nodes); err != nil {
					t.Fatalf("unmarshal discovery:nodes-changed: %v\nraw: %s", err, msg.Params)
				}
				if allowPush != nil && !allowPush(nodes) {
					t.Fatalf("unexpected discovery:nodes-changed: %s", msg.Params)
				}
			case msg.Method == "" && msg.ID != nil:
				var res availableNodesResult
				if json.Unmarshal(msg.Result, &res) == nil && containsNode(res.Nodes, instance) {
					return
				}
			}
		case <-ticker.C:
			id++
			sendReq(t, stdin, id, "discovery:get-nodes")
		case <-deadline:
			t.Fatalf("timed out (%s) waiting for %q in discovery:get-nodes", timeout, instance)
		}
	}
}

// waitForPushContaining blocks until a discovery:nodes-changed carrying
// `instance` arrives, failing on timeout.
func waitForPushContaining(t *testing.T, msgs <-chan jsonrpc.Message, instance string, timeout time.Duration) {
	t.Helper()
	to := time.After(timeout)
	for {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatalf("broker stream closed before push containing %q", instance)
			}
			if msg.Method == "discovery:nodes-changed" {
				var nodes []availableNode
				if err := json.Unmarshal(msg.Params, &nodes); err != nil {
					t.Fatalf("unmarshal discovery:nodes-changed: %v\nraw: %s", err, msg.Params)
				}
				if containsNode(nodes, instance) {
					return
				}
			}
		case <-to:
			t.Fatalf("timed out (%s) waiting for discovery:nodes-changed containing %q", timeout, instance)
		}
	}
}

func TestCrossProcessBrokerSubscription(t *testing.T) {
	const (
		inst1 = "xproc-broker-1"
		inst2 = "xproc-broker-2"
		inst3 = "xproc-broker-3"
		port1 = 14401
		port2 = 14402
		port3 = 14403
	)

	// Advertise the first node before the broker starts so it is in the
	// store by the time we query it.
	registerNodeInfo(t, inst1, port1)

	stdin, msgs, cleanup := startBroker(t)
	t.Cleanup(cleanup)

	waitForMethod(t, msgs, "app:ready", 5*time.Second)
	t.Log("broker ready")

	// Phase 1 — unsubscribed: the node must be visible via the on-demand
	// snapshot, but the broker must push NOTHING.
	pollNodeListed(t, stdin, msgs, inst1, 1, 25*time.Second, func([]availableNode) bool { return false })
	t.Log("phase 1 OK: node visible via get-nodes, no push while unsubscribed")

	// Phase 2 — subscribe: expect an ack {subscribed:true} and an
	// immediate baseline snapshot containing inst1.
	sendReq(t, stdin, 100, "discovery:subscribe")
	gotAck, gotBaseline := false, false
	deadline := time.After(8 * time.Second)
	for !(gotAck && gotBaseline) {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker stream closed waiting for subscribe ack/baseline")
			}
			switch {
			case msg.Method == "" && msg.ID != nil:
				var sr subscriptionResult
				if json.Unmarshal(msg.Result, &sr) == nil {
					if !sr.Subscribed {
						t.Errorf("subscribe ack subscribed=false, want true")
					}
					gotAck = true
				}
			case msg.Method == "discovery:nodes-changed":
				var nodes []availableNode
				if err := json.Unmarshal(msg.Params, &nodes); err != nil {
					t.Fatalf("unmarshal baseline: %v\nraw: %s", err, msg.Params)
				}
				if !containsNode(nodes, inst1) {
					t.Errorf("baseline snapshot missing %q: %s", inst1, msg.Params)
				}
				gotBaseline = true
			}
		case <-deadline:
			t.Fatalf("timed out waiting for subscribe ack=%v baseline=%v", gotAck, gotBaseline)
		}
	}
	t.Log("phase 2 OK: subscribe acked and baseline snapshot received")

	// Phase 3 — subscribed: a newly advertised node must be pushed.
	registerNodeInfo(t, inst2, port2)
	waitForPushContaining(t, msgs, inst2, 25*time.Second)
	t.Log("phase 3 OK: live push received for new node while subscribed")

	// Phase 4 — unsubscribe: expect an ack {subscribed:false}.
	sendReq(t, stdin, 200, "discovery:unsubscribe")
	ackd := false
	deadline = time.After(8 * time.Second)
	for !ackd {
		select {
		case msg, ok := <-msgs:
			if !ok {
				t.Fatal("broker stream closed waiting for unsubscribe ack")
			}
			// Discovery:nodes-changed frames may still be in flight here
			// for inst1/inst2 from just before the unsubscribe took
			// effect; ignore them and wait for the ack.
			if msg.Method == "" && msg.ID != nil {
				var sr subscriptionResult
				if json.Unmarshal(msg.Result, &sr) == nil {
					if sr.Subscribed {
						t.Errorf("unsubscribe ack subscribed=true, want false")
					}
					ackd = true
				}
			}
		case <-deadline:
			t.Fatal("timed out waiting for unsubscribe ack")
		}
	}
	t.Log("phase 4 OK: unsubscribe acked")

	// Phase 5 — unsubscribed again: a node advertised now must still be
	// discovered (visible via get-nodes) but must NOT be pushed. Keying
	// the negative assertion on inst3 (which only exists from now on)
	// makes it immune to any pre-unsubscribe push still draining.
	registerNodeInfo(t, inst3, port3)
	pollNodeListed(t, stdin, msgs, inst3, 300, 25*time.Second, func(nodes []availableNode) bool {
		return !containsNode(nodes, inst3)
	})
	t.Log("phase 5 OK: node discovered after unsubscribe with no push — gating confirmed")
}
