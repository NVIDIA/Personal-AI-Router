// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

// Cross-process proof of the peer-origin Custom routing plane (Task 10).
//
// The custompeer unit tests already drive the routing shape between two
// in-process proxies; this file proves the same path when every piece is the
// real binary and the real crypto:
//
//   - Node B owns two user-configured endpoints (one key-gated, one open).
//     Their backend set — URL, credential, declared models — arrives over B's
//     stdio exactly as the broker pushes it, and terminates at B: no further
//     routing happens on the owner.
//   - Node A's custom facade takes a loopback chat request for a model that
//     only exists on B, resolves B from a pushed discovery snapshot (cu port,
//     trusted pin, custom-attributed models), and dials B's custom facade
//     over cluster mTLS — pins minted by two real cluster-managers paired
//     for real, not fixture files.
//   - B then resolves model → endpoint among its set and injects that
//     endpoint's own credential on the last hop. The caller's Authorization
//     is stripped and never reaches an endpoint, in either hop.
//   - A foreign node with a valid cluster identity but no pin is refused at
//     B's ingress, and a model the advertise still names but no endpoint
//     serves returns the actionable 502 — the endpoint is never dialed.
//
// Like secure_inference_test.go this deliberately bypasses mDNS and
// engine-manager (fixed ports): the discovery snapshot travels over A's
// stdio, which is the same relay shape the broker would send.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/engines"
	"nvpair-shared/noderec"
)

// startCustomProxyProc launches a real nvpair-proxy for a cluster directory
// and brings its Custom facade up on listenPort. Unlike startProxyProc (which
// enables Ollama because the mTLS machinery under test is engine-agnostic),
// the engine here IS the subject: the custom facade must come up with no
// managed engine behind it, served by the same split listener that carries
// the mTLS ingress personality.
func startCustomProxyProc(t *testing.T, clusterDir string, listenPort int) *proxyProc {
	t.Helper()
	cfg := t.TempDir()
	cmd := exec.Command(proxyBin, "--cluster-dir", clusterDir)
	cmd.Env = append(os.Environ(),
		"HOME="+cfg, "XDG_CONFIG_HOME="+cfg, "APPDATA="+cfg, "LOCALAPPDATA="+cfg,
	)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err, "proxy stdin pipe")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err, "proxy stdout pipe")
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start(), "start custom proxy")
	p := &proxyProc{t: t, cmd: cmd, stdin: stdin, msgs: startMsgReader(t, stdout), nextID: 1}

	resp := p.call("facade/enable", map[string]any{
		"engine":              "custom",
		"port":                listenPort,
		"ignorePersistedPort": true,
	})
	var enabled struct {
		Engine string `json:"engine"`
		Port   int    `json:"port"`
	}
	require.NoError(t, json.Unmarshal(resp.Result, &enabled), "facade/enable result")
	require.NotEqual(t, 0, enabled.Port, "facade/enable result")
	p.port = enabled.Port
	return p
}

// setCustomBackends feeds the owner's custom facade its endpoint set over the
// same stdio shape the broker's reconcile pushes: the additive "backends"
// class addressed to the custom facade.
func (p *proxyProc) setCustomBackends(backends []map[string]any) {
	p.call(engines.AddressMethod("custom", "node/set-local-backend"), map[string]any{
		"engine":   "custom",
		"backends": backends,
	})
}

// pushCustomPeer feeds the origin's custom facade a discovery snapshot naming
// one trusted peer that advertises the cu service at cuPort and attributes
// models to the custom engine — the routing contract between the two nodes.
// The peer is pinned and currently trusted: the facade only routes to a node
// whose identity it has proved, so an unverified announcement is dropped.
func (p *proxyProc) pushCustomPeer(name, ip, clusterUUID string, cuPort int, models []string) {
	p.notify(engines.AddressMethod("custom", "discovery:nodes"), map[string]any{
		"nodes": []map[string]any{{
			"hostUuid":             name,
			"name":                 name,
			"ip":                   ip,
			"clusterUuid":          clusterUUID,
			"identityVerification": string(noderec.IdentityPinned),
			"trusted":              true,
			"services":             map[string]any{"cu": map[string]any{"port": cuPort}},
			"modelsByEngine":       map[string]any{"custom": models},
			"lastSeen":             time.Now().Unix(),
		}},
	})
}

