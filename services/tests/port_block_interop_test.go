// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"nvpair-shared/jsonrpc"
	"nvpair-shared/noderec"
)

// TestBroker_ConcurrentBrokersOnSeparateBlocks_BothComeUpClean is the #226
// regression gate: two brokers — standing in for a live PAIR and a test run —
// coexist on one machine when each was handed its own six-port block. Before
// --port-base the second broker's workers collided with the first's fixed
// 14318-14323 block, and the suite had to skip while PAIR was running.
func TestBroker_ConcurrentBrokersOnSeparateBlocks_BothComeUpClean(t *testing.T) {
	baseA := newPortBlock(t)
	baseB := newPortBlock(t)
	require.NotEqual(t, baseA, baseB, "the two brokers must not be handed the same block")

	// Each broker supervises the three workers whose binds proved the block
	// contract: node-info (ni), errors (er), and workload-manager (wl). A
	// worker that cannot bind its handed port is fatal to the spawn, so a
	// broker that reaches app:ready — and stays alive to answer — proves all
	// three binds landed inside the block it was given.
	//
	// The streams must NOT be drained in the background here: waitForMethod
	// and waitForResponseID are the consumers, and a competing reader would
	// simply steal their notifications.
	start := func(base int) (io.WriteCloser, <-chan jsonrpc.Message, func()) {
		stdin, msgs, _, cleanup := startBrokerWith(t,
			"--port-base", strconv.Itoa(base),
			"--node-info-path", nodeInfoBin,
			"--errors-path", errorsBin,
			"--workload-manager-path", workloadMgrBin,
		)
		return stdin, msgs, cleanup
	}
	stdinA, msgsA, cleanupA := start(baseA)
	t.Cleanup(cleanupA)
	stdinB, msgsB, cleanupB := start(baseB)
	t.Cleanup(cleanupB)

	waitForMethod(t, msgsA, "app:ready", 15*time.Second)
	waitForMethod(t, msgsB, "app:ready", 15*time.Second)

	// Each broker answers its own request: the two streams stayed distinct and
	// neither process died mid-handshake.
	sendReq(t, stdinA, 1, "discovery:get-nodes")
	respA := waitForResponseID(t, msgsA, 1, 10*time.Second)
	require.Nil(t, respA.Error, "broker A discovery:get-nodes")
	sendReq(t, stdinB, 1, "discovery:get-nodes")
	respB := waitForResponseID(t, msgsB, 1, 10*time.Second)
	require.Nil(t, respB.Error, "broker B discovery:get-nodes")

	// Load-bearing proof of the hand-off: each broker's ni worker answers HTTP
	// on the ni port of ITS OWN block. The canonical block is deliberately not
	// probed here — a live PAIR answering on it must not decide this test.
	for _, base := range []int{baseA, baseB} {
		ni := blockHostPortFor(t, base, noderec.ServiceNodeInfo)
		waitTCP(t, ni, 15*time.Second)
		resp, err := http.Get("http://" + ni + "/")
		require.NoErrorf(t, err, "broker on base %d: node-info must answer on its own ni port", base)
		resp.Body.Close()
	}
}

// blockHostPortFor renders the block port of key under an explicit base — for
// brokers started with a block other than the process-wide one.
func blockHostPortFor(t *testing.T, base int, key noderec.ServiceKey) string {
	t.Helper()
	port, ok := noderec.ServicePort(base, key)
	require.True(t, ok, "service %s is not in the port block", key)
	return "127.0.0.1:" + strconv.Itoa(port)
}
