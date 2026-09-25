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

const (
	mdnsPort              = 5353
	preferredMulticastTTL = 255
	fallbackMulticastTTL  = 1
)

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
// interface selection is advisory, and TTL setup falls back explicitly from
// RFC 6762's preferred 255 to link-local 1. Failures are logged but never
// suppress the packet write.
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
		if err := options.SetMulticastTTL(preferredMulticastTTL); err != nil {
			slog.Debug("mdns: set multicast TTL failed; retrying with TTL 1",
				"iface", ifi.Name,
				"ip", source.String(),
				"target", target.String(),
				"ttl", preferredMulticastTTL,
				"err", err)
			if fallbackErr := options.SetMulticastTTL(fallbackMulticastTTL); fallbackErr != nil {
				slog.Debug("mdns: set multicast fallback TTL failed; sending with socket default",
					"iface", ifi.Name,
					"ip", source.String(),
					"target", target.String(),
					"ttl", fallbackMulticastTTL,
					"err", fallbackErr)
			}
		}
	}

	if _, err := conn.WriteTo(buf, target); err != nil {
		return fmt.Errorf("write mDNS packet: %w", err)
	}
	return nil
}
