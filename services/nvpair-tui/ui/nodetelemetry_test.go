// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/noderec"
	"nvpair-tui/rpc"
)

// TestTelemetryReplyIsGenerationScoped is the companion guard: the reply, not
// the tick, is what schedules the next poll, so a reply from a previous visit
// to the same node would start a second chain alongside the current one and
// double the poll rate on every close-and-reopen.
func TestTelemetryReplyIsGenerationScoped(t *testing.T) {
	node := nodeRow{key: "n1", name: "n1", address: "10.0.0.4", port: 14318}
	first := newNodeDetail(nil, node)
	second := newNodeDetail(nil, node)
	second.SetSize(100, 30)

	// The older screen's in-flight poll lands on the newer screen.
	cmd, _ := second.update(nodeTelemetryMsg{
		nodeKey: node.key, gen: first.telemetryGen,
		telemetry: nodeTelemetry{TelemetryValid: true},
	})
	assert.Nil(t, cmd, "a superseded chain's reply scheduled another poll; the chain will double")

	// Its own reply does continue the chain.
	cmd, _ = second.update(nodeTelemetryMsg{
		nodeKey: node.key, gen: second.telemetryGen,
		telemetry: nodeTelemetry{TelemetryValid: true},
	})
	assert.NotNil(t, cmd, "the screen's own reply did not schedule the next poll; telemetry stops")
}

// TestTelemetryStartsWhenAnAddressArrivesLate checks a node opened before
// discovery resolved it still gets telemetry. Nothing schedules a tick when
// there is nothing to poll, and the reply is what continues the chain, so
// without an explicit restart the panel read "unavailable" for the whole visit.
func TestTelemetryStartsWhenAnAddressArrivesLate(t *testing.T) {
	d := newNodeDetail(nil, nodeRow{key: "peer", name: "peer"}) // no address yet
	d.SetSize(100, 30)

	require.Nil(t, d.telemetryCmd(), "polled a node with no address")
	require.False(t, d.telemetryRunning, "claims a chain is running with nothing to poll")

	params, _ := json.Marshal([]availableNode{{
		HostUUID: "peer", Name: "peer", IPAddress: "10.0.0.9", Port: 14318,
	}})
	cmd := d.handleNotification(&rpc.Message{
		Method: "discovery:nodes-changed", Params: params,
	})

	assert.NotNil(t, cmd, "an address arriving did not start the telemetry chain")
	assert.Equal(t, "10.0.0.9", d.node.address, "address discovery reported")
}

// TestTelemetryChainsAreGenerationScoped is the regression guard for polling
// chains piling up. Bubble Tea cannot cancel a pending tick, so closing and
// re-opening a node's detail screen left the old chain's tick in flight; keyed
// on the node alone it was accepted and extended, and every re-open added
// another chain polling the same endpoint forever.
func TestTelemetryChainsAreGenerationScoped(t *testing.T) {
	node := nodeRow{key: "n1", name: "n1", address: "10.0.0.4", port: 14318}

	first := newNodeDetail(nil, node)
	second := newNodeDetail(nil, node)
	require.NotEqual(t, first.telemetryGen, second.telemetryGen, "two detail screens share a chain id, so neither can retire the other's ticks")

	// The newer screen ignores the older chain's tick.
	cmd, _ := second.update(nodeTelemetryTickMsg{
		nodeKey: node.key, gen: first.telemetryGen,
	})
	assert.Nil(t, cmd, "a superseded chain's tick was extended; polling chains will accumulate")

	// And still continues its own.
	cmd, _ = second.update(nodeTelemetryTickMsg{
		nodeKey: node.key, gen: second.telemetryGen,
	})
	assert.NotNil(t, cmd, "the screen's own tick did not continue its chain")
}

