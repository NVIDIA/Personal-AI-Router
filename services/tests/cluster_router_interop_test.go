// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

// cluster_router_interop_test.go drives the cluster router across a process
// boundary: a real nvpair-proxy binary, started with --cluster-router, serves
// one loopback endpoint in front of facades for two engines, and the "hosts"
// of that cluster are mock engines registered as manual nodes — the same
// node/add-manual path an operator uses to point PAIR at an engine discovery
// cannot see. Manual entries also stand in for the second machine: this Mac
// holds the whole cluster, so the test proves name resolution, per-provider
// model rewriting, dialect scoping, cross-engine failover, and the workload
// identity end to end without a second host on the network.
// TestClusterRouter_EndpointTurnsOnAndOffWhileTheStackRuns drives the second
// shape: the same binaries with the endpoint dark at start, lifted over the
// control plane while everything runs.
// TestClusterRouter_RuntimeEnableServesAliasesWrittenWhileTheEndpointWasDown
// adds the alias turn to that second shape: an alias document written while
// the endpoint is still dark, then served after one runtime enable.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/engines"
	"nvpair-shared/jsonrpc"
)

// routerMockEngine is a mock engine endpoint advertising fixed native model
// ids. It records the model field of every inference body it receives so the
// caller-spelling versus provider-spelling assertion is readable, and it can
// refuse one named model with a chosen status so failover has a provider that
// declines.
type routerMockEngine struct {
	name      string
	nativeIDs []string
	server    *httptest.Server
	hits      atomic.Int32

	mu         sync.Mutex
	lastModel  string
	refuseFor  string
	refuseStat int
}

func newRouterMockEngine(t *testing.T, name string, nativeIDs ...string) *routerMockEngine {
	t.Helper()
	mock := &routerMockEngine{name: name, nativeIDs: nativeIDs}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			data := make([]map[string]string, 0, len(mock.nativeIDs))
			for _, id := range mock.nativeIDs {
				data = append(data, map[string]string{"id": id})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		case r.Method == http.MethodPost:
			body, err := io.ReadAll(r.Body)
			assert.NoErrorf(t, err, "%s: read inference body", mock.name)
			var received struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(body, &received)

			mock.hits.Add(1)
			mock.mu.Lock()
			mock.lastModel = received.Model
			refuse, refuseFor := mock.refuseStat, mock.refuseFor
			mock.mu.Unlock()

			if refuse != 0 && received.Model == refuseFor {
				// A refusal still carries a body: "the provider said 404"
				// only reaches the router as an answer if the provider
				// actually answered.
				w.WriteHeader(refuse)
				_, _ = io.WriteString(w, `{"error":"model not found"}`)
				return
			}
			// The answer echoes the model that actually ran, so the caller's
			// view proves the router forwarded — and replied with — the
			// provider's own spelling.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"done": true, "choices": []any{}, "model": received.Model,
			})
		default:
			http.Error(w, "unexpected path for a mock engine", http.StatusNotFound)
		}
	}))
	t.Cleanup(mock.server.Close)
	return mock
}

func (m *routerMockEngine) port(t *testing.T) int {
	t.Helper()
	_, port, err := net.SplitHostPort(m.server.Listener.Addr().String())
	require.NoErrorf(t, err, "split mock listener address (%s)", m.server.Listener.Addr())
	code, err := strconv.Atoi(port)
	require.NoErrorf(t, err, "mock listener port (%s)", port)
	return code
}

func (m *routerMockEngine) refuseModel(model string, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refuseFor = model
	m.refuseStat = status
}

func (m *routerMockEngine) lastServedModel() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastModel
}

// startRouterProxy launches the real proxy binary with the cluster endpoint
// enabled on the given loopback port and enables a facade for each named
// engine on its own free port. The persisted-port store is isolated into a
// temp config dir the way startProxyProc does, so the test never reads or
// writes the developer's real config.
func startRouterProxy(t *testing.T, routerPort int, engineNames ...string) *proxyProc {
	t.Helper()
	cfg := t.TempDir()
	cmd := exec.Command(proxyBin,
		"--cluster-router",
		"--cluster-router-port", strconv.Itoa(routerPort),
	)
	cmd.Env = append(os.Environ(),
		"HOME="+cfg, "XDG_CONFIG_HOME="+cfg, "APPDATA="+cfg, "LOCALAPPDATA="+cfg,
	)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err, "proxy stdin pipe")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err, "proxy stdout pipe")
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start(), "start proxy")

	p := &proxyProc{t: t, cmd: cmd, stdin: stdin, msgs: startMsgReader(t, stdout), nextID: 1}
	t.Cleanup(p.stop)

	facadePorts := freePorts(t, len(engineNames))
	for i, name := range engineNames {
		resp := p.call("facade/enable", map[string]any{
			"engine":              name,
			"port":                facadePorts[i],
			"ignorePersistedPort": true,
		})
		var enabled struct {
			Port int `json:"port"`
		}
		require.NoErrorf(t, json.Unmarshal(resp.Result, &enabled), "facade/enable result for %s", name)
		require.Equalf(t, facadePorts[i], enabled.Port, "facade/enable bound another port for %s", name)
	}
	return p
}

