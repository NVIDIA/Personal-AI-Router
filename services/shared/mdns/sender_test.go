// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mdns

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	require.NoError(t, writePacket(payload, ifi, source, target, writer, options), "writePacket")
	require.Len(t, options.interfaces, 1, "multicast interfaces")
	assert.Same(t, ifi, options.interfaces[0], "multicast interfaces")
	require.Len(t, options.ttls, 1, "multicast TTLs")
	assert.Equal(t, preferredMulticastTTL, options.ttls[0], "multicast TTLs")
	assert.Equal(t, 1, writer.writes)
	assert.Equal(t, payload, writer.payload)
	assert.Same(t, target, writer.target)
}

func TestWritePacketSkipsMulticastOptionsForUnicast(t *testing.T) {
	source := net.IPv4(127, 0, 0, 1)
	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 14318}
	writer := &recordingPacketWriter{}
	options := &recordingMulticastOptions{}

	require.NoError(t, writePacket([]byte("unicast payload"), nil, source, target, writer, options), "writePacket")
	require.Empty(t, options.interfaces, "multicast options used for unicast: interfaces")
	require.Empty(t, options.ttls, "multicast options used for unicast: TTLs")
	assert.Equal(t, 1, writer.writes)
}

func TestWritePacketReturnsWriteFailure(t *testing.T) {
	wantErr := errors.New("send refused")
	writer := &recordingPacketWriter{err: wantErr}
	source := net.IPv4(127, 0, 0, 1)
	target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 14318}

	require.ErrorIs(t, writePacket([]byte("unicast payload"), nil, source, target, writer, &recordingMulticastOptions{}), wantErr)
	assert.Equal(t, 1, writer.writes)
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

			require.NoError(t, writePacket([]byte("multicast payload"), ifi, source, target, writer, options), "writePacket")
			require.Len(t, options.interfaces, 1, "interface attempts")
			assert.Equal(t, tc.wantTTLs, options.ttls, "multicast TTLs")
			assert.Equal(t, 1, writer.writes)

			gotLogs := logs.String()
			for _, message := range tc.wantMessages {
				assert.Contains(t, gotLogs, message, "logs missing")
			}
			assert.Contains(t, gotLogs, "iface=eth0", "logs missing")
			assert.Contains(t, gotLogs, "ip=192.0.2.10", "logs missing")
			assert.Contains(t, gotLogs, "target=224.0.0.251:5353", "logs missing")
		})
	}
}

func TestResponderSendUsesMDNSSourcePortAlongsideReceiver(t *testing.T) {
	ifi, source := loopbackIPv4(t)

	lc := net.ListenConfig{Control: setReuseAddr}
	receiveSocket, err := lc.ListenPacket(context.Background(), "udp4", mdnsTargetV4.String())
	require.NoError(t, err, "open reusable mDNS receive socket")
	defer receiveSocket.Close()

	sink, err := net.ListenUDP("udp4", &net.UDPAddr{IP: source, Port: 0})
	require.NoError(t, err, "open UDP sink")
	defer sink.Close()
	require.NoError(t, sink.SetReadDeadline(time.Now().Add(2*time.Second)), "set sink deadline")

	target, ok := sink.LocalAddr().(*net.UDPAddr)
	require.True(t, ok, "sink address has type")
	responder := &Responder{
		ifaceAddrs: map[int][]net.IP{
			ifi.Index: {source},
		},
	}
	payload := []byte("mDNS source-port regression")
	require.NoError(t, responder.sendOnInterface(payload, ifi.Index, target), "sendOnInterface")

	buf := make([]byte, len(payload))
	n, from, err := sink.ReadFromUDP(buf)
	require.NoError(t, err, "read UDP sink")
	assert.Equal(t, payload, buf[:n])
	assert.Equal(t, source.String(), from.IP.String(), "source IP")
	assert.Equal(t, mdnsPort, from.Port, "source port")
}

func loopbackIPv4(t *testing.T) (*net.Interface, net.IP) {
	t.Helper()
	ifaces, err := net.Interfaces()
	require.NoError(t, err, "enumerate interfaces")
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
