// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"crypto/tls"
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"nvpair-shared/netpick"
	"nvpair-shared/noderec"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestScanner_DiscoversAndEnrichesAPairedPeerWithoutMulticast(t *testing.T) {
	cluster := newInterNodeCluster(t)
	address := ""
	for _, candidate := range netpick.LocalCandidates(netpick.Evidence{}) {
		ip := net.ParseIP(candidate)
		if ip != nil && ip.To4() != nil && !ip.IsLoopback() {
			address = candidate
			break
		}
	}
	if address == "" {
		t.Skip("a non-loopback IPv4 address is required to exercise peer mTLS rather than self loopback HTTP")
	}
	models := http.NewServeMux()
	models.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := cluster.peer.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"models": []string{"org/cached"}, "modelsByEngine": map[string][]string{"vllm": {"org/cached"}}, "loadedByEngine": map[string][]string{"vllm": {}}, "downloadedByEngine": map[string][]string{"vllm": {"org/cached"}}}))
	})
	listener, err := net.Listen("tcp", net.JoinHostPort(address, "0"))
	require.NoError(t, err)
	_, portText, err := net.SplitHostPort(listener.Addr().String())
	require.NoError(t, err)
	modelPort, err := strconv.Atoi(portText)
	require.NoError(t, err)
	server := &http.Server{Handler: models}
	go server.Serve(tls.NewListener(listener, cluster.peer.ServerTLSConfig()))
	t.Cleanup(func() { assert.NoError(t, server.Close()) })

	infoPort := 0
	nodeInfo := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := cluster.peer.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(noderec.NodeInfo{HostUUID: cluster.peerUUID, ClusterUUID: &cluster.peerUUID, Peer: &noderec.PeerMetadata{Name: "unicast-only-peer", Addresses: []string{address}, Services: map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: infoPort, noderec.ServiceEngineManager: modelPort, noderec.ServiceVLLM: 19800}}}))
	}))
	require.NoError(t, nodeInfo.Listener.Close())
	nodeInfo.Listener, err = net.Listen("tcp", net.JoinHostPort(address, "0"))
	require.NoError(t, err)
	_, portText, err = net.SplitHostPort(nodeInfo.Listener.Addr().String())
	require.NoError(t, err)
	infoPort, err = strconv.Atoi(portText)
	require.NoError(t, err)
	nodeInfo.TLS = cluster.peer.ServerTLSConfig()
	nodeInfo.StartTLS()
	t.Cleanup(nodeInfo.Close)
	cmd := exec.Command(scannerBin, "--cluster-dir", cluster.nodeDir)
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		assert.NoError(t, stdin.Close())
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			assert.NoError(t, cmd.Process.Kill())
			<-done
		}
	})
	messages := startMsgReader(t, stdout)
	waitForMethod(t, messages, "ready", 10*time.Second)
	require.NoError(t, json.NewEncoder(stdin).Encode(map[string]any{"jsonrpc": "2.0", "method": noderec.MethodSetClusterPeers, "params": noderec.ClusterPeersParams{Peers: []noderec.ClusterPeer{{HostUUID: cluster.peerUUID, Name: "unicast-only-peer", Address: address, NodeInfoPort: infoPort}}}}))
	// No mDNS responder is created for the peer: only the confirmed address hint
	// can introduce it, and its model inventory must still cross pinned mTLS.
	deadline := time.After(15 * time.Second)
	found := false
	for !found {
		select {
		case message, open := <-messages:
			require.True(t, open, "scanner exited before publishing the peer")
			if message.Method != noderec.NotifyNodeDiscovered && message.Method != noderec.NotifyNodeUpdated {
				continue
			}
			var event noderec.NodeEvent
			require.NoError(t, json.Unmarshal(message.Params, &event))
			node := event.Node
			if node.HostUUID != cluster.peerUUID || node.IdentityVerification != noderec.IdentityPinned {
				continue
			}
			require.True(t, node.Trusted)
			assert.Equal(t, 19800, node.Services[noderec.ServiceVLLM].Port)
			assert.Equal(t, []string{"org/cached"}, node.ModelsByEngine["vllm"])
			found = true
		case <-deadline:
			t.Fatal("confirmed peer never reached the directory without multicast")
		}
	}
	// Withdrawing a hint cannot revoke independently authenticated inventory.
	require.NoError(t, json.NewEncoder(stdin).Encode(map[string]any{"jsonrpc": "2.0", "method": noderec.MethodSetClusterPeers, "params": noderec.ClusterPeersParams{Peers: []noderec.ClusterPeer{}}}))
}
