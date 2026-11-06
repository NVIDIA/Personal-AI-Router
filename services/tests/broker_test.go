// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process tests for nvpair-ui-broker's opt-in discovery notification
// stream (discovery:subscribe / discovery:unsubscribe) and hardware delivery.
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
// Hardware tests also start owned node-info or manual-node workers against
// isolated configuration directories and local test servers.
package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/jsonrpc"
	"nvpair-shared/noderec"

	"github.com/grandcat/zeroconf"
)

// availableNode mirrors the broker's external camelCase wire shape (see
// AvailableNode in nvpair-ui-broker).
type availableNode struct {
	ID                   string                       `json:"id"`
	HostUUID             string                       `json:"hostUuid"`
	DiscoveryRecordID    string                       `json:"discoveryRecordId"`
	IdentityVerification noderec.IdentityVerification `json:"identityVerification"`
	Trusted              bool                         `json:"trusted"`
	Name                 string                       `json:"name"`
	IPAddress            string                       `json:"ipAddress"`
	Port                 int                          `json:"port"`
	LastSeen             int64                        `json:"lastSeen"`
	Models               []string                     `json:"models,omitempty"`
	ModelsByEngine       map[string][]string          `json:"modelsByEngine,omitempty"`
	LoadedByEngine       map[string][]string          `json:"loadedByEngine,omitempty"`
	Info                 *noderec.NodeInfo            `json:"info,omitempty"`
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
	require.NoError(t, err, "broker stdin pipe")
	stdoutPipe, err := cmd.StdoutPipe()
	require.NoError(t, err, "broker stdout pipe")
	require.NoError(t, cmd.Start(), "start broker")
	t.Logf("broker started: pid=%d", cmd.Process.Pid)

	ch := startMsgReader(t, stdoutPipe)

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
	_, err := w.Write([]byte(req))
	require.NoError(t, err, "write %s request", method)
}

func registerNodeInfo(t *testing.T, instance string, port int) *zeroconf.Server {
	t.Helper()
	// One consolidated _nvpair-node record carrying ni=<port> (the node-info
	// service). A distinct uuid keeps each stub from colliding with this node's
	// own record or the others. The daemon browses this, folds it into its
	// directory, and pushes it to the broker store.
	txt := []string{"v=1", "uuid=" + instance + "-uuid", fmt.Sprintf("ni=%d", port)}
	srv, err := zeroconf.Register(instance, nodeRecordService, testDomain, port, txt, nil)
	require.NoError(t, err, "register %s", instance)
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
			require.True(t, ok, "broker stream closed unexpectedly")
			switch {
			case msg.Method == "discovery:nodes-changed":
				var nodes []availableNode
				require.NoError(t, json.Unmarshal(msg.Params, &nodes), "unmarshal discovery:nodes-changed")
				require.False(t, allowPush != nil && !allowPush(nodes), "unexpected discovery:nodes-changed")
			case msg.Method == "" && msg.ID != nil:
				var res availableNodesResult
				require.NoError(t, json.Unmarshal(msg.Result, &res))
				if containsNode(res.Nodes, instance) {
					return
				}
			}
		case <-ticker.C:
			id++
			sendReq(t, stdin, id, "discovery:get-nodes")
		case <-deadline:
			require.FailNow(t, fmt.Sprintf("timed out (%s) waiting for %q in discovery:get-nodes", timeout, instance))
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
			require.True(t, ok, "broker stream closed before push containing (%v)", instance)
			if msg.Method == "discovery:nodes-changed" {
				var nodes []availableNode
				require.NoError(t, json.Unmarshal(msg.Params, &nodes), "unmarshal discovery:nodes-changed")
				if containsNode(nodes, instance) {
					return
				}
			}
		case <-to:
			require.FailNow(t, fmt.Sprintf("timed out (%s) waiting for discovery:nodes-changed containing %q", timeout, instance))
		}
	}
}

