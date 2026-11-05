// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"nvpair-shared/noderec"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Custom section reads a node's custom/list answer - the endpoint
// configuration its owner maintains elsewhere - and the one port those
// endpoints are reached through. These tests pin what is renderable (keys stay
// key-blind, "down" is a probe fact while never-checked is not failure, the
// unavailable message survives verbatim), when asking is pointless (unpaired or
// offline peers, hand-added entries), that a port is only ever a node's own
// number and never this machine's default, and that the section never crowds the
// engine table or the status line off the frame. The port field itself is
// offered on this machine only, and only with a listener behind the number.

// customEndpointRows builds n endpoint rows with distinct hosts.
func customEndpointRows(n int) []customEndpointRow {
	rows := make([]customEndpointRow, n)
	for i := range rows {
		url := fmt.Sprintf("http://10.20.%d.%d:8000", i/4, 10+i%4)
		rows[i] = customEndpointRow{ID: "custom:" + url, BaseURL: url}
	}
	return rows
}

// lineFor returns the rendered line containing needle.
func lineFor(t *testing.T, lines []string, needle string) string {
	t.Helper()
	for _, line := range lines {
		if strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("no line contains %q: %q", needle, lines)
	return ""
}

func TestCustomSection_RendersNothingWithoutAnAnswer(t *testing.T) {
	d := localDetail()
	assert.Equal(t, 0, d.customHeight(), "no endpoints means no section, not a heading over nothing")
	assert.NotContains(t, d.View(), "Custom endpoints",
		"an unconfigured machine must not advertise a Custom section")
}

func TestCustomSection_ListsEndpointsKeyBlind(t *testing.T) {
	d := localDetail()
	d.custom = customSection{endpoints: []customEndpointRow{
		{Name: "lab", BaseURL: "http://10.0.0.5:8000", KeySet: true},
		{BaseURL: "http://10.0.0.6:8000", Healthy: true, LastChecked: "2026-10-29T00:00:00Z"},
		{BaseURL: "http://10.0.0.7:8000", LastChecked: "2026-10-29T00:00:00Z"},
		{BaseURL: "http://10.0.0.8:8000"},
	}}

	block := d.customBlock()
	require.Equal(t, 5, d.customHeight(), "heading plus one line per endpoint")
	lines := strings.Split(block, "\n")
	require.Len(t, lines, 5, "the render must measure exactly customHeight")
	assert.Contains(t, lines[0], "Custom endpoints", "unlabelled URLs read as a broken table")

	keyed := lineFor(t, lines, "10.0.0.5")
	assert.Contains(t, keyed, "lab", "an endpoint the operator named reads by that name")
	assert.Contains(t, keyed, "[key]",
		"key presence is shown so a keyed endpoint is distinguishable; the key itself never travels")

	neverChecked := lineFor(t, lines, "10.0.0.8")
	assert.NotContains(t, neverChecked, "down", "no probe result is not a failed probe")
	assert.NotContains(t, neverChecked, "[key]")

	assert.Equal(t, 1, strings.Count(block, "down"), "only the probed-down endpoint is marked down")
	assert.Equal(t, 1, strings.Count(block, "[key]"))

	assert.Contains(t, d.View(), "Custom endpoints", "the section sits between the hardware readout and the engines")
}

func TestCustomSection_ReportsUnavailabilityVerbatim(t *testing.T) {
	d := remoteDetail()
	d.node.membership = membershipMember
	d.custom = customSection{note: "custom endpoints unavailable: device is offline or does not support custom endpoint management"}

	require.Equal(t, 1, d.customHeight())
	assert.Contains(t, d.View(),
		"custom endpoints unavailable: device is offline or does not support custom endpoint management",
		"the unified unavailable message must reach the operator verbatim")
}

func TestCustomRead_OnlyAskedWhereItCanBeAnswered(t *testing.T) {
	self := localDetail()
	require.NotNil(t, self.customCmd(), "this machine's own config is always readable")

	peer := remoteDetail()
	peer.node.membership = membershipMember
	require.NotNil(t, peer.customCmd(), "a paired, online peer answers over the trusted channel")

	peer.node.presence = presenceOffline
	assert.Nil(t, peer.customCmd(),
		"a peer that is not there would hold the read for its full relay budget")

	unpaired := remoteDetail()
	unpaired.node.presence = presenceOnline
	assert.Nil(t, unpaired.customCmd(), "outside the cluster there is no channel to ask over")

	manual := newNodeDetail(nil, nodeRow{key: "manual:10.9.8.7", name: "lab", address: "10.9.8.7", presence: presenceOnline})
	manual.SetSize(100, 30)
	assert.Nil(t, manual.customCmd(), "a hand-added entry is not a cluster peer")
}

func TestCustomReadRequest_SendsNodeIdOnlyForPeers(t *testing.T) {
	method, params := customReadRequest("")
	assert.Equal(t, "custom/list", method)
	assert.Empty(t, params, "this machine's own config needs no addressing; the broker forks on nodeId")

	method, params = customReadRequest("peer-uuid")
	assert.Equal(t, "custom/list", method)
	assert.Equal(t, map[string]any{"nodeId": "peer-uuid"}, params,
		"nodeId is the custom-plane spelling; the `node` key belongs to the engine methods")
}

func TestCustomSection_TruncatesPastItsCap(t *testing.T) {
	d := localDetail()
	d.custom = customSection{endpoints: customEndpointRows(12)}

	block := d.customBlock()
	require.Equal(t, 6, d.customHeight(), "six rows is the ceiling; the engine table and the status line sit below")
	assert.Equal(t, 6, renderedRows(block), "the render must measure exactly customHeight")
	assert.Contains(t, block, "and 8 more", "hidden endpoints are counted, not silently dropped")
}

// TestDetail_BudgetHoldsUnderCustomAndGpus guards the frame math with the one
// block whose height comes from configuration (endpoints) next to the one
// whose height comes from the machine (GPUs). Both sit above the engine
// table, the models list, and the status line, and at the small end of that
// range the only correct outcome is that something gets dropped - the wrong
// outcome, which happened before this block was budgeted, is that the view
// emits rows the shell then truncates, deleting the status line at the
// bottom of the frame.
func TestDetail_BudgetHoldsUnderCustomAndGpus(t *testing.T) {
	for _, gpuCount := range []int{0, 1, 8} {
		gpus := make([]noderec.GPUInfo, gpuCount)
		for i := range gpus {
			gpus[i] = noderec.GPUInfo{Name: "NVIDIA GPU", VramBytes: 1 << 30}
		}
		for h := minTerminalHeight; h <= 30; h++ {
			budget := contentBudget(t, 80, h)
			if budget <= 0 {
				continue
			}
			d := newNodeDetail(nil, nodeRow{key: "peer", name: "peer", presence: presenceOnline, membership: membershipMember})
			d.engines = []engineStatus{
				{Engine: "ollama", Installed: true, Running: true},
				{Engine: "lmstudio", Installed: true},
			}
			if gpuCount > 0 {
				d.telemetryOK = true
				d.telemetry = nodeTelemetry{
					TelemetryValid: true, GPUs: gpus,
					CPU:    &noderec.CPUInfo{Name: "CPU", Cores: 8},
					Memory: &noderec.MemoryInfo{TotalBytes: 1 << 34, UsedBytes: 1 << 33},
				}
			}
			d.custom = customSection{endpoints: customEndpointRows(12)}
			d.SetSize(80, budget)
			d.refreshEngines()
			d.status.error("stop ollama failed: not running")

			out := d.View()
			assert.LessOrEqual(t, renderedRows(out), budget, "%d GPUs at 80x%d", gpuCount, h)
			assert.Contains(t, fitLines(out, budget), "stop ollama failed", "%d GPUs at 80x%d: the status line must stay on the frame", gpuCount, h)
		}
	}
}

// --- the facade's port ---

// customPortSection is a section holding a facade on the given port and n
// endpoints.
func customPortSection(port int, running bool, n int) customSection {
	return customSection{
		endpoints: customEndpointRows(n),
		port:      customProxyPort{Port: port, Running: running},
	}
}

func TestCustomSection_ListsTheFacadePortAboveTheEndpoints(t *testing.T) {
	d := localDetail()
	d.custom = customPortSection(8123, true, 1)

	block := d.customBlock()
	require.Equal(t, 3, d.customHeight(), "heading, the port, then the endpoint")
	require.Equal(t, 3, renderedRows(block), "the render must measure exactly customHeight")

	lines := strings.Split(block, "\n")
	assert.Contains(t, lines[1], "proxy port 8123",
		"one number fronts every endpoint, and it is the fact a client pointing at one needs")
	assert.NotContains(t, lines[1], "not listening",
		"a port with a listener behind it is where the endpoints are, not a plan")
}

func TestCustomSection_PlannedPortSaysNothingListensThere(t *testing.T) {
	d := localDetail()
	d.custom = customPortSection(8123, false, 1)

	assert.Contains(t, d.customBlock(), "proxy port 8123 (not listening)",
		"where a facade will land is not a place to point a client, and must not read like one")
}

func TestCustomSection_InventsNoPortWhenTheNodeNamedNone(t *testing.T) {
	d := remoteDetail()
	d.node.membership = membershipMember
	d.custom = customSection{endpoints: customEndpointRows(1)}

	require.Equal(t, 2, d.customHeight(), "heading plus the endpoint, and no port line")
	assert.NotContains(t, d.customBlock(), "proxy port",
		"this machine's stock 8000 is not a fact about the peer on screen")
}

func TestCustomSection_PortLineIsCountedAgainstTheRowCap(t *testing.T) {
	d := localDetail()
	d.custom = customPortSection(8000, true, 12)

	block := d.customBlock()
	require.Equal(t, 6, d.customHeight(), "six rows is still the ceiling with the port line in the section")
	require.Equal(t, 6, renderedRows(block), "the render must measure exactly customHeight")
	assert.Contains(t, block, "and 9 more",
		"the port line takes a row out of the endpoints, and the hidden count has to say so")
}

// TestCustomSection_KeepsWhatEachReadAnswered guards the two open-time reads.
// The endpoint list and the facade port go out in the same batch, so either can
// be answered second, and the screen holds one section: rebuilding it for one
// reply would delete whatever the other had already put there.
func TestCustomSection_KeepsWhatEachReadAnswered(t *testing.T) {
	portFirst := localDetail()
	portFirst.update(detailCustomPortMsg{port: customProxyPort{Port: 8123, Running: true}})
	portFirst.update(detailCustomMsg{endpoints: customEndpointRows(2)})
	require.Equal(t, 4, portFirst.customHeight(), "the port that landed first is still on the frame")
	assert.Contains(t, portFirst.customBlock(), "proxy port 8123")

	endpointsFirst := localDetail()
	endpointsFirst.update(detailCustomMsg{endpoints: customEndpointRows(2)})
	endpointsFirst.update(detailCustomPortMsg{port: customProxyPort{Port: 9111, Running: true}})
	require.Equal(t, 4, endpointsFirst.customHeight())
	assert.Contains(t, endpointsFirst.customBlock(), "proxy port 9111")

	// No port is not port 8000. A node that named none leaves the section without
	// a port line rather than with a number copied from this machine.
	answeredNothing := localDetail()
	answeredNothing.update(detailCustomMsg{endpoints: customEndpointRows(2)})
	answeredNothing.update(detailCustomPortMsg{})
	require.Equal(t, 3, answeredNothing.customHeight())
	assert.NotContains(t, answeredNothing.customBlock(), "proxy port")
}

// TestDetail_BudgetHoldsWithTheFacadePort repeats the block-math guard with the
// port line present and the port field open over it. The section is a row taller
// and the editor is another, both pushing at the same end of the frame as the
// status line they must not displace.
func TestDetail_BudgetHoldsWithTheFacadePort(t *testing.T) {
	for h := minTerminalHeight; h <= 30; h++ {
		budget := contentBudget(t, 80, h)
		if budget <= 0 {
			continue
		}
		d := localDetail()
		d.engines = []engineStatus{{Engine: "ollama", Installed: true, Running: true}}
		d.refreshEngines()
		d.custom = customPortSection(8123, true, 8)
		d.mode = detailInputCustomPort
		d.SetSize(80, budget)
		d.status.error("Custom proxy port failed: port 8123 is held by another proxy")

		out := d.View()
		assert.LessOrEqual(t, renderedRows(out), budget, "80x%d", h)
		assert.Contains(t, fitLines(out, budget), "held by another proxy",
			"80x%d: the refusal must stay on the frame", h)
	}
}

func TestCustomPortKey_OpensTheFieldForThisMachinesLiveFacade(t *testing.T) {
	d := localDetail()
	d.engines = []engineStatus{{Engine: "ollama", Installed: true, Running: true}}
	d.refreshEngines()
	d.custom = customPortSection(8123, true, 1)

	cmd := d.handleEngineKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	require.Equal(t, detailInputCustomPort, d.mode, "the custom port key opened no field")
	require.NotNil(t, cmd, "a focused field still blinks")
	assert.Equal(t, "8123", d.input.Value(), "the field starts from the port the node reports")
	assert.Contains(t, d.inputLabel(), "custom proxy port",
		"three engine proxy ports and one facade port on one screen must not read alike")
	assert.True(t, d.CapturingInput(), "the shell's own keys must not fire mid-port")
}

// TestCustomPortKey_RefusesWhereNothingCanBeRebound pins the three cases whose
// only possible answer is a refusal, and that each refusal reaches the status
// line: the key is offered in the engines pane for the selected row's ports, and
// a press meant for one of those deserves to say why it landed on Custom.
func TestCustomPortKey_RefusesWhereNothingCanBeRebound(t *testing.T) {
	pressC := func(d *nodeDetail) {
		d.engines = []engineStatus{{Engine: "ollama", Installed: true, Running: true}}
		d.refreshEngines()
		d.handleEngineKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	}

	peer := remoteDetail()
	peer.node.membership = membershipMember
	peer.custom = customPortSection(8123, true, 1)
	pressC(peer)
	require.Equal(t, detailInputNone, peer.mode, "a peer's configuration is not edited from here")
	assert.Contains(t, peer.status.render(), "only available on the machine running it")
	assert.Contains(t, peer.View(), "proxy port 8123",
		"refusing to edit it does not hide a peer's own port")

	// The owner refuses a write against no listener rather than remembering a plan
	// its proxy would ignore next time it comes up, so a field opened on one could
	// only ever come back saying no.
	notListening := localDetail()
	notListening.custom = customPortSection(8123, false, 1)
	pressC(notListening)
	require.Equal(t, detailInputNone, notListening.mode)
	assert.Contains(t, notListening.status.render(), "nothing is listening on port 8123")

	noPort := localDetail()
	noPort.custom = customPortSection(0, false, 1)
	pressC(noPort)
	require.Equal(t, detailInputNone, noPort.mode)
	assert.Contains(t, noPort.status.render(), "reported no Custom proxy port",
		"a field with no number to hold has nothing to edit")
}

func TestCustomPortHelp_IsAdvertisedOnlyWhereItOpens(t *testing.T) {
	helpKeys := func(d *nodeDetail) []string {
		keys := make([]string, 0, len(d.Help()))
		for _, b := range d.Help() {
			keys = append(keys, b.Help().Key)
		}
		return keys
	}

	live := localDetail()
	live.custom = customPortSection(8123, true, 1)
	require.True(t, live.customPortEditable())
	assert.Contains(t, helpKeys(live), "c",
		"a field that works here is a verb the footer has to mention")

	for _, tc := range []struct {
		name string
		d    *nodeDetail
	}{
		{"no listener", func() *nodeDetail {
			d := localDetail()
			d.custom = customPortSection(8123, false, 1)
			return d
		}()},
		{"no port named", func() *nodeDetail {
			d := localDetail()
			d.custom = customPortSection(0, false, 1)
			return d
		}()},
		{"a peer", func() *nodeDetail {
			d := remoteDetail()
			d.node.membership = membershipMember
			d.custom = customPortSection(8123, true, 1)
			return d
		}()},
	} {
		assert.False(t, tc.d.customPortEditable(), tc.name)
		assert.NotContains(t, helpKeys(tc.d), "c",
			tc.name+": the footer would advertise a key whose only answer is a refusal")
	}
}

func TestCustomPortSubmit_SendsTheTypedNumberWithoutASettingsRoundTrip(t *testing.T) {
	d := localDetail()
	d.custom = customPortSection(8000, true, 1)
	d.mode = detailInputCustomPort
	d.input.SetValue("9111")

	cmd := d.submitInput()
	require.NotNil(t, cmd, "a typed port has to reach the proxy that owns the listener")
	assert.Equal(t, detailInputNone, d.mode, "the field closes as the write goes out")
	assert.Contains(t, d.status.render(), "moving the Custom proxy to port 9111")

	bad := localDetail()
	bad.mode = detailInputCustomPort
	bad.input.SetValue("telepathy")
	assert.Nil(t, bad.submitInput(), "a non-port is not worth a round trip")
	assert.Contains(t, bad.status.render(), "invalid port")
}

// TestCustomPortOutcome_ReportsThePortTheNodeKept is the counterpart of the
// settings plane's lesson: the reply says what the proxy ended up with, and
// reading "no error" as "it moved" is how a lost bind race gets reported as done.
func TestCustomPortOutcome_ReportsThePortTheNodeKept(t *testing.T) {
	moved := localDetail()
	moved.applyCustomPort(customPortApplyMsg{port: customProxyPort{Port: 9111, Running: true}})
	assert.Contains(t, moved.status.render(), "Custom proxy port is now 9111")
	assert.Equal(t, 9111, moved.custom.port.Port, "the section shows where the facade landed")

	unbound := localDetail()
	unbound.applyCustomPort(customPortApplyMsg{port: customProxyPort{Port: 9111}})
	assert.Contains(t, unbound.status.render(), "nothing is listening on port 9111",
		"a port nothing bound is not a move that worked, and no client should be pointed at it")

	refused := localDetail()
	refused.applyCustomPort(customPortApplyMsg{
		err: errors.New("rpc error -32000: custom /v1/custom-endpoints/set-port: HTTP 409: port 53 is already in use by another proxy"),
	})
	assert.Contains(t, refused.status.render(), "port 53 is already in use by another proxy",
		"the owner's words name the port and who holds it; wrapping them buries that")
	assert.Zero(t, refused.custom.port.Port, "a refused write changes no number on screen")
}
