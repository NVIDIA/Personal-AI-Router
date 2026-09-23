// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

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
	if cmd != nil {
		t.Error("a superseded chain's reply scheduled another poll; the chain will double")
	}

	// Its own reply does continue the chain.
	cmd, _ = second.update(nodeTelemetryMsg{
		nodeKey: node.key, gen: second.telemetryGen,
		telemetry: nodeTelemetry{TelemetryValid: true},
	})
	if cmd == nil {
		t.Error("the screen's own reply did not schedule the next poll; telemetry stops")
	}
}

// TestTelemetryStartsWhenAnAddressArrivesLate checks a node opened before
// discovery resolved it still gets telemetry. Nothing schedules a tick when
// there is nothing to poll, and the reply is what continues the chain, so
// without an explicit restart the panel read "unavailable" for the whole visit.
func TestTelemetryStartsWhenAnAddressArrivesLate(t *testing.T) {
	d := newNodeDetail(nil, nodeRow{key: "peer", name: "peer"}) // no address yet
	d.SetSize(100, 30)

	if cmd := d.telemetryCmd(); cmd != nil {
		t.Fatal("polled a node with no address")
	}
	if d.telemetryRunning {
		t.Fatal("claims a chain is running with nothing to poll")
	}

	params, _ := json.Marshal([]availableNode{{
		HostUUID: "peer", Name: "peer", IPAddress: "10.0.0.9", Port: 14318,
	}})
	cmd := d.handleNotification(&rpc.Message{
		Method: "discovery:nodes-changed", Params: params,
	})

	if cmd == nil {
		t.Error("an address arriving did not start the telemetry chain")
	}
	if d.node.address != "10.0.0.9" {
		t.Errorf("address = %q, want the one discovery reported", d.node.address)
	}
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
	if first.telemetryGen == second.telemetryGen {
		t.Fatal("two detail screens share a chain id, so neither can retire the other's ticks")
	}

	// The newer screen ignores the older chain's tick.
	if cmd, _ := second.update(nodeTelemetryTickMsg{
		nodeKey: node.key, gen: first.telemetryGen,
	}); cmd != nil {
		t.Error("a superseded chain's tick was extended; polling chains will accumulate")
	}

	// And still continues its own.
	if cmd, _ := second.update(nodeTelemetryTickMsg{
		nodeKey: node.key, gen: second.telemetryGen,
	}); cmd == nil {
		t.Error("the screen's own tick did not continue its chain")
	}
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
	if err != nil {
		t.Fatalf("split test server address: %v", err)
	}
	p, _ := strconv.Atoi(port)

	// An unreachable address first — the reserved TEST-NET-1 block — then the
	// one that answers.
	cmd := pollTelemetryCmd("n1", 1, []string{"192.0.2.1", host}, p)
	msg, ok := cmd().(nodeTelemetryMsg)
	if !ok {
		t.Fatalf("got %T, want nodeTelemetryMsg", cmd())
	}
	if msg.err != nil {
		t.Errorf("poll failed despite a reachable second address: %v", msg.err)
	}
	if !msg.telemetry.TelemetryValid {
		t.Error("no telemetry decoded from the address that answered")
	}
}

// TestNodeInfoURL pins the endpoint shape, including the fallback for an entry
// whose node-info port is not known yet.
func TestNodeInfoURL(t *testing.T) {
	if got := nodeInfoURL("10.0.0.5", 14318); got != "http://10.0.0.5:14318/v1/node-info" {
		t.Errorf("url = %q", got)
	}
	if got := nodeInfoURL("10.0.0.5", 0); !strings.Contains(got, strconv.Itoa(nodeInfoDefaultPort)) {
		t.Errorf("url = %q, want the default port when none is known", got)
	}
	// An IPv6 literal has to be bracketed or the port parses as part of the host.
	if got := nodeInfoURL("fe80::1", 14318); !strings.Contains(got, "[fe80::1]:14318") {
		t.Errorf("url = %q, want a bracketed IPv6 host", got)
	}
}

// TestTelemetryHostUsesLoopbackForSelf checks this machine is polled over
// loopback: its advertised address may be a link only peers can reach.
func TestTelemetryHostsUseLoopbackForSelf(t *testing.T) {
	if got := telemetryHosts(nodeRow{self: true, address: "10.0.0.5"}); len(got) != 1 || got[0] != nodeInfoSelfHost {
		t.Errorf("self hosts = %v, want just %q", got, nodeInfoSelfHost)
	}
	if got := telemetryHosts(nodeRow{address: "10.0.0.5"}); len(got) != 1 || got[0] != "10.0.0.5" {
		t.Errorf("remote hosts = %v", got)
	}
}

