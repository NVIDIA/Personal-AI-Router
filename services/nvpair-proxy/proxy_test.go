// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"nvpair-shared/noderec"
)

// An unverified discovery can claim the local UUID, loopback address, and trust.
// None of those claims may bypass verification to create an inference target.
func TestSubscribedToNode_RejectsUnverifiedLoopbackClaims(t *testing.T) {
	node := noderec.DirectoryNode{
		HostUUID:             "self",
		IP:                   "127.0.0.1",
		Trusted:              true,
		IdentityVerification: noderec.IdentityUnverified,
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceOllama: {Port: 11434},
		},
	}
	profile, ok := profileFor("ollama")
	require.True(t, ok)
	_, accepted := subscribedToNode(profile, node)
	assert.False(t, accepted)
}

// Otherwise routable peer records must fail independently when verification
// is absent or unverified, or when a proved peer no longer has current trust.
func TestSubscribedToNode_RejectsUnverifiedOrUntrustedPeers(t *testing.T) {
	profile, ok := profileFor("ollama")
	require.True(t, ok)
	test := func(name string, verification noderec.IdentityVerification, trusted bool) {
		t.Run(name, func(t *testing.T) {
			node := noderec.DirectoryNode{
				HostUUID:             "peer",
				IP:                   "192.0.2.20",
				IdentityVerification: verification,
				Trusted:              trusted,
				Services: map[noderec.ServiceKey]noderec.ServiceStatus{
					noderec.ServiceOllama: {Port: 11434},
				},
			}

			_, accepted := subscribedToNode(profile, node)

			assert.False(t, accepted)
		})
	}
	test("unverified identity with claimed trust", noderec.IdentityUnverified, true)
	test("missing verification with claimed trust", "", true)
	test("pinned identity without current trust", noderec.IdentityPinned, false)
}

// The owned local record is eligible before joining a cluster, so it does not
// need a remote peer pin or the corresponding trusted flag to remain routable.
func TestSubscribedToNode_AcceptsLocalRecordsWithoutPeerTrust(t *testing.T) {
	const hostUUID = "self"
	const enginePort = 11434
	profile, ok := profileFor("ollama")
	require.True(t, ok)
	node := noderec.DirectoryNode{
		HostUUID:             hostUUID,
		IP:                   "127.0.0.1",
		IdentityVerification: noderec.IdentityLocal,
		Trusted:              false,
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceOllama: {Port: enginePort},
		},
	}

	projected, accepted := subscribedToNode(profile, node)

	require.True(t, accepted)
	assert.Equal(t, hostUUID, projected.ID)
	assert.Equal(t, enginePort, projected.Port)
}

// Documentation addresses avoid host-dependent local-interface rewriting.
func TestNodeCandidates_RanksAndDeduplicatesAddresses(t *testing.T) {
	test := func(name string, node Node, want []string) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, nodeCandidates(node))
		})
	}
	test("sorted deterministically", Node{Addresses: []string{"192.0.2.20", "192.0.2.10"}, Port: 11434}, []string{"192.0.2.10:11434", "192.0.2.20:11434"})
	test("loopback first", Node{Addresses: []string{"192.0.2.10", "127.0.0.1"}, Port: 11434}, []string{"127.0.0.1:11434", "192.0.2.10:11434"})
	test("deduplicated", Node{Addresses: []string{"192.0.2.10", "192.0.2.10"}, Port: 11434}, []string{"192.0.2.10:11434"})
	test("fallback to host", Node{Host: "gpu-host.lan", Port: 11434}, []string{"gpu-host.lan:11434"})
	test("ipv6 bracketed", Node{Addresses: []string{"2001:db8::1"}, Port: 11434}, []string{"[2001:db8::1]:11434"})
}

func TestUUIDFromTXT_ExtractsNodeIdentity(t *testing.T) {
	assert.Equal(t, "abc-123", uuidFromTXT([]string{"models=a;b", "uuid=abc-123"}))
	assert.Equal(t, "", uuidFromTXT([]string{"models=a;b"}))
}

// Documentation addresses avoid host-dependent local-interface rewriting while
// checking the host, port, and IPv6 brackets used to construct each URL.
func TestNodeURL_FormatsIPv4IPv6AndServicePort(t *testing.T) {
	test := func(name string, node Node, wantHost, wantURL string) {
		t.Run(name, func(t *testing.T) {
			u := nodeURL(node)
			require.NotNil(t, u)
			assert.Equal(t, wantHost, u.Host)
			assert.Equal(t, wantURL, u.String())
		})
	}
	test("ipv4", Node{Addresses: []string{"192.0.2.10"}, Port: 11434}, "192.0.2.10:11434", "http://192.0.2.10:11434")
	test("ipv6", Node{Addresses: []string{"2001:db8::1"}, Port: 11434}, "[2001:db8::1]:11434", "http://[2001:db8::1]:11434")
	test("hostname", Node{Addresses: []string{"gpu-host.lan"}, Port: 11434}, "gpu-host.lan:11434", "http://gpu-host.lan:11434")
	test("fallback to host", Node{Host: "gpu-host.lan", Port: 11434}, "gpu-host.lan:11434", "http://gpu-host.lan:11434")
}

