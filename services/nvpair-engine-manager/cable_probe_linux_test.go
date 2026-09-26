// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"net"
	"testing"

	"nvpair-shared/cableprobe"
)

// All interfaces and port facts are synthetic. These tests never call a native
// lock, socket, configure, read, write, or the production sysfs reader.
func TestNativeCableProbeAliasCurrent(t *testing.T) {
	for _, name := range []string{"valid", "lookup-error", "lookup-nil", "wrong-index", "renamed-interface", "changed-mac", "wrong-switch", "wrong-port", "read-error", "reused-index-after-read", "renamed-after-read", "changed-mac-after-read", "lookup-error-after-read", "lookup-nil-after-read"} {
		t.Run(name, func(t *testing.T) {
			alias := cableprobe.Interface{Name: "enp1s0f0", Index: 7, MAC: "02:AA:BB:CC:DD:01"}
			port := cableprobe.PortRef{NodeID: "synthetic-local", SwitchID: "aabbccdd", PortName: "p0"}
			lookups, reads := 0, 0
			lookup := func(index int) (*net.Interface, error) {
				lookups++
				if index != alias.Index {
					t.Errorf("lookup escaped the reviewed index: %d", index)
				}
				actual := &net.Interface{Index: 7, Name: "enp1s0f0", HardwareAddr: net.HardwareAddr{2, 0xaa, 0xbb, 0xcc, 0xdd, 1}}
				switch name {
				case "lookup-error":
					return nil, errors.New("synthetic lookup failure")
				case "lookup-nil":
					return nil, nil
				case "wrong-index":
					actual.Index = 8
				case "renamed-interface":
					actual.Name = "replacement0"
				case "changed-mac":
					actual.HardwareAddr[5] = 2
				case "reused-index-after-read":
					if reads != 0 {
						actual.Index = 8
					}
				case "renamed-after-read":
					if reads != 0 {
						actual.Name = "replacement0"
					}
				case "changed-mac-after-read":
					if reads != 0 {
						actual.HardwareAddr[5] = 2
					}
				case "lookup-error-after-read":
					if reads != 0 {
						return nil, errors.New("synthetic interface removed")
					}
				case "lookup-nil-after-read":
					if reads != 0 {
						return nil, nil
					}
				}
				return actual, nil
			}
			readPort := func(actualName string) (string, string, error) {
				reads++
				if actualName != "enp1s0f0" {
					t.Errorf("physical lookup escaped the current native name: %q", actualName)
				}
				switch name {
				case "wrong-switch":
					return "aabbccde", "p0", nil
				case "wrong-port":
					return "aabbccdd", "p1", nil
				case "read-error":
					return "", "", errors.New("synthetic physical device unavailable")
				}
				return "AABBCCDD", "p0", nil
			}
			err := nativeCableProbeAliasCurrent(alias, port, lookup, readPort)
			if name == "valid" {
				if err != nil || lookups != 2 || reads != 1 {
					t.Fatalf("current alias was not validated before and after port read: lookups=%d reads=%d err=%v", lookups, reads, err)
				}
			} else if err == nil {
				t.Fatal("changed or unavailable native facts were accepted")
			}
			if lookups == 1 && (name == "lookup-error" || name == "lookup-nil" || name == "wrong-index" || name == "renamed-interface" || name == "changed-mac") && reads != 0 {
				t.Fatal("mismatched native identity reached the physical-port reader")
			}
		})
	}
}

func TestNativeCableProbeAliasRejectsInvalidReviewBeforeLookup(t *testing.T) {
	for _, name := range []string{"empty-name", "zero-index", "multicast-mac", "empty-node"} {
		t.Run(name, func(t *testing.T) {
			alias := cableprobe.Interface{Name: "enp1s0f0", Index: 7, MAC: "02:00:00:00:00:01"}
			port := cableprobe.PortRef{NodeID: "synthetic-local", SwitchID: "aabbccdd", PortName: "p0"}
			switch name {
			case "empty-name":
				alias.Name = ""
			case "zero-index":
				alias.Index = 0
			case "multicast-mac":
				alias.MAC = "01:00:00:00:00:01"
			case "empty-node":
				port.NodeID = ""
			}
			calls := 0
			lookup := func(int) (*net.Interface, error) { calls++; return nil, errors.New("unexpected lookup") }
			readPort := func(string) (string, string, error) { calls++; return "", "", errors.New("unexpected read") }
			if err := nativeCableProbeAliasCurrent(alias, port, lookup, readPort); err == nil || calls != 0 {
				t.Fatalf("invalid review reached native-fact readers: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestNativeCableProbeProtocolNetworkOrder(t *testing.T) {
	var wire [2]byte
	binary.NativeEndian.PutUint16(wire[:], nativeCableProbeProtocol())
	if wire != [2]byte{0x88, 0xcc} {
		t.Fatalf("fixed LLDP protocol changed: %x", wire)
	}
}
