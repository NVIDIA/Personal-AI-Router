// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mdns

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"slices"
	"strings"
	"testing"
	"time"
)

type recordingPacketWriter struct {
	writes  int
	payload []byte
	target  net.Addr
	err     error
}

func (w *recordingPacketWriter) WriteTo(payload []byte, target net.Addr) (int, error) {
	w.writes++
	w.payload = append([]byte(nil), payload...)
	w.target = target
	if w.err != nil {
		return 0, w.err
	}
	return len(payload), nil
}

type recordingMulticastOptions struct {
	interfaces     []*net.Interface
	ttls           []int
	interfaceErr   error
	ttlErr         error
	fallbackTTLErr error
}

func (o *recordingMulticastOptions) SetMulticastInterface(ifi *net.Interface) error {
	o.interfaces = append(o.interfaces, ifi)
	return o.interfaceErr
}

func (o *recordingMulticastOptions) SetMulticastTTL(ttl int) error {
	o.ttls = append(o.ttls, ttl)
	if ttl == fallbackMulticastTTL {
		return o.fallbackTTLErr
	}
	return o.ttlErr
}

func captureDebugLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	logs := &bytes.Buffer{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs
}

func TestWritePacketConfiguresMulticastAndWritesOnce(t *testing.T) {
	ifi := &net.Interface{Index: 7, Name: "eth0"}
	source := net.IPv4(192, 0, 2, 10)
	target := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}
	payload := []byte("multicast payload")
	writer := &recordingPacketWriter{}
	options := &recordingMulticastOptions{}

	if err := writePacket(payload, ifi, source, target, writer, options); err != nil {
		t.Fatalf("writePacket: %v", err)
	}
	if len(options.interfaces) != 1 || options.interfaces[0] != ifi {
		t.Fatalf("multicast interfaces = %v, want [%v]", options.interfaces, ifi)
	}
	if len(options.ttls) != 1 || options.ttls[0] != preferredMulticastTTL {
		t.Fatalf("multicast TTLs = %v, want [255]", options.ttls)
	}
	if writer.writes != 1 {
		t.Fatalf("writes = %d, want 1", writer.writes)
	}
	if !bytes.Equal(writer.payload, payload) {
		t.Errorf("payload = %q, want %q", writer.payload, payload)
	}
	if writer.target != target {
		t.Errorf("target = %v, want %v", writer.target, target)
	}
}

func TestWritePacketSkipsMulticastOptionsForUnicast(t *testing.T) {
	source := net.IPv4(127, 0, 0, 1)
	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 14318}
	writer := &recordingPacketWriter{}
	options := &recordingMulticastOptions{}

	if err := writePacket([]byte("unicast payload"), nil, source, target, writer, options); err != nil {
		t.Fatalf("writePacket: %v", err)
	}
	if len(options.interfaces) != 0 || len(options.ttls) != 0 {
		t.Fatalf("multicast options used for unicast: interfaces=%v TTLs=%v", options.interfaces, options.ttls)
	}
	if writer.writes != 1 {
		t.Fatalf("writes = %d, want 1", writer.writes)
	}
}

func TestWritePacketReturnsWriteFailure(t *testing.T) {
	wantErr := errors.New("send refused")
	writer := &recordingPacketWriter{err: wantErr}
	source := net.IPv4(127, 0, 0, 1)
	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 14318}

	err := writePacket([]byte("unicast payload"), nil, source, target, writer, &recordingMulticastOptions{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want wrapped %v", err, wantErr)
	}
	if writer.writes != 1 {
		t.Fatalf("writes = %d, want 1", writer.writes)
	}
}

func TestWritePacketLogsMulticastOptionFailuresAndStillWrites(t *testing.T) {
	cases := []struct {
		name           string
		interfaceErr   error
		ttlErr         error
		fallbackTTLErr error
		wantTTLs       []int
		wantMessages   []string
	}{
		{
			name:         "interface",
			interfaceErr: errors.New("interface unavailable"),
			wantTTLs:     []int{255},
			wantMessages: []string{"set multicast interface failed"},
		},
		{
			name:         "TTL fallback",
			ttlErr:       errors.New("TTL unavailable"),
			wantTTLs:     []int{255, 1},
			wantMessages: []string{"set multicast TTL failed"},
		},
		{
			name:         "both",
			interfaceErr: errors.New("interface unavailable"),
			ttlErr:       errors.New("TTL unavailable"),
			wantTTLs:     []int{255, 1},
			wantMessages: []string{"set multicast interface failed", "set multicast TTL failed"},
		},
		{
			name:           "TTL fallback failure",
			ttlErr:         errors.New("TTL unavailable"),
			fallbackTTLErr: errors.New("fallback TTL unavailable"),
			wantTTLs:       []int{255, 1},
			wantMessages:   []string{"set multicast TTL failed", "set multicast fallback TTL failed"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureDebugLogs(t)
			ifi := &net.Interface{Index: 7, Name: "eth0"}
			source := net.IPv4(192, 0, 2, 10)
			target := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}
			writer := &recordingPacketWriter{}
			options := &recordingMulticastOptions{
				interfaceErr:   tc.interfaceErr,
				ttlErr:         tc.ttlErr,
				fallbackTTLErr: tc.fallbackTTLErr,
			}

			if err := writePacket([]byte("multicast payload"), ifi, source, target, writer, options); err != nil {
				t.Fatalf("writePacket: %v", err)
			}
			if len(options.interfaces) != 1 {
				t.Fatalf("interface attempts = %d, want 1", len(options.interfaces))
			}
			if !slices.Equal(options.ttls, tc.wantTTLs) {
				t.Fatalf("multicast TTLs = %v, want %v", options.ttls, tc.wantTTLs)
			}
			if writer.writes != 1 {
				t.Fatalf("writes = %d, want 1", writer.writes)
			}

			gotLogs := logs.String()
			for _, message := range tc.wantMessages {
				if !strings.Contains(gotLogs, message) {
					t.Errorf("logs missing %q:\n%s", message, gotLogs)
				}
			}
			for _, field := range []string{"iface=eth0", "ip=192.0.2.10", "target=224.0.0.251:5353"} {
				if !strings.Contains(gotLogs, field) {
					t.Errorf("logs missing %q:\n%s", field, gotLogs)
				}
			}
		})
	}
}

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
