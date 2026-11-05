// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Task 10: Custom requests that start on a peer node. A custom-keyed request
// resolves on the origin node to the endpoint's owner, dialed over cluster
// mTLS to the owner's Custom facade port (the promoted-proxy-port precedent),
// and terminates on the owner: the owner picks which configured endpoint serves
// the request — by model when the set declares owners, otherwise the single
// unattributed entry — and injects that endpoint's own key on the last hop.
// The caller's own Authorization never reaches an endpoint, in either hop.
//
// Pair-shaped tests follow TestCORSPairedIngress_PreservesPolicyAndIsTerminal:
// two real cluster directories with mutual pins (pins must carry the peer's
// real leaf, because the mTLS transport pins the exact served certificate DER),
// a real mTLS ingress server, and real dialing, so the pin gate is exercised
// end to end rather than mocked.

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
	"nvpair-shared/clustertrusttest"
	"nvpair-shared/engines"
	"nvpair-shared/noderec"
)

// backendEntry is the per-facade backend-set element a Custom node's broker
// pushes down (node/set-local-backend grows from one backend to a set). URL and
// APIKey describe one user-configured endpoint; ModelNames lists the models
// that endpoint declared, and an empty list means the single-entry default
// rule applies.

// customProxy builds a test proxy hosting exactly the Custom facade. The port
// is never bound by these tests — requests drive handleHTTP/handleClusterIngress
// directly — but each proxy needs a distinct value for the self-target checks.
func customProxy(t *testing.T, disc *Discovery, port int) *Proxy {
	t.Helper()
	profile, ok := profileFor(engines.NameCustom)
	require.True(t, ok, "custom profile exists")
	return testProxy(profile, disc, port)
}

// ownerCustomIngress stands up one node's Custom side: a facade with a backend
// set, its real cluster mesh (dir), and the mTLS ingress server in front of
// handleClusterIngress, so callers reach the production handler through a
// production TLS gate.
func ownerCustomIngress(t *testing.T, clusterDir string, backends []backendEntry) (owner *facade, ingressURL string, ingressPort int) {
	t.Helper()
	p := customProxy(t, NewDiscovery(), 8000)
	owner = p.soleFacade()
	require.NoError(t, owner.setLocalBackends(backends), "backend set accepted")
	p.mesh = clustertrust.Open(clusterDir)
	p.mesh.Refresh()

	ingress := httptest.NewUnstartedServer(http.HandlerFunc(owner.handleClusterIngress))
	ingress.TLS = p.mesh.ServerTLSConfig()
	ingress.StartTLS()
	t.Cleanup(ingress.Close)

	u, err := url.Parse(ingress.URL)
	require.NoError(t, err, "parse ingress URL (%v, %v)", ingress.URL, err)
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err, "parse ingress port (%v, %v)", u.Port(), err)
	return owner, ingress.URL, port
}

