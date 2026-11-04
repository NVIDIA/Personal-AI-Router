// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process coverage for the Custom advertise plane (Task 9).
//
// The two halves of "custom models reach peers" run separately because one
// host cannot stand up both ends of the real path: engine-manager binds a
// fixed em port (14322), so a second broker's scanner cannot fetch a
// loopback-emitted inventory from a fake second node without a cluster mTLS
// pin, and non-loopback plaintext is refused on purpose.
//
//   - Peer side: a fake owner advertises cu=8000 + em=<stub> and the stub
//     serves a ModelsResult carrying the custom bucket. This machine's broker
//     must show that bucket attributed on the discovered node — the scanner
//     copies modelsByEngine wholesale, so this pins the receiving end of the
//     contract (and that a cu-bearing record parses).
//   - Owner side: one broker with the real proxy, prober, and
//     engine-manager binaries. A configured endpoint must travel the whole
//     chain — probe, bridge, reconcile — and surface on the real
//     engine-manager's /v1/models as ByEngine["custom"] plus the flat union;
//     custom/remove must clear both.
package tests

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/grandcat/zeroconf"
)

// --- peer side ---------------------------------------------------------------

// TestDiscovery_ReplicatesCustomModelsFromAPeerRecord: a peer advertises a
// custom facade (cu=8000) plus its em surface, and its /v1/models attributes
// one model to the "custom" engine. Our discovery must carry that attribution
// onto the node untouched — no scanner knowledge of Custom is required, and a
// cu-bearing TXT record must parse rather than poison the node.
func TestDiscovery_ReplicatesCustomModelsFromAPeerRecord(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"models":         []string{"external/model-a"},
			"modelsByEngine": map[string][]string{"custom": {"external/model-a"}},
		}))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err, "parse stub url")
	emPort, err := strconv.Atoi(u.Port())
	require.NoError(t, err, "stub port")

	const instance = "xproc-custom-1"
	txt := []string{"v=1", "uuid=" + instance + "-uuid", "ip=127.0.0.1", "cu=8000", fmt.Sprintf("em=%d", emPort)}
	zsrv, err := zeroconf.Register(instance, nodeRecordService, testDomain, emPort, txt, nil)
	require.NoError(t, err, "register (%v)", instance)
	t.Cleanup(zsrv.Shutdown)
	t.Logf("advertising %s @ %s (cu=8000, em=%d)", instance, nodeRecordService, emPort)

	stdin, msgs, cleanup := startBroker(t)
	t.Cleanup(cleanup)

	deadline := time.After(30 * time.Second)
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()
	id := 300
	sendReq(t, stdin, id, "discovery:get-nodes")
	for {
		select {
		case msg, ok := <-msgs:
			require.True(t, ok, "broker stream closed unexpectedly")
			if msg.Method == "" && msg.ID != nil {
				var res availableNodesResult
				require.NoError(t, json.Unmarshal(msg.Result, &res))
				if n, found := findNode(res.Nodes, instance); found &&
					byEngineEqual(n.ModelsByEngine, map[string][]string{"custom": {"external/model-a"}}) {
					return // the custom bucket replicated as-is
				}
			}
		case <-ticker.C:
			id++
			sendReq(t, stdin, id, "discovery:get-nodes")
		case <-deadline:
			require.FailNow(t, fmt.Sprintf("timed out waiting for %q with a custom-attributed model list", instance))
		}
	}
}

// --- owner side --------------------------------------------------------------

// fetchEngineManagerInventory reads this node's own em surface over loopback
// (the plaintext path is loopback-restricted, which is exactly where a test
// caller lives) and decodes the ModelsResult shape.
func fetchEngineManagerInventory(t *testing.T, client *http.Client) ([]string, map[string][]string) {
	t.Helper()
	response, err := client.Get("http://127.0.0.1:14322/v1/models")
	require.NoError(t, err, "GET /v1/models from the local engine-manager")
	defer func() { require.NoError(t, response.Body.Close(), "close inventory response") }()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err, "read inventory body")
	require.Equal(t, http.StatusOK, response.StatusCode, "em /v1/models status: %s", body)
	var inventory struct {
		Models         []string            `json:"models"`
		ModelsByEngine map[string][]string `json:"modelsByEngine"`
	}
	require.NoError(t, json.Unmarshal(body, &inventory), "decode inventory body: %s", body)
	return inventory.Models, inventory.ModelsByEngine
}

