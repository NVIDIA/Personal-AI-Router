// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mdns

import (
	"bytes"
	"context"
	"net"
	"testing"
	"time"
)

func TestResponderSendUsesMDNSSourcePortAlongsideReceiver(t *testing.T) {
	ifi, source := loopbackIPv4(t)

	lc := net.ListenConfig{Control: setReuseAddr}
	receiveSocket, err := lc.ListenPacket(context.Background(), "udp4", mdnsTargetV4.String())
	if err != nil {
		t.Fatalf("open reusable mDNS receive socket: %v", err)
	}
	defer receiveSocket.Close()

	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: source, Port: 0})
	if err != nil {
		t.Fatalf("open UDP sink: %v", err)
	}
	defer sink.Close()
	if err := sink.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set sink deadline: %v", err)
	}

	target, ok := sink.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("sink address has type %T, want *net.UDPAddr", sink.LocalAddr())
	}
	responder := &Responder{
		ifaceAddrs: map[int][]net.IP{
			ifi.Index: {source},
		},
	}
	payload := []byte("mDNS source-port regression")
	if err := responder.sendOnInterface(payload, ifi.Index, target); err != nil {
		t.Fatalf("sendOnInterface: %v", err)
	}

	buf := make([]byte, len(payload))
	n, from, err := sink.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("read UDP sink: %v", err)
	}
	if !bytes.Equal(buf[:n], payload) {
		t.Fatalf("payload = %q, want %q", buf[:n], payload)
	}
	if !from.IP.Equal(source) {
		t.Errorf("source IP = %s, want %s", from.IP, source)
	}
	if from.Port != mdnsPort {
		t.Errorf("source port = %d, want %d", from.Port, mdnsPort)
	}
}

func loopbackIPv4(t *testing.T) (*net.Interface, net.IP) {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("enumerate interfaces: %v", err)
	}
	for i := range ifaces {
		ifi := &ifaces[i]
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback == 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				return ifi, ip4
			}
		}
	}
	t.Skip("no up IPv4 loopback interface")
	return nil, nil
}