// addManualEngine registers a mock endpoint as a manual node under one engine,
// advertising the given model ids — addressed to that engine's facade the way
// the broker relays it.
func addManualEngine(t *testing.T, p *proxyProc, engine, id string, port int, models []string) {
	t.Helper()
	p.call(engines.AddressMethod(engine, "node/add-manual"), map[string]any{
		"id":        id,
		"host":      "127.0.0.1",
		"port":      port,
		"addresses": []string{"127.0.0.1"},
		"models":    models,
	})
}

func routerPost(t *testing.T, client *http.Client, endpoint, body string) (int, []byte) {
	t.Helper()
	resp, err := client.Post(endpoint, "application/json", bytes.NewBufferString(body))
	require.NoErrorf(t, err, "post %s", endpoint)
	defer resp.Body.Close()
	content, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read response")
	return resp.StatusCode, content
}

func TestClusterRouter_RoutesEveryEngineFromOneEndpointAcrossProcesses(t *testing.T) {
	routerPort := freePorts(t, 1)[0]

	const (
		ollamaID = "qwen3:latest"
		vllmID   = "Qwen/Qwen3-8B"
		// Both engines hold these weights under this exact spelling, so the
		// provider set is genuinely two hosts and failover has somewhere to
		// go after the first one declines.
		sharedID = "shared-owner"
	)
	ollamaHost := newRouterMockEngine(t, "ollama-host", ollamaID, sharedID)
	vllmHost := newRouterMockEngine(t, "vllm-host", vllmID, sharedID)

	p := startRouterProxy(t, routerPort, "ollama", "vllm")
	addManualEngine(t, p, "ollama", "mac-ollama", ollamaHost.port(t), []string{ollamaID, sharedID})
	addManualEngine(t, p, "vllm", "mac-vllm", vllmHost.port(t), []string{vllmID, sharedID})
	// Rank the Ollama host first so the failover subtest's direction — first
	// provider refuses, second serves — is fixed rather than incidental.
	p.call("node/set-priority", map[string]any{
		"generation": 1,
		"nodes":      []string{"mac-ollama", "mac-vllm"},
	})

	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	base := fmt.Sprintf("http://127.0.0.1:%d", routerPort)

	t.Run("the model list is the cluster's, merged across engines", func(t *testing.T) {
		resp, err := client.Get(base + "/v1/models")
		require.NoError(t, err, "get cluster model list")
		defer resp.Body.Close()
		var list struct {
			Object string `json:"object"`
			Data   []struct {
				ID      string `json:"id"`
				OwnedBy string `json:"owned_by"`
			} `json:"data"`
		}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&list), "decode cluster model list")
		assert.Equal(t, http.StatusOK, resp.StatusCode)
		assert.Equal(t, "list", list.Object)

		byID := map[string]string{}
		for _, row := range list.Data {
			byID[row.ID] = row.OwnedBy
		}
		// "qwen3" (the untagged spelling) is not a second row: the listing
		// yields the native identifier, not every spelling that resolves.
		assert.Equal(t, map[string]string{
			ollamaID: "cluster",
			vllmID:   "cluster",
			sharedID: "cluster",
		}, byID)
	})

	t.Run("a caller's untagged name runs on the Ollama host with its own tag", func(t *testing.T) {
		beforeOllama, beforeVLLM := ollamaHost.hits.Load(), vllmHost.hits.Load()

		status, body := routerPost(t, client, base+"/v1/chat/completions", `{"model":"qwen3","messages":[]}`)

		require.Equalf(t, http.StatusOK, status, "response (%s)", body)
		assert.Contains(t, string(body), ollamaID, "the provider's own spelling did not reach the caller")
		assert.Equal(t, ollamaID, ollamaHost.lastServedModel(), "the provider was sent the caller's spelling")
		assert.Equal(t, beforeOllama+1, ollamaHost.hits.Load(), "the Ollama host was not dialed exactly once")
		assert.Equal(t, beforeVLLM, vllmHost.hits.Load(), "a name the Ollama host owns was offered to vLLM")
	})

	t.Run("an exact Hugging Face id runs on the vLLM host untouched", func(t *testing.T) {
		beforeOllama, beforeVLLM := ollamaHost.hits.Load(), vllmHost.hits.Load()

		status, body := routerPost(t, client, base+"/v1/chat/completions", `{"model":"Qwen/Qwen3-8B","messages":[]}`)

		require.Equalf(t, http.StatusOK, status, "response (%s)", body)
		assert.Equal(t, vllmID, vllmHost.lastServedModel(), "an exact identifier was rewritten")
		assert.Equal(t, beforeVLLM+1, vllmHost.hits.Load(), "the vLLM host was not dialed exactly once")
		assert.Equal(t, beforeOllama, ollamaHost.hits.Load(), "an exact match gained the other engine")
	})

	t.Run("a credentialed request is still served", func(t *testing.T) {
		// The router forwards the caller's Authorization untouched; the mock
		// cannot show the header, but a router that stripped or refused
		// credentials would fail this request before reaching the provider.
		req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
			bytes.NewBufferString(`{"model":"qwen3","messages":[]}`))
		require.NoError(t, err, "build request")
		req.Header.Set("Authorization", "Bearer caller-token")
		resp, err := client.Do(req)
		require.NoError(t, err, "post with credential")
		defer resp.Body.Close()
		_, err = io.Copy(io.Discard, resp.Body)
		require.NoError(t, err, "drain response")
		assert.Equal(t, http.StatusOK, resp.StatusCode, "a credentialed request was not served")
	})

	t.Run("an engine-native dialect never reaches the other engine", func(t *testing.T) {
		beforeOllama, beforeVLLM := ollamaHost.hits.Load(), vllmHost.hits.Load()

		// /api/chat is Ollama's surface; the model exists, but only on vLLM.
		status, body := routerPost(t, client, base+"/api/chat", `{"model":"Qwen/Qwen3-8B","messages":[]}`)

		assert.Equalf(t, http.StatusBadGateway, status, "response (%s)", body)
		assert.Contains(t, string(body), "no-provider")
		assert.Equal(t, beforeOllama, ollamaHost.hits.Load(), "the request was answered by an engine that lacks the model")
		assert.Equal(t, beforeVLLM, vllmHost.hits.Load(), "a vLLM host was sent an Ollama-only path")
	})

	t.Run("a model nothing advertises is refused with no host dialed", func(t *testing.T) {
		beforeOllama, beforeVLLM := ollamaHost.hits.Load(), vllmHost.hits.Load()

		status, body := routerPost(t, client, base+"/v1/chat/completions", `{"model":"nothing-here","messages":[]}`)

		assert.Equalf(t, http.StatusBadGateway, status, "response (%s)", body)
		assert.Contains(t, string(body), "no-provider")
		assert.Equal(t, beforeOllama, ollamaHost.hits.Load(), "a host that does not serve the model was dialed")
		assert.Equal(t, beforeVLLM, vllmHost.hits.Load(), "a host that does not serve the model was dialed")
	})

	t.Run("the engine identity probe is refused rather than routed", func(t *testing.T) {
		beforeOllama, beforeVLLM := ollamaHost.hits.Load(), vllmHost.hits.Load()

		req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
			bytes.NewBufferString(`{"model":"qwen3","messages":[]}`))
		require.NoError(t, err, "build probe")
		req.Header.Set("X-NVPAIR-Engine-Identity-Probe", "1")
		resp, err := client.Do(req)
		require.NoError(t, err, "send probe")
		defer resp.Body.Close()
		content, err := io.ReadAll(resp.Body)
		require.NoError(t, err, "read probe response")

		assert.Equalf(t, http.StatusConflict, resp.StatusCode, "probe response (%s)", content)
		assert.Equal(t, beforeOllama, ollamaHost.hits.Load(), "a probe was answered by running somebody's job")
		assert.Equal(t, beforeVLLM, vllmHost.hits.Load(), "a probe was answered by running somebody's job")
	})

	t.Run("a refusing provider fails over to the other engine", func(t *testing.T) {
		ollamaHost.refuseModel(sharedID, http.StatusNotFound)
		t.Cleanup(func() { ollamaHost.refuseModel("", 0) })
		beforeOllama, beforeVLLM := ollamaHost.hits.Load(), vllmHost.hits.Load()

		status, body := routerPost(t, client, base+"/v1/chat/completions", `{"model":"shared-owner","messages":[]}`)

		require.Equalf(t, http.StatusOK, status, "response (%s)", body)
		assert.Equal(t, beforeOllama+1, ollamaHost.hits.Load(), "the ranked-first provider was not asked exactly once")
		assert.Equal(t, beforeVLLM+1, vllmHost.hits.Load(), "the second provider did not take over")
	})

	t.Run("the cluster job is reported on the workload stream", func(t *testing.T) {
		// The identity every router job card carries: engine "cluster" so
		// router jobs never collide with per-engine job ids, this process's
		// run id, and a terminal state for a request an earlier subtest
		// served. Pump through proxyProc so notifications already read off
		// stdout during an RPC response wait are still seen.
		p.pump(func(m jsonrpc.Message) bool {
			return m.Method == "workload:submitted" && bytes.Contains(m.Params, []byte(`"engine":"cluster"`))
		}, 10*time.Second)
		completed := p.pump(func(m jsonrpc.Message) bool {
			return m.Method == "workload:completed" && bytes.Contains(m.Params, []byte(`"engine":"cluster"`))
		}, 10*time.Second)

		var done struct {
			WorkloadInfo struct {
				Engine string `json:"engine"`
				Model  string `json:"model"`
				RunID  string `json:"runId"`
				State  string `json:"state"`
			} `json:"workloadInfo"`
		}
		require.NoErrorf(t, json.Unmarshal(completed.Params, &done), "decode cluster job (%s)", completed.Params)
		assert.Equal(t, "cluster", done.WorkloadInfo.Engine)
		assert.Equal(t, "completed", done.WorkloadInfo.State)
		assert.NotEmpty(t, done.WorkloadInfo.RunID, "the job card named no process run")
		// The caller's name, not one provider's spelling: the cluster job is
		// the request the client made, wherever it ended up running.
		assert.Contains(t, []string{"qwen3", "Qwen/Qwen3-8B", "shared-owner"}, done.WorkloadInfo.Model)
	})
}