// TestPollTelemetryDecodesResponse exercises the real HTTP path against a stub
// serving the node-info contract, so the JSON tags stay pinned to the producer's
// (notably the capitalised "GPUs" key).
func TestPollTelemetryDecodesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != nodeInfoPath {
			t.Errorf("polled %q, want %q", r.URL.Path, nodeInfoPath)
		}
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
	msg, ok := pollTelemetryCmd("key", 1, []string{host}, port)().(nodeTelemetryMsg)
	if !ok {
		t.Fatal("poll produced the wrong message type")
	}
	if msg.err != nil {
		t.Fatalf("poll failed: %v", msg.err)
	}
	if msg.nodeKey != "key" {
		t.Errorf("nodeKey = %q, want the key it was asked for", msg.nodeKey)
	}
	if len(msg.telemetry.GPUs) != 1 {
		t.Fatalf("decoded %d GPUs, want 1 — check the \"GPUs\" JSON key", len(msg.telemetry.GPUs))
	}
	if got := msg.telemetry.GPUs[0].UtilizationPercent; got != 42 {
		t.Errorf("GPU utilization = %d, want 42", got)
	}
	if msg.telemetry.CPU == nil || msg.telemetry.CPU.Cores != 8 {
		t.Error("CPU block did not decode")
	}
	if msg.telemetry.Memory == nil || msg.telemetry.Memory.TotalBytes == 0 {
		t.Error("memory block did not decode")
	}
	if !msg.telemetry.TelemetryValid {
		t.Error("telemetryValid did not decode")
	}
}

// TestPollTelemetryReportsFailure checks a non-200 is an error rather than being
// decoded as an empty reading, which would render as a machine with no hardware.
func TestPollTelemetryReportsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	host, port := splitTestServer(t, srv.URL)
	msg := pollTelemetryCmd("key", 1, []string{host}, port)().(nodeTelemetryMsg)
	if msg.err == nil {
		t.Error("a 403 was treated as a successful reading")
	}
}

// TestPollTelemetrySkipsUnknownAddress checks a node with no address issues no
// request at all.
func TestPollTelemetrySkipsUnknownAddress(t *testing.T) {
	if cmd := pollTelemetryCmd("key", 1, nil, 14318); cmd != nil {
		t.Error("polled a node with no known address")
	}
}

// TestTelemetrySummaryOmitsUtilizationWhenStale checks an unusable sample does
// not print 0%, which would read as an idle GPU.
func TestTelemetrySummaryOmitsUtilizationWhenStale(t *testing.T) {
	tel := nodeTelemetry{
		GPUs:           []noderec.GPUInfo{{Name: "GPU", VramBytes: 1 << 30, UtilizationPercent: 0}},
		TelemetryValid: false,
	}
	line := strings.Join(tel.summary(), "\n")
	if strings.Contains(line, "0%") {
		t.Errorf("stale sample rendered as 0%% utilization: %q", line)
	}
	if !strings.Contains(line, "--") {
		t.Errorf("stale sample should read as unknown: %q", line)
	}

	tel.TelemetryValid = true
	tel.GPUs[0].UtilizationPercent = 55
	if got := strings.Join(tel.summary(), "\n"); !strings.Contains(got, "55%") {
		t.Errorf("valid sample did not render utilization: %q", got)
	}
}

func TestTelemetrySummaryEmpty(t *testing.T) {
	if lines := (nodeTelemetry{}).summary(); len(lines) != 0 {
		t.Errorf("empty telemetry produced %d lines", len(lines))
	}
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
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestDetailHardwareUnavailableWhenPollFails checks the panel is explicit rather
// than silently blank when a node cannot be reached.
func TestDetailHardwareUnavailableWhenPollFails(t *testing.T) {
	d := newNodeDetail(nil, nodeRow{key: "k", name: "peer", presence: presenceOffline})
	if got := d.hardwareBlock(); !strings.Contains(got, "not reachable") {
		t.Errorf("hardware block = %q, want an explanation", got)
	}

	d.node.presence = presenceOnline
	if got := d.hardwareBlock(); !strings.Contains(got, "unavailable") {
		t.Errorf("hardware block = %q", got)
	}

	d.telemetryOK = true
	d.telemetry = nodeTelemetry{
		GPUs:           []noderec.GPUInfo{{Name: "Test GPU", VramBytes: 1 << 30, UtilizationPercent: 7}},
		TelemetryValid: true,
	}
	if got := d.hardwareBlock(); !strings.Contains(got, "Test GPU") {
		t.Errorf("hardware block = %q, want the GPU name", got)
	}
}

// TestDetailIgnoresTelemetryForOtherNodes checks a late reply for a node the
// operator has navigated away from does not overwrite the current one.
func TestDetailIgnoresTelemetryForOtherNodes(t *testing.T) {
	d := newNodeDetail(nil, nodeRow{key: "current", self: true})
	d.update(nodeTelemetryMsg{
		nodeKey: "stale", gen: d.telemetryGen,
		telemetry: nodeTelemetry{TelemetryValid: true},
	})
	if d.telemetryOK {
		t.Error("accepted a reading addressed to a different node")
	}
}

func splitTestServer(t *testing.T, raw string) (string, int) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}
	return u.Hostname(), port
}