// customPeerPair wires origin and owner as a paired cluster: mutual pins over
// each side's real leaf (an exact-DER pin against a minted stand-in would fail
// the dial, not the gate), and an owner snapshot advertising the listed
// custom models at its cu port.
func customPeerPair(t *testing.T, advertised []string, backends []backendEntry) (origin *facade, ownerIngressURL string) {
	t.Helper()
	aDir := filepath.Join(t.TempDir(), "clusterA")
	bDir := filepath.Join(t.TempDir(), "clusterB")
	clustertrusttest.Join(t, aDir, "custom-lab", "principal-a")
	clustertrusttest.Join(t, bDir, "custom-lab", "principal-b")
	pinLeaf := func(dstDir, srcDir, peerUUID string) {
		t.Helper()
		pem, err := os.ReadFile(filepath.Join(srcDir, "node.crt"))
		require.NoError(t, err, "read identity from %s", srcDir)
		body, err := json.Marshal(map[string]string{"nodeUuid": peerUUID, "certPem": string(pem)})
		require.NoError(t, err, "encode pin for %s", peerUUID)
		require.NoError(t, os.MkdirAll(filepath.Join(dstDir, "trusted"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dstDir, "trusted", peerUUID+".json"), body, 0o600))
	}
	pinLeaf(aDir, bDir, "principal-b")
	pinLeaf(bDir, aDir, "principal-a")

	_, ownerURL, ownerPort := ownerCustomIngress(t, bDir, backends)

	disc := NewDiscovery()
	p := customProxy(t, disc, 8123)
	origin = p.soleFacade()
	p.mesh = clustertrust.Open(aDir)
	p.mesh.Refresh()

	u, err := url.Parse(ownerURL)
	require.NoError(t, err, "parse owner URL (%v, %v)", ownerURL, err)
	snapshot := noderec.GetNodesResult{Nodes: []noderec.DirectoryNode{{
		HostUUID:       "node-b",
		Name:           "peer-b",
		IP:             u.Hostname(),
		ClusterUUID:    "principal-b",
		Services:       map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceCustom: {Port: ownerPort}},
		ModelsByEngine: map[string][]string{"custom": advertised},
	}}}
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err, "encode snapshot")
	origin.replaceSubscribed(raw)
	return origin, ownerURL
}