func ollamaFacadeForTest(t *testing.T) *facade {
	t.Helper()
	profile, ok := profileFor("ollama")
	require.True(t, ok)
	return testProxy(profile, NewDiscovery(), profile.StandalonePort).soleFacade()
}

// handleHTTP buffers the whole request body so every failover attempt can
// replay it. Without a cap, any loopback client could grow the proxy until it
// ran out of memory.
func TestBufferBodyAndModel_RejectsBodyOverCap(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(strings.Repeat("x", maxInferenceBodyBytes+1)))

	_, _, err := bufferBodyAndModel(req, false)

	assert.ErrorIs(t, err, errBodyTooLarge)
}

func TestBufferBodyAndModel_AcceptsBodyExactlyAtCap(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(strings.Repeat("x", maxInferenceBodyBytes)))

	body, _, err := bufferBodyAndModel(req, false)

	require.NoError(t, err)
	assert.Len(t, body, maxInferenceBodyBytes)
}

func TestBufferBodyAndModel_ParsesModelFromSmallBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"llama3"}`))

	_, model, err := bufferBodyAndModel(req, false)

	require.NoError(t, err)
	assert.Equal(t, "llama3", model)
}

// An over-cap POST is answered before any routing or engine work happens.
func TestHandleHTTP_RejectsBodyOverCapWithJSON413(t *testing.T) {
	f := ollamaFacadeForTest(t)
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte(strings.Repeat("x", maxInferenceBodyBytes+1024)))
		_ = pw.Close()
	}()
	req := httptest.NewRequest(http.MethodPost, "/api/chat", pr)
	rec := httptest.NewRecorder()

	f.handleHTTP(rec, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var payload map[string]string
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	assert.Equal(t, "request body exceeds 32 MiB limit", payload["error"])
}

// The fixture has no engine to route to, so a small body takes the ordinary
// no-candidate rejection instead of the 413 path.
func TestHandleHTTP_DoesNotReject413ForBodyUnderCap(t *testing.T) {
	f := ollamaFacadeForTest(t)
	req := httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"llama3"}`))
	rec := httptest.NewRecorder()

	f.handleHTTP(rec, req)

	assert.NotEqual(t, http.StatusRequestEntityTooLarge, rec.Code)
}

// The local engine must stay on loopback. Enforcing it where the backend is
// stored means the mTLS ingress can never become a forwarder to a LAN address.
func TestSetLocalBackend_AcceptsLoopbackHosts(t *testing.T) {
	test := func(name, host string) {
		t.Run(name, func(t *testing.T) {
			f := ollamaFacadeForTest(t)

			err := f.setLocalBackend(localBackend{Engine: "ollama", Host: host, Port: 11436, Healthy: true})

			assert.NoError(t, err)
		})
	}
	test("accepts an empty host", "")
	test("accepts IPv4 loopback", "127.0.0.1")
	test("accepts another IPv4 loopback address", "127.0.0.2")
	test("accepts IPv6 loopback", "::1")
}

func TestSetLocalBackend_RejectsNonLoopbackHosts(t *testing.T) {
	test := func(name, host string) {
		t.Run(name, func(t *testing.T) {
			f := ollamaFacadeForTest(t)

			err := f.setLocalBackend(localBackend{Engine: "ollama", Host: host, Port: 11436, Healthy: true})

			assert.Error(t, err)
		})
	}
	test("rejects a private 192.168 address", "192.168.1.5")
	test("rejects a private 10.x address", "10.0.0.2")
	test("rejects a hostname", "example.com")
	test("rejects an IPv4-mapped private address", "::ffff:192.168.1.5")
}

// If a peer's certificate pin disappears between candidate selection and dial
// time, the dial must fail instead of downgrading to an unauthenticated
// connection.
func TestPeerHTTPTransport_FailsClosedWithoutPin(t *testing.T) {
	profile, ok := profileFor("ollama")
	require.True(t, ok)
	p := testProxy(profile, NewDiscovery(), profile.StandalonePort) // no mesh: unclustered

	tr := p.peerHTTPTransport("no-such-peer")
	require.NotNil(t, tr)
	assert.Nil(t, tr.TLSClientConfig)

	conn, err := tr.DialContext(t.Context(), "tcp", "192.0.2.10:443")
	if conn != nil {
		assert.NoError(t, conn.Close())
	}
	assert.ErrorIs(t, err, errPeerUnpinned)
	assert.Nil(t, conn)
}