func TestBroker_RelaysDiscoverySubscriptionAcrossProcesses(t *testing.T) {
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
			require.True(t, ok, "broker stream closed waiting for subscribe ack/baseline")
			switch {
			case msg.Method == "" && msg.ID != nil:
				var sr subscriptionResult
				require.NoError(t, json.Unmarshal(msg.Result, &sr))

				assert.True(t, sr.Subscribed, "subscribe ack subscribed=false, want true")
				gotAck = true

			case msg.Method == "discovery:nodes-changed":
				var nodes []availableNode
				require.NoError(t, json.Unmarshal(msg.Params, &nodes), "unmarshal baseline")
				assert.True(t, containsNode(nodes, inst1), "baseline snapshot missing (%v)", inst1)
				gotBaseline = true
			}
		case <-deadline:
			require.FailNow(t, fmt.Sprintf("timed out waiting for subscribe ack=%v baseline=%v", gotAck, gotBaseline))
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
			require.True(t, ok, "broker stream closed waiting for unsubscribe ack")
			// Discovery:nodes-changed frames may still be in flight here
			// for inst1/inst2 from just before the unsubscribe took
			// effect; ignore them and wait for the ack.
			if msg.Method == "" && msg.ID != nil {
				var sr subscriptionResult
				require.NoError(t, json.Unmarshal(msg.Result, &sr))

				assert.False(t, sr.Subscribed, "unsubscribe ack subscribed=true, want false")
				ackd = true

			}
		case <-deadline:
			require.FailNow(t, "timed out waiting for unsubscribe ack")
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

// A real node-info worker first pushes local hardware through the broker.
// A subsequent discovery query must include that cached hardware under the same
// host and record identities, without waiting for another collection cycle.
func TestBroker_IncludesLocalHardwareInQueriesAndUpdates(t *testing.T) {
	if portBusy(14318) {
		t.Skip("node-info port 14318 is already in use")
	}
	stdin, messages, _, cleanup := startBrokerWith(t, "--node-info-path", nodeInfoBin)
	t.Cleanup(cleanup)
	waitForMethod(t, messages, "app:ready", 10*time.Second)
	var sample noderec.NodeInfoEvent
	timeout := time.NewTimer(15 * time.Second)
	defer timeout.Stop()
	for sample.IdentityVerification != noderec.IdentityLocal {
		select {
		case update, ok := <-messages:
			require.True(t, ok, "broker stream closed before local hardware arrived")
			if update.Method == noderec.NotifyNodeInfo {
				require.NoError(t, json.Unmarshal(update.Params, &sample))
			}
		case <-timeout.C:
			require.FailNow(t, "broker did not push owned local hardware")
		}
	}
	require.Equal(t, noderec.IdentityLocal, sample.IdentityVerification)
	require.NotEmpty(t, sample.HostUUID)
	assert.Equal(t, sample.HostUUID, sample.Info.HostUUID)
	require.Positive(t, sample.Info.ObservedAt)

	sendReq(t, stdin, 100, "discovery:get-nodes")
	response := waitForResponseID(t, messages, 100, 5*time.Second)
	var snapshot availableNodesResult
	require.NoError(t, json.Unmarshal(response.Result, &snapshot))
	var local *availableNode
	for i := range snapshot.Nodes {
		if snapshot.Nodes[i].IdentityVerification == noderec.IdentityLocal {
			local = &snapshot.Nodes[i]
		}
	}
	require.NotNil(t, local)
	require.NotNil(t, local.Info, "the query must include hardware received before the request")
	assert.Equal(t, sample.HostUUID, local.Info.HostUUID)
	assert.Equal(t, sample.DiscoveryRecordID, local.DiscoveryRecordID)
	// The live worker may collect again before this query. Store tests check
	// exact timestamp preservation using a fixed observation.
	assert.GreaterOrEqual(t, local.Info.ObservedAt, sample.Info.ObservedAt)

	assert.Len(t, local.Info.GPUs, len(sample.Info.GPUs))
}

// A public loopback server claims a host and cluster identity. The real manual
// worker and broker must deliver its hardware under the independent manual
// record, without adopting those claims or marking the entry trusted.
func TestBroker_DeliversManualPreviewWithoutTrustingPublicIdentity(t *testing.T) {
	// Manual previews use the default node-info port. A tls_port override would
	// disable public previews, so use a fixed loopback listener and skip if busy.
	listener, err := net.Listen("tcp", "127.0.0.1:14318")
	if err != nil {
		t.Skipf("hardware preview port unavailable: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal := "unverified-claim"
		assert.NoError(t, json.NewEncoder(w).Encode(noderec.NodeInfo{
			HostUUID: principal, ClusterUUID: &principal,
			GPUs: []noderec.GPUInfo{{Name: "preview GPU"}},
		}))
	})}
	t.Cleanup(func() { assert.NoError(t, server.Close()) })
	go func() { assert.ErrorIs(t, server.Serve(listener), http.ErrServerClosed) }()
	stdin, messages, _, cleanup := startBrokerWith(t, "--manual-nodes-path", manualNodesBin)
	t.Cleanup(cleanup)
	waitForMethod(t, messages, "app:ready", 10*time.Second)
	sendReq(t, stdin, 100, "discovery:subscribe")
	request := `{"jsonrpc":"2.0","id":101,"method":"node/add","params":{"address":"127.0.0.1","name":"preview-target"}}` + "\n"
	_, err = stdin.Write([]byte(request))
	require.NoError(t, err)
	timeout := time.NewTimer(15 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case message, ok := <-messages:
			require.True(t, ok, "broker stream closed before hardware preview arrived")
			if message.Method != "discovery:nodes-changed" {
				continue
			}
			var nodes []availableNode
			require.NoError(t, json.Unmarshal(message.Params, &nodes))
			for _, node := range nodes {
				if node.DiscoveryRecordID != "manual:preview-target" || node.Info == nil {
					continue
				}
				assert.Equal(t, noderec.IdentityUnverified, node.IdentityVerification)
				assert.False(t, node.Trusted)
				assert.Equal(t, "manual:preview-target", node.HostUUID)

				// The public responder's claimed identity must not escape into
				// either the manual record key or its display hardware payload.
				assert.Empty(t, node.Info.HostUUID)
				assert.Nil(t, node.Info.ClusterUUID)

				assert.Equal(t, []noderec.GPUInfo{{Name: "preview GPU"}}, node.Info.GPUs)
				assert.Positive(t, node.Info.ObservedAt)
				return
			}
		case <-timeout.C:
			require.FailNow(t, "broker did not deliver the unpaired manual hardware preview")
		}
	}
}

