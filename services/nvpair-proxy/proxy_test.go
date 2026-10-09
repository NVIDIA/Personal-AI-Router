// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNodeCandidates covers the deterministic, loopback-first ordering and
// de-duplication of a node's advertised addresses. Local-address rewriting is
// not exercised (it depends on the host's interfaces); these addresses are
// chosen to be non-local.
func TestNodeCandidates(t *testing.T) {
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

func TestUUIDFromTXT(t *testing.T) {
	assert.Equal(t, "abc-123", uuidFromTXT([]string{"models=a;b", "uuid=abc-123"}))
	assert.Equal(t, "", uuidFromTXT([]string{"models=a;b"}))
}

// TestNodeURL covers the URL-construction part of nodeURL — specifically
// that IPv6 literals are bracket-wrapped and IPv4/hostnames remain
// byte-identical to the pre-JoinHostPort implementation. The
// local-address shortcut is not exercised here because it depends on
// the host's network interfaces (see init() in proxy.go) and would be
// flaky across environments.
func TestNodeURL(t *testing.T) {
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
