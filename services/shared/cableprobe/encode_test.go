// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cableprobe

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
	"time"
)

// Synthetic byte checks only; no sender, socket, or native privilege is used.
func TestEncodeFrameRoundTrip(t *testing.T) {
	marker := strings.Repeat("a", 32)
	for sequence := uint32(1); sequence <= 100; sequence++ {
		data, err := EncodeFrame("02:AA:00:00:00:02", marker, sequence)
		if err != nil {
			t.Fatal(err)
		}
		got, err := DecodeFrame(data)
		if err != nil || got.SourceMAC != "02:aa:00:00:00:02" || got.RunMarker != marker || got.Sequence != sequence {
			t.Fatalf("sequence %d did not roundtrip: %+v %v", sequence, got, err)
		}
		if len(data) < 60 || len(data) > 1518 {
			t.Fatalf("frame size outside Ethernet bounds: %d", len(data))
		}
	}
}

func TestEncodeFrameFixedProfile(t *testing.T) {
	marker := strings.Repeat("0", 32)
	data, err := EncodeFrame("02:00:00:00:00:02", marker, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data[:14], []byte{1, 128, 194, 0, 0, 14, 2, 0, 0, 0, 0, 2, 0x88, 0xcc}) {
		t.Fatal("Ethernet destination, source, or EtherType changed")
	}
	position := 14
	for _, expected := range []struct {
		kind  uint16
		value []byte
	}{
		{1, []byte{4, 2, 0, 0, 0, 0, 2}},
		{2, []byte{3, 2, 0, 0, 0, 0, 2}},
		{3, []byte{0, 20}},
		{6, []byte("PAIR-LINK-CHECK/1 " + marker + " 100")},
		{0, nil},
	} {
		if position+2 > len(data) {
			t.Fatal("missing mandatory TLV header")
		}
		header := binary.BigEndian.Uint16(data[position : position+2])
		position += 2
		length := int(header & 511)
		if header>>9 != expected.kind || length != len(expected.value) || position+length > len(data) || !bytes.Equal(data[position:position+length], expected.value) {
			t.Fatalf("TLV %d does not match the fixed profile", expected.kind)
		}
		if expected.kind == 3 {
			ttl := binary.BigEndian.Uint16(data[position : position+length])
			if ttl != 20 || time.Duration(ttl)*time.Second != Window {
				t.Fatal("TTL does not match the finite window")
			}
		}
		position += length
	}
	if position != len(data) {
		t.Fatal("encoder added trailing payload or unnecessary padding")
	}
}

func TestEncodeFrameRejectsInvalidInput(t *testing.T) {
	for _, name := range []string{"empty-mac", "malformed-mac", "zero-mac", "multicast-mac", "broadcast-mac", "eight-byte-mac", "empty-marker", "short-marker", "long-marker", "uppercase-marker", "nonhex-marker", "control-marker", "zero-sequence", "oversize-sequence", "max-sequence"} {
		t.Run(name, func(t *testing.T) {
			mac, marker, sequence := "02:00:00:00:00:02", strings.Repeat("a", 32), uint32(1)
			switch name {
			case "empty-mac":
				mac = ""
			case "malformed-mac":
				mac = "02:00:invalid"
			case "zero-mac":
				mac = "00:00:00:00:00:00"
			case "multicast-mac":
				mac = "01:00:00:00:00:01"
			case "broadcast-mac":
				mac = "ff:ff:ff:ff:ff:ff"
			case "eight-byte-mac":
				mac = "02:00:00:00:00:00:00:02"
			case "empty-marker":
				marker = ""
			case "short-marker":
				marker = marker[:31]
			case "long-marker":
				marker += "a"
			case "uppercase-marker":
				marker = strings.Repeat("A", 32)
			case "nonhex-marker":
				marker = strings.Repeat("g", 32)
			case "control-marker":
				marker = marker[:31] + "\n"
			case "zero-sequence":
				sequence = 0
			case "oversize-sequence":
				sequence = 101
			case "max-sequence":
				sequence = ^uint32(0)
			}
			data, err := EncodeFrame(mac, marker, sequence)
			if err == nil || data != nil {
				t.Fatal("invalid input produced frame bytes")
			}
		})
	}
}