// The test peer's certificate is approved in an isolated cluster directory.
// The real manual worker and broker must deliver its authenticated hardware
// under that peer's UUID, rather than an unverified manual record key.
func TestBroker_DeliversAuthenticatedManualHardwareInDiscovery(t *testing.T) {
	cluster := newInterNodeCluster(t)
	info := noderec.NodeInfo{
		HostUUID: cluster.peerUUID, ClusterUUID: &cluster.peerUUID,
		GPUs:   []noderec.GPUInfo{{Name: "paired test GPU", VramBytes: 8192}},
		CPU:    &noderec.CPUInfo{Name: "paired test CPU", Cores: 8},
		Memory: &noderec.MemoryInfo{TotalBytes: 16384}, TelemetryValid: true, MSSince: 137,
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := cluster.peer.VerifyClientPin(r); !ok {
			http.Error(w, "not a pinned peer", http.StatusForbidden)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(info))
	}))
	server.TLS = cluster.peer.ServerTLSConfig()
	server.StartTLS()
	t.Cleanup(server.Close)
	port := server.Listener.Addr().(*net.TCPAddr).Port
	stdin, messages, _, cleanup := startBrokerWithDirs(t, t.TempDir(), cluster.nodeDir, "--manual-nodes-path", manualNodesBin)
	t.Cleanup(cleanup)
	waitForMethod(t, messages, "app:ready", 10*time.Second)
	sendReq(t, stdin, 100, "discovery:subscribe")
	_, err := fmt.Fprintf(stdin, `{"jsonrpc":"2.0","id":101,"method":"node/add","params":{"address":"127.0.0.1","name":"paired-target","tls_port":%d}}`+"\n", port)
	require.NoError(t, err)
	timeout := time.NewTimer(20 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case message, ok := <-messages:
			require.True(t, ok)
			if message.Method != "discovery:nodes-changed" {
				continue
			}
			var nodes []availableNode
			require.NoError(t, json.Unmarshal(message.Params, &nodes))
			for _, node := range nodes {
				if node.HostUUID != cluster.peerUUID || node.Info == nil {
					continue
				}
				assert.Equal(t, noderec.IdentityPinned, node.IdentityVerification)
				assert.True(t, node.Trusted)

				assert.Equal(t, cluster.peerUUID, node.Info.HostUUID)
				require.NotNil(t, node.Info.ClusterUUID)
				assert.Equal(t, cluster.peerUUID, *node.Info.ClusterUUID)

				// Identity-only responses cannot prove that the hardware payload
				// survives the worker and broker's field projections.
				assert.Equal(t, info.GPUs, node.Info.GPUs)
				assert.Equal(t, info.CPU, node.Info.CPU)
				assert.Equal(t, info.Memory, node.Info.Memory)
				assert.True(t, node.Info.TelemetryValid)
				assert.GreaterOrEqual(t, node.Info.MSSince, int64(137))
				assert.Positive(t, node.Info.ObservedAt)
				return
			}
		case <-timeout.C:
			require.FailNow(t, "broker did not deliver pinned manual hardware")
		}
	}
}
