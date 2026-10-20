// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
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

// TestNodeCandidates covers the deterministic, loopback-first ordering and
// de-duplication of a node's advertised addresses. Local-address rewriting is
// not exercised (it depends on the host's interfaces); these addresses are
// chosen to be non-local.
func TestNodeCandidates_UsesPublishedAddressOrder(t *testing.T) {
	tests := []struct {
		name string
		node Node
		want []string
	}{
		{
			name: "sorted deterministically",
			node: Node{Addresses: []string{"192.0.2.20", "192.0.2.10"}, Port: 11434},
			want: []string{"192.0.2.10:11434", "192.0.2.20:11434"},
		},
		{
			name: "loopback floated to front",
			node: Node{Addresses: []string{"192.0.2.10", "127.0.0.1"}, Port: 11434},
			want: []string{"127.0.0.1:11434", "192.0.2.10:11434"},
		},
		{
			name: "deduplicated",
			node: Node{Addresses: []string{"192.0.2.10", "192.0.2.10"}, Port: 11434},
			want: []string{"192.0.2.10:11434"},
		},
		{
			name: "fallback to host",
			node: Node{Host: "gpu-host.lan", Port: 11434},
			want: []string{"gpu-host.lan:11434"},
		},
		{
			name: "ipv6 bracketed",
			node: Node{Addresses: []string{"2001:db8::1"}, Port: 11434},
			want: []string{"[2001:db8::1]:11434"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, nodeCandidates(tc.node), "nodeCandidates")
		})
	}
}

func TestUUIDFromTXT_ExtractsNodeIdentity(t *testing.T) {
	assert.Equal(t, "abc-123", uuidFromTXT([]string{"models=a;b", "uuid=abc-123"}))
	assert.Equal(t, "", uuidFromTXT([]string{"models=a;b"}))
}

// TestNodeURL covers the URL-construction part of nodeURL — specifically
// that IPv6 literals are bracket-wrapped and IPv4/hostnames remain
// byte-identical to the pre-JoinHostPort implementation. The
// local-address shortcut is not exercised here because it depends on
// the host's network interfaces (see init() in proxy.go) and would be
// flaky across environments.
func TestNodeURL_FormatsIPv4IPv6AndServicePort(t *testing.T) {
	// Pick addresses that are unlikely to appear on any local interface.
	tests := []struct {
		name     string
		node     Node
		wantHost string
		wantURL  string
	}{
		{
			name:     "ipv4",
			node:     Node{Addresses: []string{"192.0.2.10"}, Port: 11434},
			wantHost: "192.0.2.10:11434",
			wantURL:  "http://192.0.2.10:11434",
		},
		{
			name:     "ipv6",
			node:     Node{Addresses: []string{"2001:db8::1"}, Port: 11434},
			wantHost: "[2001:db8::1]:11434",
			wantURL:  "http://[2001:db8::1]:11434",
		},
		{
			name:     "hostname",
			node:     Node{Addresses: []string{"gpu-host.lan"}, Port: 11434},
			wantHost: "gpu-host.lan:11434",
			wantURL:  "http://gpu-host.lan:11434",
		},
		{
			// Empty Addresses slice — nodeURL should fall back to Host.
			name:     "fallback to Host",
			node:     Node{Host: "gpu-host.lan", Port: 11434},
			wantHost: "gpu-host.lan:11434",
			wantURL:  "http://gpu-host.lan:11434",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u := nodeURL(tc.node)
			require.NotNil(t, u, "nodeURL returned nil")
			assert.Equal(t, tc.wantHost, u.Host, "Host")
			assert.Equal(t, tc.wantURL, u.String())
		})
	}
}
