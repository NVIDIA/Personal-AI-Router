// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cableprobe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
)

// This is a decoder for the fixed diagnostic profile, not a socket, sender,
// privilege mechanism, or authenticator. Unknown LLDP profiles are not evidence.
type Frame struct {
	SourceMAC string
	RunMarker string
	Sequence  uint32
}

const markerPrefix = "PAIR-LINK-CHECK/1 "

func validMarker(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func DecodeFrame(data []byte) (Frame, error) {
	var result Frame
	invalid := errors.New("not a complete fixed-profile LLDP observation")
	if len(data) < 16 || len(data) > 1518 || !bytes.Equal(data[:6], []byte{1, 128, 194, 0, 0, 14}) || binary.BigEndian.Uint16(data[12:14]) != 0x88cc {
		return result, invalid
	}
	source := data[6:12]
	if source[0]&1 != 0 || bytes.Equal(source, make([]byte, 6)) {
		return result, invalid
	}
	result.SourceMAC = net.HardwareAddr(source).String()
	expected := []uint16{1, 2, 3, 6, 0}
	position := 14
	for _, kind := range expected {
		if position+2 > len(data) {
			return Frame{}, invalid
		}
		header := binary.BigEndian.Uint16(data[position : position+2])
		position += 2
		length := int(header & 511)
		if header>>9 != kind || position+length > len(data) {
			return Frame{}, invalid
		}
		value := data[position : position+length]
		position += length
		switch kind {
		case 1, 2:
			subtype := byte(4) // Chassis MAC; port MAC has subtype 3.
			if kind == 2 {
				subtype = 3
			}
			if len(value) != 7 || value[0] != subtype || !bytes.Equal(value[1:], source) {
				return Frame{}, invalid
			}
		case 3:
			if len(value) != 2 || binary.BigEndian.Uint16(value) != uint16(Window/time.Second) {
				return Frame{}, invalid
			}
		case 6:
			text := string(value)
			if !strings.HasPrefix(text, markerPrefix) {
				return Frame{}, invalid
			}
			parts := strings.Split(strings.TrimPrefix(text, markerPrefix), " ")
			if len(parts) != 2 || !validMarker(parts[0]) {
				return Frame{}, invalid
			}
			sequence, err := strconv.ParseUint(parts[1], 10, 32)
			if err != nil || sequence < 1 || sequence > 100 || strconv.FormatUint(sequence, 10) != parts[1] {
				return Frame{}, invalid
			}
			result.RunMarker, result.Sequence = parts[0], uint32(sequence)
		case 0:
			if length != 0 {
				return Frame{}, invalid
			}
		}
	}
	// Ethernet padding is permitted; trailing protocol data is not.
	for _, value := range data[position:] {
		if value != 0 {
			return Frame{}, invalid
		}
	}
	return result, nil
}

type IngressMatch struct {
	Local, Peer PortRef
	Sequence    uint32
}

// ResolveIngress consumes kernel-provided interface/type metadata and an
// immutable, authenticated review. It does not establish freshness or trust;
// those need a caller-owned finite run and actual receive timestamps.
func ResolveIngress(frame Frame, marker, localNode string, index int, packetType uint8, targets []Target) (IngressMatch, error) {
	invalid := errors.New("observation does not resolve uniquely within the reviewed ingress ports")
	if !validMarker(marker) || frame.RunMarker != marker || frame.Sequence == 0 || frame.Sequence > 100 || index <= 0 || packetType != 2 || len(targets) < 2 || len(targets) > 3 {
		return IngressMatch{}, invalid
	}
	source, err := net.ParseMAC(frame.SourceMAC)
	if err != nil || len(source) != 6 || source[0]&1 != 0 || bytes.Equal(source, make([]byte, 6)) {
		return IngressMatch{}, invalid
	}
	var local, peer *PortRef
	seenNodes := map[string]bool{}
	seenPrincipals := map[string]bool{}
	for _, target := range targets {
		if target.NodeID == "" || target.Principal == "" || seenNodes[target.NodeID] || seenPrincipals[target.Principal] || len(target.Ports) < 1 || len(target.Ports) > 2 {
			return IngressMatch{}, invalid
		}
		seenNodes[target.NodeID] = true
		seenPrincipals[target.Principal] = true
		for _, port := range target.Ports {
			if port.SwitchID == "" || port.PortName == "" || len(port.Interfaces) < 1 || len(port.Interfaces) > 4 {
				return IngressMatch{}, invalid
			}
			ref := PortRef{target.NodeID, port.SwitchID, port.PortName}
			for _, iface := range port.Interfaces {
				mac, err := net.ParseMAC(iface.MAC)
				if err != nil || len(mac) != 6 || mac[0]&1 != 0 || bytes.Equal(mac, make([]byte, 6)) || iface.Index <= 0 {
					return IngressMatch{}, invalid
				}
				if target.NodeID == localNode && iface.Index == index {
					if local != nil && *local != ref {
						return IngressMatch{}, invalid
					}
					copy := ref
					local = &copy
				}
				if bytes.Equal(mac, source) {
					if target.NodeID == localNode || peer != nil && *peer != ref {
						return IngressMatch{}, invalid
					}
					copy := ref
					peer = &copy
				}
			}
		}
	}
	if local == nil || peer == nil {
		return IngressMatch{}, invalid
	}
	return IngressMatch{Local: *local, Peer: *peer, Sequence: frame.Sequence}, nil
}