// keyedUpstream stands in for a user-configured OpenAI-compatible endpoint: it
// answers only when the request carries exactly wantAuth (empty accepts any),
// records the first Authorization it saw, and counts hits so a test can prove
// which endpoint the request actually reached.
func keyedUpstream(t *testing.T, wantAuth, marker string) (endpointURL string, seenAuth func() string, hits func() int32) {
	t.Helper()
	var mu sync.Mutex
	var auth string
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if auth == "" {
			auth = r.Header.Get("Authorization")
		}
		mu.Unlock()
		if wantAuth != "" && r.Header.Get("Authorization") != wantAuth {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		n.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, err := io.WriteString(w, fmt.Sprintf(`{"marker":%q}`, marker))
		assert.NoError(t, err, "write endpoint response")
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() string { mu.Lock(); defer mu.Unlock(); return auth }, func() int32 { return n.Load() }
}

// postCustomChat drives a chat request through a facade's full local router.
// The caller's own Authorization is set deliberately: every endpoint-side
// assertion of "Bearer <endpoint key>" doubles as a check that this
// credential was dropped and never relayed onward.
func postCustomChat(t *testing.T, f *facade, model string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[]}`, model)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer caller-own-key")
	rec := httptest.NewRecorder()
	f.handleHTTP(rec, req)
	return rec
}

// tlsClientWithIdentity builds an HTTPS client presenting clusterDir's own
// leaf. InsecureSkipVerify carries no risk to the assertion: no pin is
// configured on the client side because the server-side pin gate is what is
// under test.
func tlsClientWithIdentity(t *testing.T, clusterDir string) *http.Client {
	t.Helper()
	cert, err := tls.LoadX509KeyPair(filepath.Join(clusterDir, "node.crt"), filepath.Join(clusterDir, "node.key"))
	require.NoError(t, err, "load identity keypair from %s", clusterDir)
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{Certificates: []tls.Certificate{cert}, InsecureSkipVerify: true},
	}}
}

func TestResolveCandidates_PinnedCuPeerDialsOwnerFacadePortOverMtls(t *testing.T) {
	const peerUUID = "principal-owner"
	clusterDir := filepath.Join(t.TempDir(), "cluster")

	disc := NewDiscovery()
	p := customProxy(t, disc, 8000)
	p.mesh = clustertrust.Open(clusterDir)
	f := p.soleFacade()

	// The relay snapshot names the owner's cu service at its facade port —
	// peers dial the facade, never an engine — plus the per-engine inventory
	// that attributes custom models to it.
	snapshot := noderec.GetNodesResult{Nodes: []noderec.DirectoryNode{{
		HostUUID:       "node-b",
		Name:           "peer-b",
		IP:             "192.0.2.20",
		ClusterUUID:    peerUUID,
		Services:       map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceCustom: {Port: 18000}},
		ModelsByEngine: map[string][]string{"custom": {"small-llm"}},
	}}}
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err, "encode snapshot")
	f.replaceSubscribed(raw)

	// Pre-join: no pin, no route. Advertising cu is not permission to dial.
	require.Empty(t, f.resolveCandidates("small-llm"), "pre-join candidates")

	clustertrusttest.Join(t, clusterDir, "custom-lab", "principal-self", peerUUID)

	cands := f.resolveCandidates("small-llm")
	require.Len(t, cands, 1, "post-join candidates")
	assert.Equal(t, "https", cands[0].url.Scheme, "cu peer dialed over mTLS")
	assert.Equal(t, "192.0.2.20:18000", cands[0].url.Host, "cu service port, not an engine port")
	assert.Equal(t, peerUUID, cands[0].peerUUID, "pin lookup key")
	assert.False(t, cands[0].external, "a paired peer is not the external-endpoint dial class")
	assert.Empty(t, cands[0].key, "a peer never carries an endpoint credential")
}

func TestResolveCandidates_CuPeerModelFilterUsesCustomAttribution(t *testing.T) {
	const peerUUID = "principal-owner"
	clusterDir := filepath.Join(t.TempDir(), "cluster")
	clustertrusttest.Join(t, clusterDir, "custom-lab", "principal-self", peerUUID)

	disc := NewDiscovery()
	p := customProxy(t, disc, 8000)
	p.mesh = clustertrust.Open(clusterDir)
	f := p.soleFacade()

	peerWithAttribution := noderec.DirectoryNode{
		HostUUID:       "node-b",
		Name:           "peer-b",
		IP:             "192.0.2.20",
		ClusterUUID:    peerUUID,
		Services:       map[noderec.ServiceKey]noderec.ServiceStatus{noderec.ServiceCustom: {Port: 18000}},
		ModelsByEngine: map[string][]string{"custom": {"small-llm"}},
	}
	raw, err := json.Marshal(noderec.GetNodesResult{Nodes: []noderec.DirectoryNode{peerWithAttribution}})
	require.NoError(t, err, "encode attributed snapshot")
	f.replaceSubscribed(raw)
	assert.Empty(t, f.resolveCandidates("other-model"), "a model in no bucket belongs to no cu peer")

	// A mixed-version peer that sends no attribution: the flat union is the
	// fallback, the same rule every other facade follows.
	legacy := peerWithAttribution
	legacy.HostUUID = "node-legacy"
	legacy.Name = "peer-legacy"
	legacy.IP = "192.0.2.21"
	legacy.ModelsByEngine = nil
	legacy.Models = []string{"legacy-model"}
	raw2, err := json.Marshal(noderec.GetNodesResult{Nodes: []noderec.DirectoryNode{legacy}})
	require.NoError(t, err, "encode legacy snapshot")
	f.replaceSubscribed(raw2)
	cands := f.resolveCandidates("legacy-model")
	require.Len(t, cands, 1, "legacy flat-union attribution still routes")
	assert.Equal(t, "https://192.0.2.21:18000", cands[0].url.String(), "legacy candidate target")
}

// TestCustomPeerRouting_RoutesModelRequestToEndpointAdvertisingIt proves the
// full paired path: the request enters the origin's router, crosses to the
// owner over mTLS, and terminates on exactly the endpoint whose declared model
// list contains the requested model — with that endpoint's own key injected on
// the last hop and the caller's credential dropped.
func TestCustomPeerRouting_RoutesModelRequestToEndpointAdvertisingIt(t *testing.T) {
	t.Run("medium model reaches endpoint 2 with endpoint 2's key", func(t *testing.T) {
		ep1URL, _, ep1Hits := keyedUpstream(t, "Bearer k1", "ep1")
		ep2URL, ep2Seen, ep2Hits := keyedUpstream(t, "Bearer k2", "ep2")
		origin, _ := customPeerPair(t, []string{"small-llm", "medium-llm"}, []backendEntry{
			{URL: ep1URL, APIKey: "k1", ModelNames: []string{"small-llm"}},
			{URL: ep2URL, APIKey: "k2", ModelNames: []string{"medium-llm"}},
		})

		rec := postCustomChat(t, origin, "medium-llm")
		require.Equal(t, http.StatusOK, rec.Code, "status")
		assert.Contains(t, rec.Body.String(), "ep2", "the request must reach the endpoint advertising the model")
		assert.Equal(t, int32(1), ep2Hits(), "endpoint 2 hit count")
		assert.Equal(t, int32(0), ep1Hits(), "endpoint 1 must not be touched")
		assert.Equal(t, "Bearer k2", ep2Seen(), "endpoint 2 saw its own key, not the caller's")
	})

	t.Run("small model reaches endpoint 1 with endpoint 1's key", func(t *testing.T) {
		ep1URL, ep1Seen, ep1Hits := keyedUpstream(t, "Bearer k1", "ep1")
		ep2URL, _, ep2Hits := keyedUpstream(t, "Bearer k2", "ep2")
		origin, _ := customPeerPair(t, []string{"small-llm", "medium-llm"}, []backendEntry{
			{URL: ep1URL, APIKey: "k1", ModelNames: []string{"small-llm"}},
			{URL: ep2URL, APIKey: "k2", ModelNames: []string{"medium-llm"}},
		})

		rec := postCustomChat(t, origin, "small-llm")
		require.Equal(t, http.StatusOK, rec.Code, "status")
		assert.Contains(t, rec.Body.String(), "ep1", "the request must reach the endpoint advertising the model")
		assert.Equal(t, int32(1), ep1Hits(), "endpoint 1 hit count")
		assert.Equal(t, int32(0), ep2Hits(), "endpoint 2 must not be touched")
		assert.Equal(t, "Bearer k1", ep1Seen(), "endpoint 1 saw its own key, not the caller's")
	})

	t.Run("model no owner advertises is refused at the origin", func(t *testing.T) {
		ep1URL, _, ep1Hits := keyedUpstream(t, "Bearer k1", "ep1")
		ep2URL, _, ep2Hits := keyedUpstream(t, "Bearer k2", "ep2")
		origin, _ := customPeerPair(t, []string{"small-llm", "medium-llm"}, []backendEntry{
			{URL: ep1URL, APIKey: "k1", ModelNames: []string{"small-llm"}},
			{URL: ep2URL, APIKey: "k2", ModelNames: []string{"medium-llm"}},
		})

		rec := postCustomChat(t, origin, "unconfigured-model")
		require.Equal(t, http.StatusBadGateway, rec.Code, "unknown model keeps the actionable 502")
		assert.Contains(t, rec.Body.String(), "no available node advertises the requested model", "rejection reason")
		assert.Equal(t, int32(0), ep1Hits(), "no endpoint may serve an unadvertised model")
		assert.Equal(t, int32(0), ep2Hits(), "no endpoint may serve an unadvertised model")
	})
}

func TestCustomPeerRouting_StaleAdvertisedModelRejectedAtOwner(t *testing.T) {
	// The owner advertises ghost-model (its inventory snapshot predates the
	// endpoint set now installed) but no entry declares it: the owner must
	// refuse rather than guess a backend, and the refusal must not touch the
	// endpoint that does not serve the model.
	epURL, _, hits := keyedUpstream(t, "", "ep")
	origin, _ := customPeerPair(t, []string{"ghost-model"}, []backendEntry{
		{URL: epURL, ModelNames: []string{"medium-llm"}},
	})

	rec := postCustomChat(t, origin, "ghost-model")
	require.Equal(t, http.StatusBadGateway, rec.Code, "advertised-but-unassigned model must not reach an endpoint")
	assert.Equal(t, int32(0), hits(), "endpoint serving a different model must stay untouched")
}

func TestCustomPeerRouting_SingleUnattributedEndpointForwardsEveryModel(t *testing.T) {
	// Selection by model, else the single configured backend: one endpoint
	// with no per-model attribution (a server that answers without declaring
	// its list) forwards what it is asked, like the single loopback engine
	// today — and still injects its own key.
	epURL, seenAuth, hits := keyedUpstream(t, "Bearer k", "ep")
	origin, _ := customPeerPair(t, []string{"anything-goes"}, []backendEntry{{URL: epURL, APIKey: "k"}})

	rec := postCustomChat(t, origin, "anything-goes")
	require.Equal(t, http.StatusOK, rec.Code, "status")
	assert.Contains(t, rec.Body.String(), "ep", "body")
	assert.Equal(t, int32(1), hits(), "the single default endpoint forwards the request")
	assert.Equal(t, "Bearer k", seenAuth(), "endpoint key injected on the last hop")
}

func TestCustomClusterIngress_RefusesUnpinnedCallers(t *testing.T) {
	t.Run("cluster cert outside the pin set is refused after handshake", func(t *testing.T) {
		epURL, _, hits := keyedUpstream(t, "", "ep")
		// The owner directory must be a joined cluster, not just a keypair:
		// an unclustered mesh fails the handshake at the TLS layer, while the
		// behavior under test is the HTTP 403 pin gate above it.
		bDir := filepath.Join(t.TempDir(), "clusterB")
		clustertrusttest.Join(t, bDir, "custom-lab", "principal-b")
		_, ownerURL, _ := ownerCustomIngress(t, bDir, []backendEntry{{URL: epURL}})

		foreignDir := filepath.Join(t.TempDir(), "clusterC")
		clustertrusttest.Join(t, foreignDir, "foreign-lab", "principal-foreign")
		client := tlsClientWithIdentity(t, foreignDir)

		resp, err := client.Post(ownerURL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"small-llm","messages":[]}`))
		require.NoError(t, err, "foreign dial must complete the mTLS handshake")
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err, "read refusal body")
		require.Equal(t, http.StatusForbidden, resp.StatusCode, "an unpinned cluster certificate must not route")
		assert.Contains(t, string(body), "cluster-auth", "refusal names the trust gate")
		assert.Equal(t, int32(0), hits(), "refused request must not reach the endpoint")
	})

	t.Run("origin without a pin resolves no cu target", func(t *testing.T) {
		epURL, _, hits := keyedUpstream(t, "", "ep")
		bDir := filepath.Join(t.TempDir(), "clusterB")
		clustertrusttest.Join(t, bDir, "custom-lab", "principal-b")
		_, ownerURL, ownerPort := ownerCustomIngress(t, bDir,
			[]backendEntry{{URL: epURL}})
		u, err := url.Parse(ownerURL)
		require.NoError(t, err, "parse owner URL (%v, %v)", ownerURL, err)

		foreignDir := filepath.Join(t.TempDir(), "clusterC")
		clustertrusttest.Join(t, foreignDir, "foreign-lab", "principal-foreign")

		disc := NewDiscovery()
		disc.SetSubscribed([]Node{{
			ID:          "node-b",
			Host:        "peer-b",
			Port:        ownerPort,
			Addresses:   []string{u.Hostname()},
			IP:          u.Hostname(),
			ClusterUUID: "principal-b",
			Models:      []string{"small-llm"},
		}})
		p := customProxy(t, disc, 8123)
		p.mesh = clustertrust.Open(foreignDir)
		origin := p.soleFacade()

		rec := postCustomChat(t, origin, "small-llm")
		require.Equal(t, http.StatusBadGateway, rec.Code, "an unpaired origin must find no route")
		assert.Equal(t, int32(0), hits(), "the endpoint must never see a cross-hop attempt")
	})
}