// TestClusterRouter_ServesBehindABrokerStartedProxy is the product path: the
// broker itself starts the proxy with --cluster-router, so nothing in the
// test wires the router up by hand. The proxy's facades come from the
// broker's ordinary bring-up, the "Ollama host" is a manual node added the
// same way the desktop adds one, and the only thing the client touches is
// the router's own port.
func TestClusterRouter_ServesBehindABrokerStartedProxy(t *testing.T) {
	routerPort := freePorts(t, 1)[0]
	ollamaHost := newRouterMockEngine(t, "ollama-host", "broker-checked-model:latest")

	stdin, msgs, stderr, cleanup := startBrokerWith(t,
		"--proxy-path", proxyBin,
		"--proxy-engines", "ollama",
		"--cluster-router",
		"--cluster-router-port", strconv.Itoa(routerPort),
	)
	t.Cleanup(cleanup)
	go func() {
		for range stderr {
		}
	}()

	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	callBrokerRPC(t, stdin, msgs, 4100, "ollama-proxy:node/add-manual", map[string]any{
		"id":        "broker-ollama-host",
		"host":      "127.0.0.1",
		"port":      ollamaHost.port(t),
		"addresses": []string{"127.0.0.1"},
		"models":    []string{"broker-checked-model:latest"},
	})

	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	base := fmt.Sprintf("http://127.0.0.1:%d", routerPort)

	// The router binds when the proxy starts, which is somewhere inside the
	// broker's bring-up: poll until the endpoint answers.
	var listOK bool
	var lastBody []byte
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		resp, err := client.Get(base + "/v1/models")
		if err == nil {
			lastBody, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				listOK = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	require.Truef(t, listOK, "the broker-started proxy never served the cluster endpoint (%s)", lastBody)

	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	require.NoErrorf(t, json.Unmarshal(lastBody, &listing), "decode listing (%s)", lastBody)
	var ids []string
	for _, row := range listing.Data {
		ids = append(ids, row.ID)
	}
	assert.Contains(t, ids, "broker-checked-model:latest",
		"the manual node's model was missing from the broker-served cluster endpoint")

	t.Run("inference through the broker-served endpoint runs on the host", func(t *testing.T) {
		status, body := routerPost(t, client, base+"/v1/chat/completions",
			`{"model":"broker-checked-model","messages":[]}`)

		require.Equalf(t, http.StatusOK, status, "response (%s)", body)
		assert.Equal(t, "broker-checked-model:latest", ollamaHost.lastServedModel(),
			"the provider was sent the caller's spelling")
	})
}

// TestClusterRouter_EndpointTurnsOnAndOffWhileTheStackRuns is the D22 proof:
// a stack that started with the endpoint dark — the broker spawns the proxy
// with --proxy-engines but no --cluster-router — lifts its routing endpoint,
// drops it, lifts it again, and refuses a move onto a stranger's port, all
// through desktop-shaped writes on the broker's stdio and with no restart
// anywhere. The moves stay one test on purpose, the way the proxy's own
// lifecycle test does: they are one contract, and a vacuous "refused" after
// a failed enable would hide the turn-off that a test is supposed to prove.
//
// Every assertion reads the endpoint after the write landed, and none waits:
// a stdio reply is ordered after the services applied their half — the bind
// or close happens inline on the proxy's read goroutine before it answers,
// and a successful enable answers only once the broker couriered the alias
// document. The no-serve checks live at the socket and the HTTP client
// turns keep-alives off for one shared reason: a closed listener keeps
// serving connections it accepted earlier (the no-abort invariant), so only
// a fresh connection — what the operator's curl makes — tells "the port is
// down" apart from "the port still serves."
func TestClusterRouter_EndpointTurnsOnAndOffWhileTheStackRuns(t *testing.T) {
	const modelID = "runtime-toggle-model:latest"

	routerPort := freePorts(t, 1)[0]
	ollamaHost := newRouterMockEngine(t, "runtime-toggle-host", modelID)

	stdin, msgs, stderr, cleanup := startBrokerWith(t,
		"--proxy-path", proxyBin,
		"--proxy-engines", "ollama",
	)
	t.Cleanup(cleanup)
	go func() {
		for range stderr {
		}
	}()

	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	waitProxyReady(t, stdin, msgs, 15*time.Second)
	callBrokerRPC(t, stdin, msgs, 4400, "ollama-proxy:node/add-manual", map[string]any{
		"id":        "runtime-toggle-host",
		"host":      "127.0.0.1",
		"port":      ollamaHost.port(t),
		"addresses": []string{"127.0.0.1"},
		"models":    []string{modelID},
	})

	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
	t.Cleanup(client.CloseIdleConnections)
	base := fmt.Sprintf("http://127.0.0.1:%d", routerPort)

	// Serving means both halves of an endpoint: the listing answers, and it
	// carries the cluster's only model. An endpoint that answers names is
	// not yet an endpoint that routes.
	requiresServing := func(why string) {
		t.Helper()
		resp, err := client.Get(base + "/v1/models")
		require.NoErrorf(t, err, "cluster endpoint did not answer (%s)", why)
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err, "read model list")
		require.Equalf(t, http.StatusOK, resp.StatusCode, "model list (%s)", body)
		require.Truef(t, routerListingHasID(body, modelID),
			"the endpoint answers without the model it was given to route (%s)", body)
	}

	// A port being down is proven on the socket: no fresh connection may
	// open, which is the desktop's curl-level experience of "down."
	requiresRefused := func(port int, why string) {
		t.Helper()
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
		if err == nil {
			_ = conn.Close()
		}
		require.Errorf(t, err, "port %d still accepts new connections (%s)", port, why)
	}

	// Dark by default: the stack runs, --cluster-router was not passed,
	// and no write has named a port yet.
	requiresRefused(routerPort, "before the first write")

	// The first write binds. callBrokerRPC pins "answered without error";
	// the endpoint checks pin that the answer followed a live listener.
	callBrokerRPC(t, stdin, msgs, 4401, "router/set-enabled", map[string]any{"enabled": true, "port": routerPort})
	requiresServing("after the enable")

	before := ollamaHost.hits.Load()
	status, body := routerPost(t, client, base+"/v1/chat/completions",
		`{"model":"runtime-toggle-model","messages":[]}`)
	require.Equalf(t, http.StatusOK, status, "routed response (%s)", body)
	assert.Equal(t, modelID, ollamaHost.lastServedModel(), "the provider was sent the caller's spelling")
	assert.Equal(t, before+1, ollamaHost.hits.Load(), "the host was not dialed exactly once")

	// A turn-off closes the listener without draining, and it does that
	// before the answer: the port must refuse the moment the reply lands.
	callBrokerRPC(t, stdin, msgs, 4402, "router/set-enabled", map[string]any{"enabled": false})
	requiresRefused(routerPort, "after the disable")

	// The same port again, because a stop that turned terminal would
	// otherwise hide inside a passing test: the rebuilt endpoint must still
	// route, not merely hold a live socket.
	callBrokerRPC(t, stdin, msgs, 4403, "router/set-enabled", map[string]any{"enabled": true, "port": routerPort})
	requiresServing("after the re-enable")

	before = ollamaHost.hits.Load()
	status, body = routerPost(t, client, base+"/v1/chat/completions",
		`{"model":"runtime-toggle-model","messages":[]}`)
	require.Equalf(t, http.StatusOK, status, "routed response after the re-enable (%s)", body)
	assert.Equal(t, before+1, ollamaHost.hits.Load(), "the rebuilt endpoint no longer routes")

	// A move onto a stranger's port, held open for the rest of the test so
	// the collision is the everyday one: another application owns the
	// number. The refusal has to reach the client as an error with its code
	// intact across both process boundaries, so this write goes raw —
	// callBrokerRPC asserts "no error" and would swallow exactly the
	// answer under test.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "hold a second loopback port")
	t.Cleanup(func() { _ = occupied.Close() })
	takenPort := occupied.Addr().(*net.TCPAddr).Port

	writeRawFrame(t, stdin,
		fmt.Sprintf(`{"jsonrpc":"2.0","id":4404,"method":"router/set-enabled","params":{"enabled":true,"port":%d}}`, takenPort))
	refused := waitForResponseID(t, msgs, 4404, 5*time.Second)
	require.NotNil(t, refused.Error, "a taken port was answered as a success")
	assert.Equal(t, -32011, refused.Error.Code,
		"a bind failure must reach the client with the code that means another port could fix it")
	assert.Contains(t, refused.Error.Message, strconv.Itoa(takenPort),
		"the refusal must name the port that could not be bound")

	// And the refusal changed nothing: the endpoint that was serving keeps
	// serving. Losing a bind race costs one wrong number, not the path
	// that was fine.
	requiresServing("after the refused move")
}

