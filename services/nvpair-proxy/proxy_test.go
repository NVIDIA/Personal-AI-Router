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
