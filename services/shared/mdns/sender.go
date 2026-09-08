// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mdns

import (
	"context"
	"errors"
	"fmt"
	"net"

	"golang.org/x/net/ipv4"
)

const mdnsPort = 5353

// SendFromInterface transmits one IPv4 mDNS packet from the selected address
// and the RFC 6762 source port. A fresh source-bound socket preserves reliable
// per-interface egress on Windows while the reuse controls let it coexist with
// the long-lived mDNS receive sockets on every supported platform.
func SendFromInterface(buf []byte, ifi *net.Interface, src net.IP, target *net.UDPAddr) error {
	source := src.To4()
	if source == nil {
		return errors.New("mDNS source is not IPv4")
	}
	if target == nil || target.IP.To4() == nil || target.Port == 0 {
		return errors.New("mDNS target is not a valid IPv4 endpoint")
	}

	lc := net.ListenConfig{Control: setSenderReuse}
	conn, err := lc.ListenPacket(
		context.Background(),
		"udp4",
		(&net.UDPAddr{IP: source, Port: mdnsPort}).String(),
	)
	if err != nil {
		return fmt.Errorf("bind mDNS sender: %w", err)
	}
	defer conn.Close()

	if target.IP.IsMulticast() {
		if ifi == nil {
			return errors.New("mDNS multicast target requires an interface")
		}
		pc := ipv4.NewPacketConn(conn)
		if err := pc.SetMulticastInterface(ifi); err != nil {
			return fmt.Errorf("set mDNS multicast interface: %w", err)
		}
		if err := pc.SetMulticastTTL(255); err != nil {
			return fmt.Errorf("set mDNS multicast TTL: %w", err)
		}
	}

	if _, err := conn.WriteTo(buf, target); err != nil {
		return fmt.Errorf("write mDNS packet: %w", err)
	}
	return nil
}