// TestClusterRouter_RoutesToAPairedPeerOverClusterMtls is the phase-2 proof:
// two real cluster-managers pair (A and B, each holding the other's pin on
// disk), B's proxy serves a loopback engine as its local backend, and A's
// proxy — started with --cluster-router — receives a pinned peer record for
// B the way the broker relay delivers it. A request naming B's model on A's
// one cluster endpoint must then travel A's router -> B's dedicated engine
// facade port over cluster mTLS -> B's engine, and back.
//
// The pin-deletion subtest is the on-wire form of the no-two-hop invariant:
// the route observes A's pin only because it is dialed over the pinned
// channel to B's facade ingress, where it terminates. A router hop onward
// would be plaintext loopback and would feel no pin at all — so a route that
// dies with the pin is a route that stopped at the facade.
func TestClusterRouter_RoutesToAPairedPeerOverClusterMtls(t *testing.T) {
	baseA, baseB := t.TempDir(), t.TempDir()
	clusterA := filepath.Join(baseA, "cluster")
	clusterB := filepath.Join(baseB, "cluster")

	cmPorts := freePorts(t, 2)
	cmA := startCM(t, baseA, cmPorts[0])
	defer cmA.stop()
	cmB := startCM(t, baseB, cmPorts[1])
	defer cmB.stop()
	waitForPort(t, "127.0.0.1", cmPorts[1], 10*time.Second)

	bInfo := decodeResult[cmNodeID](t, cmB.call("cluster:get-node-id", nil))

	cmA.call("cluster:create", map[string]any{"clusterFriendlyName": "Router Lab"})
	pairNodes(t, cmA, cmB, cmPorts[1], "node-b")

	// B's engine: reached only through B's facade, so a hit proves the
	// request completed inference behind B's ingress rather than being
	// answered in transit.
	bEngineHost, bEnginePort, bGenerates := startFakeOllama(t)
	proxyB := startProxyProc(t, clusterB, freePort(t))
	defer proxyB.stop()
	proxyB.setLocalBackend("ollama", bEngineHost, bEnginePort, true)

	// A: the origin. Its ollama overlay receives B as a trusted pinned peer
	// advertising far-model at B's facade port — the same addressed relay the
	// broker sends.
	routerPort := freePort(t)
	proxyA := startProxyProc(t, clusterA, freePort(t),
		"--cluster-router", "--cluster-router-port", strconv.Itoa(routerPort))
	defer proxyA.stop()
	proxyA.pushOllamaPeer("node-b", "127.0.0.1", bInfo.NodeUUID, proxyB.port, []string{"far-model:latest"})
	proxyA.waitForRoutableNode(t, bInfo.NodeUUID)

	client := &http.Client{Timeout: 15 * time.Second}
	defer client.CloseIdleConnections()
	base := fmt.Sprintf("http://127.0.0.1:%d", routerPort)

	t.Run("the peer's model is listed on the origin's endpoint", func(t *testing.T) {
		resp, err := client.Get(base + "/v1/models")
		require.NoError(t, err, "get cluster model list")
		defer resp.Body.Close()
		content, err := io.ReadAll(resp.Body)
		require.NoError(t, err, "read model list")
		assert.Contains(t, string(content), "far-model:latest",
			"a pinned peer's model must be listed by the cluster endpoint (%s)", content)
	})

	t.Run("a name only the peer owns runs on the peer's engine over mTLS", func(t *testing.T) {
		before := atomic.LoadInt32(bGenerates)

		// The untagged caller's spelling: name resolution must normalize to
		// the peer's tagged identifier across the node boundary too.
		status, body := routerPost(t, client, base+"/api/generate",
			`{"model":"far-model","prompt":"hi","stream":false}`)

		require.Equalf(t, http.StatusOK, status, "response (%s)", body)
		assert.Contains(t, string(body), "hello from the backend",
			"the answer did not come from B's engine")
		assert.Equal(t, before+1, atomic.LoadInt32(bGenerates),
			"B's engine was not hit exactly once")
	})

	t.Run("the cross-node job names the peer as scheduledOn", func(t *testing.T) {
		completed := proxyA.pump(func(m jsonrpc.Message) bool {
			return m.Method == "workload:completed" && bytes.Contains(m.Params, []byte(`"engine":"cluster"`))
		}, 10*time.Second)
		var done struct {
			WorkloadInfo struct {
				ScheduledOn string `json:"scheduledOn"`
				Model       string `json:"model"`
			} `json:"workloadInfo"`
		}
		require.NoErrorf(t, json.Unmarshal(completed.Params, &done), "decode cluster job (%s)", completed.Params)
		// The job's placement is the peer's node id, and its model is the
		// caller's spelling: cross-node jobs stay attributable to both ends.
		assert.Equal(t, bInfo.NodeUUID, done.WorkloadInfo.ScheduledOn)
		assert.Equal(t, "far-model", done.WorkloadInfo.Model)
	})

	t.Run("losing the pin unlists the model and dials nothing", func(t *testing.T) {
		pin := filepath.Join(clusterA, "trusted", bInfo.NodeUUID+".json")
		require.FileExists(t, pin, "expected B's pin in A's trust store")
		require.NoError(t, os.Remove(pin), "remove A's pin for B")

		// Listed implies routable, so the listing must drop the model the
		// origin can no longer dial — within the catalog's staleness bound.
		var listed string
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			resp, err := client.Get(base + "/v1/models")
			require.NoError(t, err, "get model list after pin removal")
			content, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			require.NoError(t, err, "read model list")
			listed = string(content)
			if !strings.Contains(listed, "far-model") {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		assert.NotContains(t, listed, "far-model",
			"an unpinnable peer stayed listed on the cluster endpoint (%s)", listed)

		before := atomic.LoadInt32(bGenerates)
		status, body := routerPost(t, client, base+"/api/generate",
			`{"model":"far-model","prompt":"hi","stream":false}`)
		assert.Equal(t, http.StatusBadGateway, status,
			"a request the pin no longer authorizes was answered (%s)", body)
		assert.Contains(t, string(body), "no-provider")
		assert.Equal(t, before, atomic.LoadInt32(bGenerates),
			"the peer's engine was reached without a pin")
	})
}

// routerListingHasID reports whether one /v1/models body lists a model id.
func routerListingHasID(body []byte, id string) bool {
	var list struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return false
	}
	for _, row := range list.Data {
		if row.ID == id {
			return true
		}
	}
	return false
}

