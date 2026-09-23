// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"nvpair-shared/noderec"

	tea "github.com/charmbracelet/bubbletea"
)

// The node-info HTTP contract. Hardware telemetry is the one thing the broker's
// JSON-RPC surface does not carry: it keeps GPU, CPU, and memory readings
// internally but does not put them on the discovery wire, so a client that wants
// them has to ask each node directly. The desktop app does the same thing, and
// these values match its cadence so the two behave alike.
const (
	nodeInfoPath         = "/v1/node-info"
	nodeInfoDefaultPort  = 14318
	nodeInfoPollInterval = 2 * time.Second
	nodeInfoPollTimeout  = 1500 * time.Millisecond
	nodeInfoSelfHost     = "127.0.0.1"
)

// nodeTelemetry is one node-info reading. The GPU, CPU, and memory shapes are
// the canonical ones from nvpair-shared/noderec, which node-info's response uses
// field-for-field.
type nodeTelemetry struct {
	GPUs   []noderec.GPUInfo   `json:"GPUs"`
	CPU    *noderec.CPUInfo    `json:"cpu"`
	Memory *noderec.MemoryInfo `json:"memory"`
	// TelemetryValid reports whether the dynamic sample is usable at all; a node
	// with no GPU telemetry source still answers, with this false.
	TelemetryValid bool  `json:"telemetryValid"`
	MSSince        int64 `json:"msSince"`
}

// nodeTelemetryMsg carries a poll result. A failure is not surfaced as an error
// toast: an unreachable node is an ordinary, expected state here, and the detail
// screen simply says telemetry is unavailable.
type nodeTelemetryMsg struct {
	nodeKey string
	// gen identifies the polling chain this reading belongs to. It matters
	// because the reply is what schedules the next tick: a reading from a
	// previous visit to the same node would otherwise be accepted and start a
	// second chain alongside the current one, doubling the poll rate on every
	// close-and-reopen. Matching on the node alone is not enough — it is the
	// same node.
	gen       int
	telemetry nodeTelemetry
	err       error
}

// nodeTelemetryTickMsg schedules the next poll.
//
// gen identifies the polling chain that scheduled it. Bubble Tea has no way to
// cancel a pending tea.Tick, so closing and re-opening the same node's detail
// screen left the previous chain's tick in flight; matching on the node key
// alone accepted it, and each re-open added another self-sustaining chain
// polling the same endpoint.
type nodeTelemetryTickMsg struct {
	nodeKey string
	gen     int
}

// maxTelemetryBody bounds a node-info reply. The real payload is a few hundred
// bytes; this is generous while still refusing to buffer whatever a peer feels
// like sending. Without a bound, a hostile or broken node on the LAN could make
// this client allocate until it died — the request timeout limits how long a
// read takes, not how much it yields.
const maxTelemetryBody = 256 << 10 // 256 KiB

// telemetryClient is shared so connections are pooled across polls rather than
// opening a socket every two seconds.
//
// Redirects are refused. The address comes from mDNS or a typed manual entry, so
// following one would let a peer point this poll at an arbitrary host — loopback
// or a metadata endpoint included — and a node-info endpoint has no legitimate
// reason to redirect.
var telemetryClient = &http.Client{
	Timeout: nodeInfoPollTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// pollTelemetryCmd fetches one node's readings.
//
// Only the node whose detail screen is open is polled, unlike the desktop's
// sweep of every known node. The terminal shows one machine's detail at a time,
// so a sweep would put N requests on the network to render one panel — and this
// runs on the headless hosts least able to spare that.
// addresses is tried in the order the node ranked them, since a multi-homed
// node's first address may be a link only some peers can reach.
func pollTelemetryCmd(key string, gen int, addresses []string, port int) tea.Cmd {
	if len(addresses) == 0 {
		return nil
	}
	return func() tea.Msg {
		var lastErr error
		for _, address := range addresses {
			t, err := fetchTelemetry(nodeInfoURL(address, port))
			if err == nil {
				return nodeTelemetryMsg{nodeKey: key, gen: gen, telemetry: t}
			}
			lastErr = err
		}
		return nodeTelemetryMsg{nodeKey: key, gen: gen, err: lastErr}
	}
}

// fetchTelemetry reads one node-info endpoint.
func fetchTelemetry(url string) (nodeTelemetry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), nodeInfoPollTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nodeTelemetry{}, err
	}
	resp, err := telemetryClient.Do(req)
	if err != nil {
		return nodeTelemetry{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nodeTelemetry{}, fmt.Errorf("node-info returned %s", resp.Status)
	}
	var t nodeTelemetry
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTelemetryBody)).Decode(&t); err != nil {
		return nodeTelemetry{}, err
	}
	return t, nil
}

