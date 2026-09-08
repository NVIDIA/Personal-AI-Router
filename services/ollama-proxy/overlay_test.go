// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Routing to a node reachable only over an encrypted overlay such as a Tailscale
// tailnet. Such a node is never discovered — a tailnet carries no multicast — so
// it arrives as a manually added address, and that address is a 100.64/10 CGNAT
// literal, an IPv6 ULA, or a MagicDNS name. netpick demotes all three relative to
// a LAN literal; none of them may be excluded, or the node has nowhere to be
// dialed.

package main

import (
	"reflect"
	"testing"
)

func TestNodeCandidates_OverlayOnlyNodeIsRoutable(t *testing.T) {
	tests := []struct {
		name string
		node Node
		want []string
	}{
		{
			name: "cgnat only",
			node: Node{Addresses: []string{"100.101.102.103"}, Port: 11434},
			want: []string{"100.101.102.103:11434"},
		},
		{
			name: "ipv6 ula only",
			node: Node{Addresses: []string{"fd7a:115c:a1e0::1701:b2c3"}, Port: 11434},
			want: []string{"[fd7a:115c:a1e0::1701:b2c3]:11434"},
		},
		{
			name: "magicdns name only",
			node: Node{Addresses: []string{"gpu-box.tail1234.ts.net"}, Port: 11434},
			want: []string{"gpu-box.tail1234.ts.net:11434"},
		},
		{
			name: "magicdns name carried as the node's canonical ip= TXT",
			node: Node{
				TXT:       []string{"ip=gpu-box.tail1234.ts.net"},
				Addresses: []string{"gpu-box.tail1234.ts.net"},
				Port:      11434,
			},
			want: []string{"gpu-box.tail1234.ts.net:11434"},
		},
		{
			name: "lan literal still preferred when the node has both",
			node: Node{Addresses: []string{"100.101.102.103", "192.0.2.10"}, Port: 11434},
			want: []string{"192.0.2.10:11434", "100.101.102.103:11434"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := nodeCandidates(tc.node); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("nodeCandidates = %v, want %v", got, tc.want)
			}
		})
	}
}

// A manual entry that names this host's own tailnet address must resolve to
// loopback rather than being dialed back around the tunnel to ourselves. The
// local-address set comes from netmon, which enumerates every interface without
// filtering, so an overlay adapter's address is in it exactly like a LAN one —
// this pins that, because a set built from the *publishing* picker instead would
// omit an unproven overlay address and let the entry loop.
func TestIsLocalAddress_CountsThisHostsOverlayAddresses(t *testing.T) {
	const ourTailnetIP = "100.64.7.7"

	localAddrsMu.Lock()
	prev := localAddrs
	localAddrs = map[string]bool{"192.0.2.5": true, ourTailnetIP: true}
	localAddrsMu.Unlock()
	t.Cleanup(func() {
		localAddrsMu.Lock()
		localAddrs = prev
		localAddrsMu.Unlock()
	})

	if !isLocalAddress(ourTailnetIP) {
		t.Fatalf("isLocalAddress(%q) = false, want true", ourTailnetIP)
	}
	got := nodeCandidates(Node{Addresses: []string{ourTailnetIP}, Port: 11434})
	want := []string{"127.0.0.1:11434"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nodeCandidates for our own tailnet address = %v, want %v", got, want)
	}
}