// waitForRouterModelListed polls the cluster endpoint until it lists (want)
// or stops listing modelID. Alias configuration reaches the router across
// two process boundaries, so the assertion is eventually-consistent by
// nature; the timeout is what makes a broken courier fail loudly.
func waitForRouterModelListed(t *testing.T, client *http.Client, base, modelID string, want bool, why string) {
	t.Helper()
	var lastBody []byte
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		resp, err := client.Get(base + "/v1/models")
		if err == nil {
			lastBody, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && routerListingHasID(lastBody, modelID) == want {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	wantWord := "stop"
	if want {
		wantWord = "start"
	}
	t.Fatalf("cluster endpoint did not %s list %q (%s); last body: %s", wantWord, modelID, why, lastBody)
}

// waitTCPPortFree waits for a loopback port to stop accepting connections.
// Used between broker generations: the router port is held by the proxy, so
// once it is free the whole previous process is gone and the next broker can
// re-bind both the router and its persisted facade port.
func waitTCPPortFree(t *testing.T, port int, why string) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("port %d never released (%s)", port, why)
}

// TestClusterRouter_ConfiguredAliasesTravelSettingsBrokerProxy is the phase-3a
// proof: aliases are stored by the real nvpair-node-settings binary, reached
// through the broker's ordinary settings relay, and delivered to the real
// broker-started proxy's cluster router by the broker's courier code. Both
// deliveries the design requires are exercised: the change push (an edit takes
// effect while everything runs) and the bring-up sync (a broker restarted
// against the same config dir honors the stored aliases with no re-save).
//
// What crosses those boundaries is two names per alias and nothing else. The
// manual node above is what makes "quick-chat" routable, because this process
// can see a host advertising the model that name stands for; on a machine that
// saw no such host the very same document would be inert. No host list travels
// to make it either way.
func TestClusterRouter_ConfiguredAliasesTravelSettingsBrokerProxy(t *testing.T) {
	const (
		aliasNodeID = "alias-target-host"
		nativeID    = "alias-target:latest"
		aliasName   = "quick-chat"
	)
	routerPort := freePorts(t, 1)[0]
	configDir := t.TempDir()
	ollamaHost := newRouterMockEngine(t, "alias-host", nativeID)

	brokerArgs := []string{
		"--proxy-path", proxyBin,
		"--proxy-engines", "ollama",
		"--settings-path", nodeSettingsBin,
		"--cluster-router",
		"--cluster-router-port", strconv.Itoa(routerPort),
	}

	// One broker generation: bring-up, app:ready, and the manual node that
	// advertises the model the alias will stand for. The settings list itself
	// is what the generations differ over.
	startGeneration := func() (stdin io.WriteCloser, msgs <-chan jsonrpc.Message, cleanup func()) {
		stdin, msgs, stderr, cleanup := startBrokerWithConfigDir(t, configDir, brokerArgs...)
		go func() {
			for range stderr {
			}
		}()
		waitForMethod(t, msgs, "app:ready", 15*time.Second)
		callBrokerRPC(t, stdin, msgs, 4300, "ollama-proxy:node/add-manual", map[string]any{
			"id":        aliasNodeID,
			"host":      "127.0.0.1",
			"port":      ollamaHost.port(t),
			"addresses": []string{"127.0.0.1"},
			"models":    []string{nativeID},
		})
		return stdin, msgs, cleanup
	}

	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	base := fmt.Sprintf("http://127.0.0.1:%d", routerPort)

	stdin1, msgs1, cleanup1 := startGeneration()
	t.Cleanup(cleanup1)

	waitForRouterModelListed(t, client, base, nativeID, true, "the manual node's model should be routable")
	waitForRouterModelListed(t, client, base, aliasName, false, "no alias is configured yet")

	t.Run("a settings change reaches the live router without a restart", func(t *testing.T) {
		callBrokerRPC(t, stdin1, msgs1, 4301, "settings/set-cluster-model-aliases", map[string]any{
			"aliases": []any{map[string]any{
				"name":  aliasName,
				"model": nativeID,
			}},
		})

		waitForRouterModelListed(t, client, base, aliasName, true,
			"a configured alias never reached the router through the change push")

		status, body := routerPost(t, client, base+"/v1/chat/completions",
			fmt.Sprintf(`{"model":%q,"messages":[]}`, aliasName))
		require.Equalf(t, http.StatusOK, status, "alias inference failed (%s)", body)
		assert.Equal(t, nativeID, ollamaHost.lastServedModel(),
			"the provider was sent the alias name instead of its own identifier")
	})

	t.Run("a restarted broker delivers the stored aliases at bring-up", func(t *testing.T) {
		// No settings call in this generation: the alias exists only on disk,
		// so the router listing it at all proves the courier read the settings
		// worker and pushed the list during proxy bring-up.
		cleanup1()
		waitTCPPortFree(t, routerPort, "the previous proxy must exit before the next broker binds the router port")

		stdin2, msgs2, cleanup2 := startGeneration()
		t.Cleanup(cleanup2)

		waitForRouterModelListed(t, client, base, aliasName, true,
			"a stored alias was not synced to the router at bring-up")

		status, body := routerPost(t, client, base+"/v1/chat/completions",
			fmt.Sprintf(`{"model":%q,"messages":[]}`, aliasName))
		require.Equalf(t, http.StatusOK, status, "alias inference after restart failed (%s)", body)
		assert.Equal(t, nativeID, ollamaHost.lastServedModel(),
			"after restart the provider was sent the alias name")

		t.Run("clearing the aliases retires them from the router", func(t *testing.T) {
			before := ollamaHost.hits.Load()

			callBrokerRPC(t, stdin2, msgs2, 4302, "settings/set-cluster-model-aliases",
				map[string]any{"aliases": []any{}})

			waitForRouterModelListed(t, client, base, aliasName, false,
				"a deleted alias stayed listed after the change push")
			status, body := routerPost(t, client, base+"/v1/chat/completions",
				fmt.Sprintf(`{"model":%q,"messages":[]}`, aliasName))
			assert.Equalf(t, http.StatusBadGateway, status, "a deleted alias still routed (%s)", body)
			assert.Contains(t, string(body), "no-provider")
			assert.Equal(t, before, ollamaHost.hits.Load(),
				"the engine was dialed for an alias it no longer answers")
		})
	})
}

// TestClusterRouter_RuntimeEnableServesAliasesWrittenWhileTheEndpointWasDown
// owns the end-to-end claim that the two unit halves only cover up to their
// shared seam: the proxy's ClusterSetAliases stores a document while no
// listener exists (router_test.go) and the broker re-couriers the stored
// document when router/set-enabled lifts an endpoint
// (clusterrouter_test.go), but each unit test only crossed its own side of
// that hand-off. This test crosses the seam with real processes: a broker
// that started dark (no --cluster-router), an alias written through the real
// settings worker while the endpoint is dark, then one runtime enable that
// must serve the alias — listed and routed — with no restart and no
// spawn-time preference anywhere.
func TestClusterRouter_RuntimeEnableServesAliasesWrittenWhileTheEndpointWasDown(t *testing.T) {
	const (
		aliasNodeID = "dark-alias-host"
		nativeID    = "dark-alias-target:latest"
		aliasName   = "dark-quick-chat"
	)
	routerPort := freePorts(t, 1)[0]
	ollamaHost := newRouterMockEngine(t, "dark-alias-host", nativeID)

	// Dark start: --proxy-engines brings the proxy up with its Ollama facade
	// but no --cluster-router, so a listener can only arrive through the
	// runtime write below. --settings-path brings the real
	// nvpair-node-settings, the binary that must hold the alias document
	// written before any endpoint exists.
	stdin, msgs, stderr, cleanup := startBrokerWith(t,
		"--proxy-path", proxyBin,
		"--proxy-engines", "ollama",
		"--settings-path", nodeSettingsBin,
	)
	t.Cleanup(cleanup)
	go func() {
		for range stderr {
		}
	}()

	waitForMethod(t, msgs, "app:ready", 10*time.Second)
	waitProxyReady(t, stdin, msgs, 15*time.Second)
	callBrokerRPC(t, stdin, msgs, 4500, "ollama-proxy:node/add-manual", map[string]any{
		"id":        aliasNodeID,
		"host":      "127.0.0.1",
		"port":      ollamaHost.port(t),
		"addresses": []string{"127.0.0.1"},
		"models":    []string{nativeID},
	})

	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	base := fmt.Sprintf("http://127.0.0.1:%d", routerPort)

	// Down is proven on the socket: only a fresh connection tells "no
	// listener" apart from "a closed listener still serving connections it
	// accepted earlier."
	requiresRefused := func(why string) {
		t.Helper()
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", routerPort), 2*time.Second)
		if err == nil {
			_ = conn.Close()
		}
		require.Errorf(t, err, "port %d still accepts new connections (%s)", routerPort, why)
	}

	requiresRefused("before the alias write")

	// The alias document arrives while nothing can route it. Where it
	// waits is the courier's business, not this test's: the broker either
	// hands the push to the router now — where it sits inert with no
	// endpoint to serve it — or re-reads the stored document from the
	// settings worker when the enable below lifts the endpoint.
	callBrokerRPC(t, stdin, msgs, 4501, "settings/set-cluster-model-aliases", map[string]any{
		"aliases": []any{map[string]any{
			"name":  aliasName,
			"model": nativeID,
		}},
	})

	// Storing the document must not itself lift the endpoint: a write to
	// the settings worker is not a request to start a listener.
	requiresRefused("after the alias write")

	// The runtime lift. The broker answers a successful enable only after
	// it couriered the stored alias document to the proxy, so the endpoint
	// comes up owning the alias; the listing still polls because the
	// listener has only just bound.
	callBrokerRPC(t, stdin, msgs, 4502, "router/set-enabled", map[string]any{"enabled": true, "port": routerPort})

	waitForRouterModelListed(t, client, base, nativeID, true,
		"the lifted endpoint must list the model the manual node advertises")
	waitForRouterModelListed(t, client, base, aliasName, true,
		"an alias stored while the endpoint was dark never reached the lifted endpoint")

	status, body := routerPost(t, client, base+"/v1/chat/completions",
		fmt.Sprintf(`{"model":%q,"messages":[]}`, aliasName))
	require.Equalf(t, http.StatusOK, status, "inference naming the alias failed after the runtime lift (%s)", body)
	assert.Equal(t, nativeID, ollamaHost.lastServedModel(),
		"the provider was sent the alias name instead of its own identifier")
}