// telemetryTickCmd schedules the next poll for a node's polling chain.
func telemetryTickCmd(key string, gen int) tea.Cmd {
	return tea.Tick(nodeInfoPollInterval, func(time.Time) tea.Msg {
		return nodeTelemetryTickMsg{nodeKey: key, gen: gen}
	})
}

// nodeInfoURL builds the endpoint for a node. Discovery reports the node-info
// port per node; the constant is only the fallback for an entry that has none
// yet, such as a manual node whose first probe has not landed.
func nodeInfoURL(address string, port int) string {
	if port <= 0 {
		port = nodeInfoDefaultPort
	}
	return "http://" + net.JoinHostPort(address, strconv.Itoa(port)) + nodeInfoPath
}

// summary renders the readings as compact lines for the detail screen. Returns
// nil when the node reported nothing worth showing.
func (t nodeTelemetry) summary() []string {
	lines := make([]string, 0, len(t.GPUs)+2)
	for _, g := range t.GPUs {
		lines = append(lines, "  "+gpuLine(g, t.TelemetryValid))
	}
	if t.CPU != nil {
		name := t.CPU.Name
		if name == "" {
			name = "CPU"
		}
		detail := fmt.Sprintf("  %-28s %3d%%", truncate(name, 28), t.CPU.UtilizationPercent)
		if t.CPU.Cores > 0 {
			detail += fmt.Sprintf("  %d cores", t.CPU.Cores)
		}
		lines = append(lines, detail)
	}
	if t.Memory != nil && t.Memory.TotalBytes > 0 {
		lines = append(lines, fmt.Sprintf("  %-28s %s / %s", "RAM",
			humanBytes(t.Memory.UsedBytes), humanBytes(t.Memory.TotalBytes)))
	}
	return lines
}

// gpuLine renders one GPU. Utilization is omitted when the node reports its
// dynamic sample as unusable, rather than printing a zero that reads as idle.
func gpuLine(g noderec.GPUInfo, valid bool) string {
	name := g.Name
	if name == "" {
		name = "GPU"
	}
	util := "  --%"
	if valid {
		util = fmt.Sprintf("%3d%%", g.UtilizationPercent)
	}
	line := fmt.Sprintf("%-28s %s", truncate(name, 28), util)
	if g.VramBytes > 0 {
		line += fmt.Sprintf("  %s / %s", humanBytes(g.VramUsedBytes), humanBytes(g.VramBytes))
	}
	return line
}

// humanBytes renders a byte count in the largest unit that keeps it readable.
func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return strconv.FormatUint(b, 10) + " B"
	}
	value := float64(b)
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	idx := -1
	for value >= unit && idx < len(units)-1 {
		value /= unit
		idx++
	}
	if value >= 100 {
		return fmt.Sprintf("%.0f %s", value, units[idx])
	}
	return fmt.Sprintf("%.1f %s", value, units[idx])
}

// telemetryHosts is the addresses to poll, in preference order. This machine is
// reached over loopback: its own advertised address may be a link a peer uses to
// reach it rather than one it can usefully dial itself.
func telemetryHosts(node nodeRow) []string {
	if node.self {
		return []string{nodeInfoSelfHost}
	}
	if len(node.addresses) > 0 {
		return node.addresses
	}
	if a := strings.TrimSpace(node.address); a != "" {
		return []string{a}
	}
	return nil
}