// waitForCustomBucket polls the local engine-manager's /v1/models until the
// "custom" key matches want. want nil means the key must be absent — the
// documented "no custom inventory" spelling, distinct from an empty list.
func waitForCustomBucket(t *testing.T, client *http.Client, want []string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		models, byEngine := fetchEngineManagerInventory(t, client)
		got, present := byEngine["custom"]
		matched := (want == nil && !present) ||
			(want != nil && present && len(got) == len(want) && func() bool {
				for i := range want {
					if got[i] != want[i] {
						return false
					}
				}
				return true
			}())
		if matched {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline:
			require.FailNowf(t, "custom bucket wait",
				"timed out after %s waiting for custom bucket %v, last seen: models=%v byEngine=%v",
				timeout, want, models, byEngine)
		}
	}
}

// TestCustomChain_OwnerSurfacesCustomModelsToEngineManager drives the owner
// half with the real binaries: after custom/add and one successful probe, the
// real engine-manager's /v1/models must attribute the endpoint's model to
// "custom" (both the flat union and the per-engine map); after custom/remove
// the custom key must disappear entirely. That is what makes the models
// visible to a peer's fetch and, through it, to peer UIs.
func TestCustomChain_OwnerSurfacesCustomModelsToEngineManager(t *testing.T) {
	if portBusy(8000) {
		t.Skip("custom facade default port 8000 already in use; skipping")
	}
	if portBusy(14322) {
		t.Skip("engine-manager em port 14322 already in use; skipping")
	}
	const model = "custom-adv/model-x"
	upPort := freePort(t)
	newFakeEndpoint(t, upPort, model, "")

	stdin, msgs, stderrLines, cleanup := startBrokerWith(t,
		"--manual-nodes-path", manualNodesBin,
		"--proxy-path", proxyBin,
		"--engine-manager-path", engineMgrBin,
	)
	t.Cleanup(cleanup)
	go func() {
		for range stderrLines {
		}
	}()

	waitForMethod(t, msgs, "app:ready", 15*time.Second)
	facadePort := waitEngineProxyReady(t, "custom-proxy", stdin, msgs, 20*time.Second)
	require.Equal(t, 8000, facadePort, "custom facade must default to port 8000")
	waitTCP(t, "127.0.0.1:14322", 20*time.Second)

	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)

	// Baseline: nothing configured, so the custom key must be absent.
	waitForCustomBucket(t, client, nil, 20*time.Second)

	callCustomRPC(t, stdin, msgs, nextCustomRPCID(), "custom/add",
		map[string]any{"name": "adv-e2e", "url": "http://127.0.0.1", "port": upPort})

	// The inventory read is the whole wait. An endpoint projects no node
	// entry, so advertising is observed where it is published: a model
	// reaching em means the probe, the bridge and the reconcile all ran for
	// it, which is strictly more than the routing-set entry the bridge
	// creates on the way.
	waitForCustomBucket(t, client, []string{model}, 45*time.Second)
	models, _ := fetchEngineManagerInventory(t, client)
	assert.Contains(t, models, model, "the flat union must carry the custom model for union-only consumers")

	callCustomRPC(t, stdin, msgs, nextCustomRPCID(), "custom/remove",
		map[string]any{"id": customCandidateID(upPort)})

	// Removal retracts the advertised inventory too: the key disappears, it
	// does not linger as an empty list.
	waitForCustomBucket(t, client, nil, 30*time.Second)
	models, _ = fetchEngineManagerInventory(t, client)
	assert.NotContains(t, models, model, "the flat union must drop the removed model")
}
