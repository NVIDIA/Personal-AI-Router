// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cableprobe

import (
	"encoding/binary"
	"strings"
	"testing"
)

// Synthetic bytes only. These tests do not open sockets or send packets.
func fixtureFrame() []byte {
	mac := []byte{2, 0, 0, 0, 0, 2}
	data := append([]byte{1, 128, 194, 0, 0, 14}, mac...)
	data = append(data, 0x88, 0xcc)
	for _, tlv := range []struct {
		kind  uint16
		value []byte
	}{
		{1, append([]byte{4}, mac...)}, {2, append([]byte{3}, mac...)}, {3, []byte{0, 20}},
		{6, []byte(markerPrefix + strings.Repeat("a", 32) + " 1")}, {0, nil},
	} {
		header := make([]byte, 2)
		binary.BigEndian.PutUint16(header, tlv.kind<<9|uint16(len(tlv.value)))
		data = append(append(data, header...), tlv.value...)
	}
	return data
}

func TestFixedProfileFrame(t *testing.T) {
	frame, err := DecodeFrame(fixtureFrame())
	if err != nil || frame.SourceMAC != "02:00:00:00:00:02" || frame.Sequence != 1 || frame.RunMarker != strings.Repeat("a", 32) {
		t.Fatalf("unexpected decoded observation: %+v %v", frame, err)
	}
	if _, err := DecodeFrame(append(fixtureFrame(), make([]byte, 12)...)); err != nil {
		t.Fatal(err)
	}
	for size := 0; size < len(fixtureFrame()); size++ {
		if _, err := DecodeFrame(fixtureFrame()[:size]); err == nil {
			t.Fatalf("accepted truncation at %d", size)
		}
	}
	for _, name := range []string{"destination", "ether-type", "source-multicast", "mandatory-order", "tlv-overflow", "identity-mismatch", "ttl-zero", "ttl-legacy", "ttl-oversize", "marker", "trailing", "oversize"} {
		t.Run(name, func(t *testing.T) {
			data := fixtureFrame()
			switch name {
			case "destination":
				data[0] = 0
			case "ether-type":
				data[12] = 0x81
			case "source-multicast":
				data[6] = 3
			case "mandatory-order":
				data[14] = 4
			case "tlv-overflow":
				data[15] = 255
			case "identity-mismatch":
				data[22]++
			case "ttl-zero":
				data[35] = 0
			case "ttl-legacy":
				data[35] = 10
			case "ttl-oversize":
				data[35] = 21
			case "marker":
				data[39] = 'X'
			case "trailing":
				data = append(data, 1)
			case "oversize":
				data = append(data, make([]byte, 1519)...)
			}
			if _, err := DecodeFrame(data); err == nil {
				t.Fatal("invalid profile accepted")
			}
		})
	}
}

func targetsFixture() []Target {
	return []Target{
		{NodeID: "local", Principal: "local-principal", Ports: []Port{{SwitchID: "local-switch", PortName: "p0", Interfaces: []Interface{{Name: "local0", Index: 1, MAC: "02:00:00:00:00:01"}}}}},
		{NodeID: "peer", Principal: "peer-principal", Ports: []Port{{SwitchID: "peer-switch", PortName: "p1", Interfaces: []Interface{{Name: "peer0", Index: 2, MAC: "02:00:00:00:00:02"}}}}},
	}
}

func TestReviewedIngressResolution(t *testing.T) {
	frame, _ := DecodeFrame(fixtureFrame())
	got, err := ResolveIngress(frame, strings.Repeat("a", 32), "local", 1, 2, targetsFixture())
	if err != nil || got.Local.NodeID != "local" || got.Peer.NodeID != "peer" || got.Peer.PortName != "p1" {
		t.Fatalf("match=%+v %v", got, err)
	}
	for _, name := range []string{"old-run", "outgoing", "wrong-ingress", "self", "unknown-source", "duplicate-peer", "duplicate-principal", "empty-principal", "five-aliases", "ambiguous-peer-mac", "ambiguous-local-port", "unreviewed-local", "zero-sequence", "zero-source"} {
		t.Run(name, func(t *testing.T) {
			f, marker, node, index, kind, targets := frame, strings.Repeat("a", 32), "local", 1, uint8(2), targetsFixture()
			switch name {
			case "old-run":
				marker = strings.Repeat("b", 32)
			case "outgoing":
				kind = 4
			case "wrong-ingress":
				index = 9
			case "self":
				f.SourceMAC = "02:00:00:00:00:01"
			case "unknown-source":
				f.SourceMAC = "02:00:00:00:00:09"
			case "duplicate-peer":
				targets = append(targets, targets[1])
			case "duplicate-principal":
				targets[1].Principal = targets[0].Principal
			case "empty-principal":
				targets[1].Principal = ""
			case "five-aliases":
				for i := 2; i <= 5; i++ {
					alias := targets[0].Ports[0].Interfaces[0]
					alias.Index = i
					targets[0].Ports[0].Interfaces = append(targets[0].Ports[0].Interfaces, alias)
				}
			case "ambiguous-peer-mac":
				other := targets[1]
				other.NodeID, other.Principal = "other-peer", "other-principal"
				targets = append(targets, other)
			case "ambiguous-local-port":
				other := targets[0].Ports[0]
				other.PortName = "p1"
				targets[0].Ports = append(targets[0].Ports, other)
			case "unreviewed-local":
				node = "other"
			case "zero-sequence":
				f.Sequence = 0
			case "zero-source":
				f.SourceMAC = "00:00:00:00:00:00"
			}
			if _, err := ResolveIngress(f, marker, node, index, kind, targets); err == nil {
				t.Fatal("unbound observation accepted")
			}
		})
	}
}

func TestFourAliasesWithinPhysicalPort(t *testing.T) {
	frame, _ := DecodeFrame(fixtureFrame())
	targets := targetsFixture()
	for i := 2; i <= 4; i++ {
		alias := targets[0].Ports[0].Interfaces[0]
		alias.Index = i
		targets[0].Ports[0].Interfaces = append(targets[0].Ports[0].Interfaces, alias)
	}
	if _, err := ResolveIngress(frame, strings.Repeat("a", 32), "local", 1, 2, targets); err != nil {
		t.Fatal(err)
	}
}
