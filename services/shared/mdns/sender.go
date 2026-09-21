// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mdns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"golang.org/x/net/ipv4"
)

const mdnsPort = 5353

type packetWriter interface {
	WriteTo([]byte, net.Addr) (int, error)
}

type multicastOptions interface {
	SetMulticastInterface(*net.Interface) error
	SetMulticastTTL(int) error
}

// SendFromInterface transmits one IPv4 mDNS packet from the selected address
// and the RFC 6762 source port. A fresh source-bound socket preserves reliable
// per-interface egress on Windows while the reuse controls let it coexist with
// the long-lived mDNS receive sockets on every supported platform. Multicast
// interface and TTL options are advisory: failures are logged before the packet
// is written using the bound source and the socket's remaining defaults.
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

	return writePacket(buf, ifi, source, target, conn, ipv4.NewPacketConn(conn))
}

func writePacket(
	buf []byte,
	ifi *net.Interface,
	source net.IP,
	target *net.UDPAddr,
	conn packetWriter,
	options multicastOptions,
) error {
	if target.IP.IsMulticast() {
		if ifi == nil {
			return errors.New("mDNS multicast target requires an interface")
		}
		if err := options.SetMulticastInterface(ifi); err != nil {
			slog.Debug("mdns: set multicast interface failed; sending with socket route",
				"iface", ifi.Name,
				"ip", source.String(),
				"target", target.String(),
				"err", err)
		}
		if err := options.SetMulticastTTL(255); err != nil {
			slog.Debug("mdns: set multicast TTL failed; sending with socket default",
				"iface", ifi.Name,
				"ip", source.String(),
				"target", target.String(),
				"ttl", 255,
				"err", err)
		}
	}

	if _, err := conn.WriteTo(buf, target); err != nil {
		return fmt.Errorf("write mDNS packet: %w", err)
	}
	return nil
}