// TestPollTelemetryWalksEveryAddress checks the poll tries a node's other
// published addresses. A multi-homed node's first address may be a link this
// machine cannot reach, and giving up on it reported the node as having no
// telemetry at all.
func TestPollTelemetryWalksEveryAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"telemetryValid":true}`))
	}))
	defer srv.Close()

	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	require.NoError(t, err, "split test server address")
	p, _ := strconv.Atoi(port)

	// An unreachable address first — the reserved TEST-NET-1 block — then the
	// one that answers.
	cmd := pollTelemetryCmd("n1", 1, []string{"192.0.2.1", host}, p, "")
	msg, ok := cmd().(nodeTelemetryMsg)
	require.True(t, ok, "poll must produce nodeTelemetryMsg")
	assert.NoError(t, msg.err, "poll failed despite a reachable second address")
	assert.True(t, msg.telemetry.TelemetryValid, "no telemetry decoded from the address that answered")
}

// TestNodeInfoURL pins the endpoint shape, including the fallback for an entry
// whose node-info port is not known yet.
func TestNodeInfoURL(t *testing.T) {
	assert.Equal(t, "http://10.0.0.5:14318/v1/node-info", nodeInfoURL("10.0.0.5", 14318))
	assert.Contains(t, nodeInfoURL("10.0.0.5", 0), strconv.Itoa(nodeInfoDefaultPort), "default port when none is known")
	// An IPv6 literal has to be bracketed or the port parses as part of the host.
	assert.Contains(t, nodeInfoURL("fe80::1", 14318), "[fe80::1]:14318", "bracketed IPv6 host")
}

// TestTelemetryHostUsesLoopbackForSelf checks this machine is polled over
// loopback: its advertised address may be a link only peers can reach.
func TestTelemetryHostsUseLoopbackForSelf(t *testing.T) {
	assert.Equal(t, []string{nodeInfoSelfHost}, telemetryHosts(nodeRow{self: true, address: "10.0.0.5"}))
	assert.Equal(t, []string{"10.0.0.5"}, telemetryHosts(nodeRow{address: "10.0.0.5"}))
}

// TestPollTelemetryDecodesResponse exercises the real HTTP path against a stub
// serving the node-info contract, so the JSON tags stay pinned to the producer's
// (notably the capitalised "GPUs" key).
func TestPollTelemetryDecodesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, nodeInfoPath, r.URL.Path, "polled endpoint")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"GPUs":[{"name":"Test GPU","vram_bytes":8589934592,"vram_used_bytes":1073741824,"utilization_percent":42}],
			"cpu":{"name":"Test CPU","cores":8,"utilization_percent":13},
			"memory":{"total_bytes":34359738368,"used_bytes":8589934592},
			"telemetryValid":true,
			"msSince":120
		}`))
	}))
	defer srv.Close()

	host, port := splitTestServer(t, srv.URL)
	msg, ok := pollTelemetryCmd("key", 1, []string{host}, port, "")().(nodeTelemetryMsg)
	require.True(t, ok, "poll produced the wrong message type")
	require.NoError(t, msg.err, "poll failed")
	assert.Equal(t, "key", msg.nodeKey, "key the poll was asked for")
	require.Len(t, msg.telemetry.GPUs, 1, "check the \"GPUs\" JSON key")
	assert.Equal(t, uint32(42), msg.telemetry.GPUs[0].UtilizationPercent)
	if assert.NotNil(t, msg.telemetry.CPU, "CPU block did not decode") {
		assert.Equal(t, uint32(8), msg.telemetry.CPU.Cores, "CPU block did not decode")
	}
	if assert.NotNil(t, msg.telemetry.Memory, "memory block did not decode") {
		assert.NotZero(t, msg.telemetry.Memory.TotalBytes, "memory block did not decode")
	}
	assert.True(t, msg.telemetry.TelemetryValid, "telemetryValid did not decode")
}

// TestPollTelemetryReportsFailure checks a non-200 is an error rather than being
// decoded as an empty reading, which would render as a machine with no hardware.
func TestPollTelemetryReportsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	host, port := splitTestServer(t, srv.URL)
	msg := pollTelemetryCmd("key", 1, []string{host}, port, "")().(nodeTelemetryMsg)
	assert.Error(t, msg.err, "a 403 was treated as a successful reading")
}

// TestPollTelemetrySkipsUnknownAddress checks a node with no address issues no
// request at all.
func TestPollTelemetrySkipsUnknownAddress(t *testing.T) {
	assert.Nil(t, pollTelemetryCmd("key", 1, nil, 14318, ""), "polled a node with no known address")
}