func TestSetLocalBackends_AcceptsBrokerPushedHostsAndRejectsMalformed(t *testing.T) {
	f := customProxy(t, NewDiscovery(), 8000).soleFacade()

	// The backend set only ever arrives over the broker's own stdio push, so
	// a user-configured endpoint host is not refused for being off-box — that
	// is the whole point of a Custom endpoint. Shape errors still fail closed.
	require.NoError(t, f.setLocalBackends([]backendEntry{
		{URL: "http://127.0.0.1:8001"},
		{URL: "https://api.example.com:8443", APIKey: "k"},
	}), "broker-pushed endpoint hosts are storable")

	err := f.setLocalBackends([]backendEntry{{URL: "192.0.2.9:8000"}})
	require.Error(t, err, "a host:port with no scheme is not a backend URL")
	err = f.setLocalBackends([]backendEntry{{URL: "ftp://192.0.2.9:21"}})
	require.Error(t, err, "only http and https endpoints are routable")
}

// The broker pushes both payload classes through the same node/set-local-backend
// method, so the handler's envelope decode is the boundary that must stay
// honest: a present "backends" key is the set class even when empty (that
// spelling is the retraction — a null would decode as the single-backend class
// and silently keep a stale set forwarding), the set class is refused on
// managed facades (defense-in-depth against a control-plane bug repointing a
// managed ingress to an off-box host), and the single-backend class keeps
// working byte-for-byte for the engines that always used it.
func TestSetLocalBackendHandler_DecodesBothPayloadClasses(t *testing.T) {
	push := func(p *Proxy, engine, params string) {
		t.Helper()
		id := json.RawMessage(`7`)
		p.handleMessage(&Message{
			Method: engines.AddressMethod(engine, "node/set-local-backend"),
			Params: json.RawMessage(params),
			ID:     &id,
		})
	}

	t.Run("custom set installs through the message path", func(t *testing.T) {
		f := customProxy(t, NewDiscovery(), 8000).soleFacade()
		push(f.host, engines.NameCustom,
			`{"engine":"custom","backends":[{"url":"https://api.example.com:8443","apiKey":"sk-1","modelNames":["m1"]}]}`)
		set := f.configuredBackends()
		require.Len(t, set, 1, "the set class installs the endpoint set")
		assert.Equal(t, "sk-1", set[0].key, "the endpoint key arrives with the set, on this hop only")
		assert.Equal(t, []string{"m1"}, set[0].models)
	})

	t.Run("empty array retracts the set", func(t *testing.T) {
		f := customProxy(t, NewDiscovery(), 8000).soleFacade()
		require.NoError(t, f.setLocalBackends([]backendEntry{{URL: "http://127.0.0.1:8001"}}))

		push(f.host, engines.NameCustom, `{"engine":"custom","backends":[]}`)
		assert.Empty(t, f.configuredBackends(),
			"a present empty array clears the set; it must not fall through to the single-backend class")
	})

	t.Run("set class refused on a managed engine", func(t *testing.T) {
		ollama, ok := profileFor(engines.NameOllama)
		require.True(t, ok, "ollama profile exists")
		p := newTestProxy(ollama, NewCodec(rwNop{}), NewDiscovery(), 12345)
		f := p.soleFacade()

		push(p, engines.NameOllama, `{"engine":"ollama","backends":[{"url":"http://evil.example:9000"}]}`)
		assert.Empty(t, f.configuredBackends(), "the set class names Unmanaged facades only")
		assert.Equal(t, localBackend{}, f.currentLocalBackend(), "a refused push must not disturb the loopback backend")
	})

	t.Run("single-backend payload still works for managed engines", func(t *testing.T) {
		ollama, ok := profileFor(engines.NameOllama)
		require.True(t, ok, "ollama profile exists")
		p := newTestProxy(ollama, NewCodec(rwNop{}), NewDiscovery(), 12345)
		f := p.soleFacade()

		push(p, engines.NameOllama, `{"engine":"ollama","host":"127.0.0.1","port":1234,"healthy":true}`)
		assert.Equal(t, 1234, f.currentLocalBackend().Port,
			"the additive envelope must not break the class managed engines already speak")
	})
}