// waitForCustomRoutableNode polls the origin's custom:nodes/list until the
// pushed snapshot has been applied, so the first inference attempt meets a
// resolved route rather than racing the notification.
func (p *proxyProc) waitForCustomRoutableNode(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp := p.call(engines.AddressMethod("custom", "nodes/list"), nil)
		var r struct {
			Nodes []json.RawMessage `json:"nodes"`
		}
		require.NoError(t, json.Unmarshal(resp.Result, &r))
		if len(r.Nodes) > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.FailNow(t, "custom facade never registered the pushed peer node")
}

// TestCustomPeerInference_PairedRoutingResolvesEndpointsAtTheOwner pairs two
// real cluster-managers, runs a real custom-facade proxy on each node, and
// drives the whole peer path: A-side model resolution, the pinned mTLS hop,
// and B-side endpoint selection with credential injection.
func TestCustomPeerInference_PairedRoutingResolvesEndpointsAtTheOwner(t *testing.T) {
	baseA, baseB, baseC := t.TempDir(), t.TempDir(), t.TempDir()
	clusterA := filepath.Join(baseA, "cluster")
	clusterB := filepath.Join(baseB, "cluster")
	clusterC := filepath.Join(baseC, "cluster")

	cmBPort := freePort(t)
	cmA := startCM(t, baseA, freePort(t))
	defer cmA.stop()
	cmB := startCM(t, baseB, cmBPort)
	defer cmB.stop()
	cmC := startCM(t, baseC, freePort(t))
	defer cmC.stop()

	bInfo := decodeResult[cmNodeID](t, cmB.call("cluster:get-node-id", nil))

	// The cluster-manager listeners need a moment to bind before pairing.
	time.Sleep(time.Second)

	cmA.call("cluster:create", map[string]any{"clusterFriendlyName": "Secure Lab"})
	pairNodes(t, cmA, cmB, cmBPort, "node-b")
	cmC.call("cluster:create", map[string]any{"clusterFriendlyName": "Foreign Lab"})

	// B owns the endpoints: one that demands its configured key, one open.
	// Both live on high loopback ports so nothing collides with the custom
	// facade's default 6767 or with the facades' own bind.
	keyPort := freePort(t)
	plainPort := freePort(t)
	keyed := newFakeEndpoint(t, keyPort, "custom-e2e/model-a", "Bearer sk-e2e")
	plain := newFakeEndpoint(t, plainPort, "custom-e2e/model-b", "")

	proxyB := startCustomProxyProc(t, clusterB, freePort(t))
	defer proxyB.stop()
	proxyB.setCustomBackends([]map[string]any{
		{
			"url":        fmt.Sprintf("http://127.0.0.1:%d", keyPort),
			"apiKey":     "sk-e2e",
			"modelNames": []string{"custom-e2e/model-a"},
		},
		{
			"url":        fmt.Sprintf("http://127.0.0.1:%d", plainPort),
			"modelNames": []string{"custom-e2e/model-b"},
		},
	})

	proxyA := startCustomProxyProc(t, clusterA, freePort(t))
	defer proxyA.stop()
	// A learns B owns three models — including one B no longer serves, which
	// is the stale-advertise case a real advertise could leave behind.
	proxyA.pushCustomPeer("node-b", "127.0.0.1", bInfo.NodeUUID, proxyB.port,
		[]string{"custom-e2e/model-a", "custom-e2e/model-b", "custom-e2e/ghost-model"})
	proxyA.waitForCustomRoutableNode(t)

	chatURL := fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", proxyA.port)
	chatBody := func(model string) []byte {
		return []byte(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":false}`, model))
	}
	// The wait budget covers the proxy's designed retry cycle, not just a fast
	// hop. A model no endpoint serves answers 502 at the owner, and the origin
	// treats that as a failed dispatch: maxDispatchAttempts (5) with the
	// backoff ladder (1+2+4+8 s, jittered) runs to ~20 s before the last 502
	// surfaces. A 15 s client timeout sat below that bound and only passed
	// when the jittered ladder happened to finish early.
	newChatClient := func() *http.Client { return &http.Client{Timeout: 45 * time.Second} }

	// 1. A paired request crosses the mTLS hop and reaches the endpoint that
	//    declares its model, authenticated by the key configured on the OWNER.
	t.Run("paired A to B reaches the keyed endpoint with only the injected key", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodPost, chatURL, bytes.NewReader(chatBody("custom-e2e/model-a")))
		require.NoError(t, err, "build chat request")
		req.Header.Set("Content-Type", "application/json")
		// A caller-supplied credential must be dropped before the endpoint
		// hop: it belongs to whatever service the caller thinks it is talking
		// to, never to a third-party endpoint.
		req.Header.Set("Authorization", "Bearer caller-secret")
		resp, err := newChatClient().Do(req)
		require.NoError(t, err, "A->B custom inference")
		defer resp.Body.Close()
		body, readErr := io.ReadAll(resp.Body)
		require.NoError(t, readErr)
		require.Equal(t, http.StatusOK, resp.StatusCode, "A->B custom status (%v)", body)
		require.Contains(t, string(body), `"choices":[]`, "response must come from the fake endpoint")
		assert.Equal(t, int32(1), keyed.chatHits.Load(), "the model's endpoint must be dialed exactly once")
		assert.Zero(t, plain.chatHits.Load(), "per-endpoint attribution must not touch the other endpoint")
		assert.Equal(t, []string{"Bearer sk-e2e"}, keyed.authorizations(),
			"only the owner-injected endpoint key may arrive — never the caller's Authorization")
	})

	// 2. A second model routes to the second endpoint: selection is per
	//    model, not a single pinned upstream.
	t.Run("model resolution picks the endpoint that declares the model", func(t *testing.T) {
		code, body := postCustomChat(t, newChatClient(), proxyA.port, "custom-e2e/model-b")
		require.Equal(t, http.StatusOK, code, "model-b status (%v)", body)
		require.Contains(t, body, `"choices":[]`, "response must come from the fake endpoint")
		assert.Equal(t, int32(1), plain.chatHits.Load(), "model-b must land on the endpoint that declares it")
		assert.Equal(t, int32(1), keyed.chatHits.Load(), "the other endpoint must stay untouched")
	})

	// 3. A model the advertise still names but no endpoint serves fails with
	//    the actionable 502 at the owner — nothing is dialed on a guess.
	t.Run("stale advertised model is refused with the actionable 502", func(t *testing.T) {
		code, body := postCustomChat(t, newChatClient(), proxyA.port, "custom-e2e/ghost-model")
		require.Equal(t, http.StatusBadGateway, code, "ghost-model status (%v)", body)
		assert.Contains(t, body, "no-backend-for-model",
			"the caller must learn the advertise is stale, not that its request was malformed")
		assert.Equal(t, int32(1), keyed.chatHits.Load(), "an unattributed model must dial no endpoint")
		assert.Equal(t, int32(1), plain.chatHits.Load(), "an unattributed model must dial no endpoint")
	})

	// 4. A foreign node's certificate completes the TLS handshake (the
	//    ingress asks for any client cert) but is not pinned here, so the
	//    owner refuses before the backend set is consulted at all.
	t.Run("foreign node is refused at the owner ingress", func(t *testing.T) {
		client := mtlsClientWithIdentity(t, clusterC)
		resp, err := client.Post(
			fmt.Sprintf("https://127.0.0.1:%d/v1/chat/completions", proxyB.port),
			"application/json", bytes.NewReader(chatBody("custom-e2e/model-a")))
		require.NoError(t, err, "foreign C dial B custom ingress")
		defer resp.Body.Close()
		body, readErr := io.ReadAll(resp.Body)
		require.NoError(t, readErr)
		require.Equal(t, http.StatusForbidden, resp.StatusCode, "foreign C status (%v)", body)
		assert.Equal(t, int32(1), keyed.chatHits.Load(), "the endpoint must never see a cross-cluster attempt")
		assert.Equal(t, int32(1), plain.chatHits.Load(), "the endpoint must never see a cross-cluster attempt")
	})
}