// TestTelemetryFromAnotherMachineIsRefused is the regression guard for an
// address that has passed to another machine. Its readings were shown under the
// name of the machine that used to hold it.
func TestTelemetryFromAnotherMachineIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"telemetryValid":true,"hostUuid":"uuid-b"}`))
	}))
	defer srv.Close()
	host, port := splitTestServer(t, srv.URL)

	assert.Error(t, pollTelemetryCmd("uuid-a", 1, []string{host}, port, "uuid-a")().(nodeTelemetryMsg).err, "another machine's readings were accepted for this one")
	assert.NoError(t, pollTelemetryCmd("uuid-b", 1, []string{host}, port, "uuid-b")().(nodeTelemetryMsg).err, "the machine's own readings were refused")
	// A row with nothing to check against takes whichever machine answers.
	assert.NoError(t, pollTelemetryCmd("manual:1", 1, []string{host}, port, "")().(nodeTelemetryMsg).err, "an unchecked poll was refused")

	assert.Equal(t, "uuid-a", telemetryIdentity(nodeRow{key: "uuid-a"}), "a discovered node is checked against its UUID")
	for _, row := range []nodeRow{{key: "manual:1"}, {key: "name:lab"}, {key: "uuid-self", self: true}} {
		assert.Empty(t, telemetryIdentity(row), "%q has nothing to check", row.key)
	}
}

// TestTelemetryTriesTheAddressThatAnswered is the regression guard for a poll
// that walked every unreachable address, each to its full timeout, before the
// one that had answered last time.
func TestTelemetryTriesTheAddressThatAnswered(t *testing.T) {
	addresses := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
	assert.Equal(t, []string{"10.0.0.3", "10.0.0.1", "10.0.0.2"}, preferAddress(addresses, "10.0.0.3"), "address that answered first")
	for _, last := range []string{"", "10.0.0.1", "10.9.9.9"} {
		assert.Equal(t, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, preferAddress(addresses, last), "after %q: node's own order", last)
	}
	assert.Equal(t, []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}, addresses, "the node's own address list was reordered in place")

	d := newNodeDetail(nil, nodeRow{key: "peer", addresses: addresses, presence: presenceOnline})
	d.update(nodeTelemetryMsg{nodeKey: "peer", gen: d.telemetryGen, address: "10.0.0.3"})
	assert.Equal(t, "10.0.0.3", d.telemetryAddress, "address that answered is kept")
	d.update(nodeTelemetryMsg{nodeKey: "peer", gen: d.telemetryGen, err: errors.New("unreachable")})
	assert.Empty(t, d.telemetryAddress, "an address that stopped answering was still tried first")
}

// TestTelemetrySummaryOmitsUtilizationWhenStale checks an unusable sample does
// not print 0%, which would read as an idle GPU.
func TestTelemetrySummaryOmitsUtilizationWhenStale(t *testing.T) {
	tel := nodeTelemetry{
		GPUs:           []noderec.GPUInfo{{Name: "GPU", VramBytes: 1 << 30, UtilizationPercent: 0}},
		TelemetryValid: false,
	}
	line := strings.Join(tel.summary(), "\n")
	assert.NotContains(t, line, "0%", "stale sample rendered as zero utilization")
	assert.Contains(t, line, "--", "stale sample should read as unknown")

	tel.TelemetryValid = true
	tel.GPUs[0].UtilizationPercent = 55
	assert.Contains(t, strings.Join(tel.summary(), "\n"), "55%", "valid sample renders utilization")
}

func TestTelemetrySummaryEmpty(t *testing.T) {
	assert.Empty(t, (nodeTelemetry{}).summary())
}

func TestHumanBytes(t *testing.T) {
	cases := map[uint64]string{
		0:              "0 B",
		512:            "512 B",
		1024:           "1.0 KiB",
		1 << 30:        "1.0 GiB",
		8 * (1 << 30):  "8.0 GiB",
		32 * (1 << 30): "32.0 GiB",
		// At three digits the decimal stops earning its place.
		128 * (1 << 30): "128 GiB",
		4 * (1 << 40):   "4.0 TiB",
	}
	for in, want := range cases {
		assert.Equal(t, want, humanBytes(in), "humanBytes(%d)", in)
	}
}

// TestDetailHardwareUnavailableWhenPollFails checks the panel is explicit rather
// than silently blank when a node cannot be reached.
func TestDetailHardwareUnavailableWhenPollFails(t *testing.T) {
	d := newNodeDetail(nil, nodeRow{key: "k", name: "peer", presence: presenceOffline})
	assert.Contains(t, d.hardwareBlock(), "not reachable", "hardware block explains the failure")

	d.node.presence = presenceOnline
	assert.Contains(t, d.hardwareBlock(), "unavailable")

	d.telemetryOK = true
	d.telemetry = nodeTelemetry{
		GPUs:           []noderec.GPUInfo{{Name: "Test GPU", VramBytes: 1 << 30, UtilizationPercent: 7}},
		TelemetryValid: true,
	}
	assert.Contains(t, d.hardwareBlock(), "Test GPU", "hardware block includes GPU name")
}

// TestDetailIgnoresTelemetryForOtherNodes checks a late reply for a node the
// operator has navigated away from does not overwrite the current one.
func TestDetailIgnoresTelemetryForOtherNodes(t *testing.T) {
	d := newNodeDetail(nil, nodeRow{key: "current", self: true})
	d.update(nodeTelemetryMsg{
		nodeKey: "stale", gen: d.telemetryGen,
		telemetry: nodeTelemetry{TelemetryValid: true},
	})
	assert.False(t, d.telemetryOK, "accepted a reading addressed to a different node")
}

func splitTestServer(t *testing.T, raw string) (string, int) {
	t.Helper()
	u, err := url.Parse(raw)
	require.NoError(t, err, "parse test server url")
	port, err := strconv.Atoi(u.Port())
	require.NoError(t, err, "parse test server port")
	return u.Hostname(), port
}
