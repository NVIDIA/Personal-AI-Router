// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"strings"
	"testing"

	"nvpair-shared/noderec"
	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
)

func localDetail() *nodeDetail {
	d := newNodeDetail(nil, nodeRow{key: "self", name: "this-host", self: true, presence: presenceOnline})
	d.SetSize(100, 30)
	return d
}

func remoteDetail() *nodeDetail {
	d := newNodeDetail(nil, nodeRow{key: "peer", name: "peer-host", presence: presenceOnline})
	d.SetSize(100, 30)
	return d
}

// discoveryPush builds a discovery:nodes-changed notification.
func discoveryPush(nodes ...availableNode) NotificationMsg {
	params, _ := json.Marshal(nodes)
	return NotificationMsg{Msg: &rpc.Message{Method: "discovery:nodes-changed", Params: params}}
}

// TestRemoteDetailFollowsDiscoveryModels is the regression guard for a peer's
// detail screen freezing at the moment it was opened.
//
// A peer's model inventory arrives only in the enriched discovery snapshot —
// there is no per-node models RPC — and the screen seeded itself from the
// snapshot it was constructed with and then ignored every later one. Pulling a
// model onto that peer, including from this very screen, changed nothing on
// screen until the operator backed out and re-entered.
func TestRemoteDetailFollowsDiscoveryModels(t *testing.T) {
	d := remoteDetail()
	d.models = modelsResult{Models: []string{"old-model"}}
	d.refreshModels()

	d.update(discoveryPush(availableNode{
		HostUUID:       "peer",
		Name:           "peer-host",
		Models:         []string{"old-model", "new-model"},
		ModelsByEngine: map[string][]string{"ollama": {"old-model", "new-model"}},
	}))

	if len(d.models.Models) != 2 {
		t.Fatalf("models = %v, want the refreshed pair from discovery", d.models.Models)
	}
	if !contains(d.View(), "new-model") {
		t.Error("a model that appeared on the peer is not on screen")
	}
}

// TestRemoteDetailIgnoresOtherNodesDiscovery checks the screen only takes the
// entry for its own node, so a busy cluster cannot overwrite it.
func TestRemoteDetailIgnoresOtherNodesDiscovery(t *testing.T) {
	d := remoteDetail()
	d.models = modelsResult{Models: []string{"mine"}}
	d.refreshModels()

	d.update(discoveryPush(availableNode{
		HostUUID: "somebody-else",
		Name:     "other-host",
		Models:   []string{"theirs"},
	}))

	if len(d.models.Models) != 1 || d.models.Models[0] != "mine" {
		t.Errorf("another node's discovery entry overwrote this one: %v", d.models.Models)
	}
}

// TestLocalDetailIgnoresDiscoveryModels checks this machine keeps using the
// authoritative engine:models RPC rather than the discovery summary.
func TestLocalDetailIgnoresDiscoveryModels(t *testing.T) {
	d := localDetail()
	d.models = modelsResult{Models: []string{"authoritative"}}
	d.refreshModels()

	d.update(discoveryPush(availableNode{
		HostUUID: "self",
		Name:     "this-host",
		Models:   []string{"stale-summary"},
	}))

	if len(d.models.Models) != 1 || d.models.Models[0] != "authoritative" {
		t.Errorf("local detail took models from discovery: %v", d.models.Models)
	}
}

// TestRemoteDetailPollsEngines checks a peer's engine state is re-read on a
// tick. engine:state-changed carries a local snapshot with no node on it, so it
// cannot be attributed to a peer — polling is the only way an open remote
// screen notices an engine starting or stopping over there.
func TestRemoteDetailPollsEngines(t *testing.T) {
	d := remoteDetail()
	if cmd, _ := d.update(detailEnginesTickMsg{gen: d.telemetryGen}); cmd == nil {
		t.Error("remote detail did not re-read engines on its tick")
	}

	// A superseded screen's tick is dropped, matching the telemetry chain.
	if cmd, _ := d.update(detailEnginesTickMsg{gen: d.telemetryGen + 1}); cmd != nil {
		t.Error("a stale chain's tick was extended")
	}

	// This machine has real pushes, so it must not poll.
	local := localDetail()
	if cmd, _ := local.update(detailEnginesTickMsg{gen: local.telemetryGen}); cmd != nil {
		t.Error("local detail polls engines despite receiving engine:state-changed")
	}
}

// TestDetailSectionsAreSeparated is the guard for the two tables reading as one
// with a stray header in the middle: there must be a blank line between them.
func TestDetailSectionsAreSeparated(t *testing.T) {
	d := localDetail()
	d.engines = []engineStatus{{Engine: "ollama", Installed: true, Running: true, Port: 11434}}
	d.refreshEngines()

	lines := strings.Split(d.View(), "\n")
	modelsAt := -1
	for i, l := range lines {
		if strings.Contains(l, "Models") {
			modelsAt = i
			break
		}
	}
	if modelsAt <= 0 {
		t.Fatalf("no Models heading found in:\n%s", d.View())
	}
	if strings.TrimSpace(lines[modelsAt-1]) != "" {
		t.Errorf("no blank line before the Models heading; previous line was %q", lines[modelsAt-1])
	}
}

// TestLocalDetailShowsBothPorts is the guard for the two ports being managed in
// different places: on this machine they sit side by side on the engine's row.
func TestLocalDetailShowsBothPorts(t *testing.T) {
	d := localDetail()
	if d.proxy == nil {
		t.Fatal("no proxy tracker on the local node")
	}
	d.proxy.apply(proxyStatusMsg{idx: 0, ready: true, port: 11435})
	d.engines = []engineStatus{{Engine: "ollama", Installed: true, Running: true, Port: 11434}}
	d.refreshEngines()

	cols := detailEngineColumns(100, false)
	titles := make([]string, 0, len(cols))
	for _, c := range cols {
		titles = append(titles, c.Title)
	}
	joined := strings.Join(titles, " ")
	if !strings.Contains(joined, "ENGINE PORT") || !strings.Contains(joined, "PROXY PORT") {
		t.Fatalf("local engine columns = %q, want both ports", joined)
	}

	row := d.engineTable.Rows()[0]
	if row[4] != "11434" {
		t.Errorf("engine port cell = %q, want 11434", row[4])
	}
	if row[5] != "11435" {
		t.Errorf("proxy port cell = %q, want 11435", row[5])
	}
}

// TestRemoteDetailHidesProxyPort checks a peer's endpoints are not presented as
// ours to configure.
func TestRemoteDetailHidesProxyPort(t *testing.T) {
	d := remoteDetail()
	if d.proxy != nil {
		t.Error("a remote node should carry no proxy tracker")
	}
	for _, c := range detailEngineColumns(100, true) {
		if c.Title == "PROXY PORT" {
			t.Error("remote engine table offers a proxy port column")
		}
	}

	d.engines = []engineStatus{{Engine: "ollama", Port: 11434}}
	d.refreshEngines()
	if got := len(d.engineTable.Rows()[0]); got != 5 {
		t.Errorf("remote row has %d cells, want 5", got)
	}
}

// TestProxyPortCellStates checks the cell distinguishes a live endpoint from a
// port whose proxy is down, and from an engine no proxy fronts.
func TestProxyPortCellStates(t *testing.T) {
	d := localDetail()

	d.proxy.apply(proxyStatusMsg{idx: 0, ready: true, port: 11435})
	if got := d.proxyPortCell("ollama"); got != "11435" {
		t.Errorf("live endpoint = %q", got)
	}

	d.proxy.engines[0].ready = false
	if got := d.proxyPortCell("ollama"); !strings.Contains(got, "down") {
		t.Errorf("endpoint with the proxy down = %q, want it marked down", got)
	}

	if got := d.proxyPortCell("not-an-engine"); got != "-" {
		t.Errorf("engine with no proxy = %q, want %q", got, "-")
	}
}

// TestProxyIndexForEngine pins the engine-to-proxy pairing the proxy port
// column depends on.
func TestProxyIndexForEngine(t *testing.T) {
	p := newProxyTracker()
	if got := p.indexForEngine("ollama"); got != 0 {
		t.Errorf("ollama -> %d, want 0", got)
	}
	if got := p.indexForEngine("lmstudio"); got != 1 {
		t.Errorf("lmstudio -> %d, want 1", got)
	}
	// Case and padding must not decide whether a port renders.
	if got := p.indexForEngine("  OLLAMA "); got != 0 {
		t.Errorf("normalisation failed: %d", got)
	}
	if got := p.indexForEngine("vllm"); got != -1 {
		t.Errorf("unknown engine -> %d, want -1", got)
	}
}

// TestProxyPortKeyRejectedOnRemote checks the key explains itself rather than
// silently doing nothing on a peer.
func TestProxyPortKeyRejectedOnRemote(t *testing.T) {
	d := remoteDetail()
	d.engines = []engineStatus{{Engine: "ollama", Port: 11434}}
	d.refreshEngines()

	d.handleEngineKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if d.mode != detailInputNone {
		t.Error("opened a proxy port editor for a remote node")
	}
	if d.status.render() == "" {
		t.Error("no explanation for refusing the remote proxy port change")
	}
}

// TestPortEditorsAreDistinct checks the two port editors are told apart, so a
// typed value cannot be applied to the wrong one.
func TestPortEditorsAreDistinct(t *testing.T) {
	d := localDetail()
	d.proxy.apply(proxyStatusMsg{idx: 0, ready: true, port: 11435})
	d.engines = []engineStatus{{Engine: "ollama", Installed: true, Port: 11434}}
	d.refreshEngines()

	d.handleEngineKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if d.mode != detailInputEnginePort {
		t.Fatalf("e opened mode %v, want the engine port editor", d.mode)
	}
	if got := d.input.Value(); got != "11434" {
		t.Errorf("engine port editor seeded with %q, want the engine's own port", got)
	}
	if !strings.Contains(d.inputLabel(), "engine") {
		t.Errorf("label %q does not say which port", d.inputLabel())
	}

	d.mode = detailInputNone
	d.handleEngineKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	if d.mode != detailInputProxyPort {
		t.Fatalf("p opened mode %v, want the proxy port editor", d.mode)
	}
	if got := d.input.Value(); got != "11435" {
		t.Errorf("proxy port editor seeded with %q, want the proxy's port", got)
	}
	if !strings.Contains(d.inputLabel(), "proxy") {
		t.Errorf("label %q does not say which port", d.inputLabel())
	}
}

// TestNoBindingRequiresShift is the guard for the mixed-case keyboard: needing
// shift for some keys and not others makes every press a guess. Named keys like
// shift+tab are exempt; this is about letters.
func TestNoBindingRequiresShift(t *testing.T) {
	bindings := map[string][]key.Binding{
		"global":         globalBindings(),
		"nodes":          newNodesView(nil).Help(),
		"jobs":           newJobsView(nil).Help(),
		"service":        newServiceView(nil).Help(),
		"logs":           newLogsView(nil).Help(),
		"errors overlay": newErrorsView(nil).Help(),
		"catalog":        newCatalogBrowser(nil, "ollama", "Ollama", "this-host", false).Help(),
		"detail engines": localDetail().Help(),
	}

	models := localDetail()
	models.pane = detailModels
	bindings["detail models"] = models.Help()

	for where, set := range bindings {
		for _, b := range set {
			for _, k := range b.Keys() {
				// A single upper-case letter is the shift-dependent case; the
				// named keys (esc, enter, shift+tab) are longer than one rune.
				if len(k) == 1 && k >= "A" && k <= "Z" {
					t.Errorf("%s: binding %q uses shift-dependent key %q", where, b.Help().Desc, k)
				}
			}
		}
	}
}

// TestProxyPortOutcomeReportsTheBoundPort is the regression guard for a change
// that was refused and reported as done.
//
// A running engine outranks the proxy for a port, so the broker resolves the
// conflict and the proxy binds elsewhere; the reply carries the port actually
// bound. Reporting success on the absence of an RPC error meant asking for a
// port an engine held produced "proxy port updated" while the table went on
// showing the old one.
func TestProxyPortOutcomeReportsTheBoundPort(t *testing.T) {
	cases := []struct {
		name      string
		msg       proxyPortMsg
		wantKind  toastKind
		wantHas   []string
		wantNotIn []string
	}{
		{
			name:     "refused because the port is taken",
			msg:      proxyPortMsg{label: "LM Studio", requested: 1235, actual: 1234},
			wantKind: toastError,
			// Both numbers: which port it is on, and which one it could not have.
			wantHas:   []string{"LM Studio", "1234", "1235"},
			wantNotIn: []string{"updated"},
		},
		{
			name:     "honoured",
			msg:      proxyPortMsg{label: "Ollama", requested: 11500, actual: 11500},
			wantKind: toastOK,
			wantHas:  []string{"Ollama", "11500"},
		},
		{
			name:     "rpc failed",
			msg:      proxyPortMsg{label: "Ollama", requested: 80, err: errStub{}},
			wantKind: toastError,
			wantHas:  []string{"Ollama", "failed"},
		},
		{
			name: "reply carried no port",
			msg:  proxyPortMsg{label: "Ollama", requested: 11500},
			// Not an assertion of success: nothing confirmed the bind.
			wantKind:  toastInfo,
			wantNotIn: []string{"11500"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := localDetail()
			d.update(tc.msg)

			if d.status.kind != tc.wantKind {
				t.Errorf("toast kind = %v, want %v", d.status.kind, tc.wantKind)
			}
			got := d.status.render()
			for _, want := range tc.wantHas {
				if !strings.Contains(got, want) {
					t.Errorf("message %q does not mention %q", got, want)
				}
			}
			for _, unwanted := range tc.wantNotIn {
				if strings.Contains(got, unwanted) {
					t.Errorf("message %q should not contain %q", got, unwanted)
				}
			}
		})
	}
}

// TestEngineNameDoesNotDependOnTheEngineFetch is the regression guard for the
// same engine reading "Ollama" on one machine and "ollama" on another.
//
// A remote node's models come from discovery while its engine list comes from a
// separate call, so the models table routinely has rows while the engine list is
// still empty — the manager not running, the read failing, or a reply that
// genuinely lists nothing. Resolving the name only against that list meant the
// spelling depended on whether an unrelated fetch had landed.
func TestEngineNameDoesNotDependOnTheEngineFetch(t *testing.T) {
	withList := localDetail()
	withList.engines = []engineStatus{
		{Engine: "ollama", DisplayName: "Ollama"},
		{Engine: "lmstudio", DisplayName: "LM Studio"},
	}
	empty := localDetail() // no engine list at all

	for _, engine := range []string{"ollama", "lmstudio"} {
		want := withList.engineLabel(engine)
		if got := empty.engineLabel(engine); got != want {
			t.Errorf("engine %q reads %q with no engine list but %q with one",
				engine, got, want)
		}
		if want == engine {
			t.Errorf("engine %q resolved to its own wire id, not a display name", engine)
		}
	}

	// A genuinely unknown engine still has to render as something.
	if got := empty.engineLabel("some-new-engine"); got != "some-new-engine" {
		t.Errorf("unknown engine rendered %q, want the id passed through", got)
	}
}

// TestEmptyEngineListExplainsItself checks the two empty states read
// differently: the manager not answering, and the manager answering with
// nothing. They have different causes and the operator's next move differs.
func TestEmptyEngineListExplainsItself(t *testing.T) {
	local := localDetail()
	if got := local.emptyEnginesHint(); !strings.Contains(got, "engine manager") {
		t.Errorf("local hint does not point at the engine manager: %q", got)
	}
	remote := remoteDetail()
	if got := remote.emptyEnginesHint(); !strings.Contains(got, "this node") {
		t.Errorf("remote hint does not attribute the gap to the peer: %q", got)
	}
	if local.emptyEnginesHint() == remote.emptyEnginesHint() {
		t.Error("local and remote read identically; the causes are different")
	}
}

// TestNodeKeysDoNotCollide checks no two verbs on the Nodes tab claim the same
// key, and that none of them shadows a shell binding.
//
// A collision is silent: whichever case the switch reaches first wins and the
// other verb simply stops working, with the footer still advertising it. The
// risk is concentrated here because this tab has eleven verbs competing for one
// letter each, and they have been renamed more than once — pair moved from i to
// p and accept from p to a, which is exactly the edit that lands two verbs on
// one key if the whole set is not considered at once.
func TestNodeKeysDoNotCollide(t *testing.T) {
	nodeKeys := map[string]key.Binding{
		"details":         nodeDetailKey,
		"pair":            nodeInviteKey,
		"pair by address": nodeInviteAddrKey,
		"find by address": nodeAddKey,
		"remove":          nodeRemoveKey,
		"accept pairing":  nodePairKey,
		"decline":         nodeDeclineKey,
		"leave cluster":   nodeLeaveKey,
		"cancel invite":   nodeCancelKey,
		"filter":          nodeFilterKey,
		"confirm":         nodeConfirmKey,
	}

	// esc is deliberately excluded: nodeClearKey shares it with the universal
	// "get out of here" gesture, and both mean the same thing.
	seen := map[string]string{}
	for verb, binding := range nodeKeys {
		for _, k := range binding.Keys() {
			if other, dup := seen[k]; dup {
				t.Errorf("key %q is bound to both %q and %q", k, other, verb)
			}
			seen[k] = verb
		}
	}

	shell := newGlobalKeyMap(len(defaultViews(nil)))
	for _, g := range []key.Binding{shell.NextTab, shell.PrevTab, shell.JumpTab, shell.Help, shell.Quit} {
		for _, k := range g.Keys() {
			if verb, clash := seen[k]; clash {
				t.Errorf("node verb %q claims %q, which the shell uses for %q",
					verb, k, g.Help().Desc)
			}
		}
	}
}

// globalBindings is the shell's own key set, for the shift audit above, built
// for the real tab count so the digit binding matches what ships.
func globalBindings() []key.Binding {
	k := newGlobalKeyMap(len(defaultViews(nil)))
	return []key.Binding{k.NextTab, k.PrevTab, k.JumpTab, k.Help, k.Quit}
}

// TestDetailResizesWhenHardwareArrives is the regression guard for the status
// line falling off the frame. The model table is sized against the hardware
// block's height, and that height changes when a telemetry reading lands.
func TestDetailResizesWhenHardwareArrives(t *testing.T) {
	const budget = 20
	d := localDetail()
	d.engines = []engineStatus{{Engine: "ollama", Installed: true, Running: true}}
	models := make([]string, 30)
	for i := range models {
		models[i] = "model-" + string(rune('a'+i%26))
	}
	d.models = modelsResult{Models: models, ModelsByEngine: map[string][]string{"ollama": models}}
	d.SetSize(100, budget)
	d.refreshEngines()
	d.refreshModels()
	d.status.error("something to push off the bottom")

	if got := renderedRows(d.View()); got > budget {
		t.Fatalf("setup already overflows: %d rows into %d", got, budget)
	}

	// A dual-GPU reading is four hardware lines instead of the unavailable one.
	d.update(nodeTelemetryMsg{nodeKey: d.node.key, gen: d.telemetryGen, telemetry: nodeTelemetry{
		TelemetryValid: true,
		GPUs: []noderec.GPUInfo{
			{Name: "GPU 0", VramBytes: 1 << 30},
			{Name: "GPU 1", VramBytes: 1 << 30},
		},
		CPU:    &noderec.CPUInfo{Name: "CPU", Cores: 8},
		Memory: &noderec.MemoryInfo{TotalBytes: 1 << 34, UsedBytes: 1 << 33},
	}})

	if d.hardwareHeight() < 4 {
		t.Errorf("hardwareHeight = %d, want one row per reading", d.hardwareHeight())
	}
	// The frame is what matters, not any particular table's height. Asserting on
	// the height instead measured a value SetSize wrote and the renderer then
	// overwrote — so it passed whether or not the frame actually fit.
	if got := renderedRows(d.View()); got > budget {
		t.Errorf("rendered %d rows into %d after the hardware block grew; "+
			"the shell will delete the status line", got, budget)
	}
	if !contains(d.View(), "something to push off the bottom") {
		t.Error("the status line was squeezed out by the hardware block")
	}
}

// TestDetailResizesCatalogBrowser checks the browser follows a terminal resize
// rather than keeping the size it was opened at.
func TestDetailResizesCatalogBrowser(t *testing.T) {
	d := localDetail()
	d.engines = []engineStatus{{Engine: "ollama", Installed: true, Running: true}}
	d.refreshEngines()
	d.openCatalog()
	if d.catalog == nil {
		t.Fatal("catalog did not open")
	}

	d.SetSize(140, 40)
	if d.catalog.width != 140 || d.catalog.height != 40 {
		t.Errorf("catalog is %dx%d after resize, want 140x40", d.catalog.width, d.catalog.height)
	}
}

// TestModelSelectionSurvivesARebuild is the regression guard for a delete
// landing on the wrong model.
//
// The list is sorted and rebuilt wholesale whenever a download finishes or a
// peer republishes its inventory, so a row inserted above the cursor shifts
// everything below it. An earlier attempt at this captured the selection after
// the rebuild, which reads back whatever now sits at the old index — the very
// row the cursor slid onto — so it restored nothing.
func TestModelSelectionSurvivesARebuild(t *testing.T) {
	d := localDetail()
	d.models = modelsResult{
		ModelsByEngine: map[string][]string{"ollama": {"bravo", "charlie", "delta"}},
	}
	d.refreshModels()
	d.modelTable.SetCursor(1)
	if got := d.selectedModel(); got == nil || got.model != "charlie" {
		t.Fatalf("setup: selected %v", got)
	}

	// A background pull lands "alpha", which sorts first.
	d.models = modelsResult{
		ModelsByEngine: map[string][]string{"ollama": {"alpha", "bravo", "charlie", "delta"}},
	}
	d.refreshModels()

	got := d.selectedModel()
	if got == nil {
		t.Fatal("nothing selected after the rebuild")
	}
	if got.model != "charlie" {
		t.Errorf("selection slid to %q; a delete would now destroy the wrong model", got.model)
	}
}

// TestActionsWorkAfterAnEmptyRefresh is the regression guard for keys that went
// dead on the normal startup path.
//
// bubbles clamps an out-of-range cursor to len-1, so a table handed zero rows
// lands on -1 and stays there — refilling it never moves a cursor already below
// the range. A local node's inventory is empty until engine:models replies, so
// every model action reported "no model selected" for the life of the screen.
func TestActionsWorkAfterAnEmptyRefresh(t *testing.T) {
	d := localDetail() // constructed with no inventory, as at startup

	d.models = modelsResult{ModelsByEngine: map[string][]string{"ollama": {"a", "b"}}}
	d.refreshModels()
	if d.selectedModel() == nil {
		t.Errorf("no model selected after the inventory arrived (cursor %d); "+
			"load, eject, and delete are all dead", d.modelTable.Cursor())
	}

	// Same for the engines pane, which gates install/start/stop.
	d.engines = []engineStatus{{Engine: "ollama", Installed: true}}
	d.refreshEngines()
	if d.selectedEngine() == nil {
		t.Errorf("no engine selected after engines arrived (cursor %d)",
			d.engineTable.Cursor())
	}
}

// TestArmedActionOwnsTheKeyboard checks a pending confirmation cannot be
// escaped by a global key, which would leave it armed behind an off-screen
// prompt for whatever the operator pressed on returning.
func TestArmedActionOwnsTheKeyboard(t *testing.T) {
	d := localDetail()
	d.models = modelsResult{ModelsByEngine: map[string][]string{"ollama": {"victim"}}}
	d.refreshModels()
	d.pane = detailModels

	d.deleteSelectedModel()
	if d.pending == nil {
		t.Fatal("delete did not arm")
	}
	if !d.CapturingInput() {
		t.Error("an armed action does not capture input, so tab or a digit escapes it")
	}
	if !contains(d.status.render(), "press y to confirm") {
		t.Error("the confirmation prompt is not on screen")
	}
	// The prompt must not expire out from under the armed state.
	if d.status.expired() {
		t.Error("the confirmation prompt expires while the action stays armed")
	}
}

// TestArmedActionRunsOnConfirmAndNotOtherwise covers the half of the gate that
// was never tested: that `y` actually runs the captured action and that any
// other key abandons it. Arming was covered; confirming was not, on the one
// screen whose arming implementation the others were copied from.
func TestArmedActionRunsOnConfirmAndNotOtherwise(t *testing.T) {
	build := func() *nodeDetail {
		d := localDetail()
		d.models = modelsResult{ModelsByEngine: map[string][]string{"ollama": {"victim"}}}
		d.refreshModels()
		d.pane = detailModels
		d.deleteSelectedModel()
		return d
	}

	// Any other key cancels, and says so.
	d := build()
	if cmd := d.handleKeyForTest(t, "n"); cmd != nil {
		t.Error("a non-confirming key ran the destructive action")
	}
	if d.pending != nil {
		t.Error("the action stayed armed after being cancelled")
	}
	if !contains(d.status.render(), "cancelled") {
		t.Errorf("cancelling said nothing: %q", d.status.render())
	}

	// y runs it and disarms.
	d = build()
	if cmd := d.handleKeyForTest(t, "y"); cmd == nil {
		t.Error("confirming produced no command; the delete never ran")
	}
	if d.pending != nil {
		t.Error("the action stayed armed after being confirmed")
	}
}

// TestUninstallRefusedOnAPeerBeforeArming checks the refusal comes before the
// confirmation, not after it. Staging a gigabyte-destroying prompt and then
// answering that the operation is unavailable is worse than refusing outright.
func TestUninstallRefusedOnAPeerBeforeArming(t *testing.T) {
	d := remoteDetail()
	d.engines = []engineStatus{{Engine: "ollama", Installed: true}}
	d.refreshEngines()
	d.pane = detailEngines

	d.handleKeyForTest(t, "u")

	if d.pending != nil {
		t.Error("armed a confirmation for an uninstall that cannot run on a peer")
	}
	if !contains(d.status.render(), "machine running it") {
		t.Errorf("no explanation given: %q", d.status.render())
	}
}

// handleKeyForTest presses one key through the detail screen's key handling.
func (d *nodeDetail) handleKeyForTest(t *testing.T, k string) tea.Cmd {
	t.Helper()
	cmd, _ := d.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	return cmd
}

func TestParsePort(t *testing.T) {
	valid := map[string]int{"1": 1, "11434": 11434, "65535": 65535}
	for in, want := range valid {
		if got, ok := parsePort(in); !ok || got != want {
			t.Errorf("parsePort(%q) = %d, %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "0", "-1", "65536", "abc", "80x"} {
		if _, ok := parsePort(in); ok {
			t.Errorf("parsePort(%q) accepted an invalid port", in)
		}
	}
}

// TestServiceTabNoLongerOffersPorts checks the ports are configured in exactly
// one place, not two.
func TestServiceTabNoLongerOffersPorts(t *testing.T) {
	v := newServiceView(nil)
	for _, it := range v.items {
		if strings.Contains(strings.ToLower(it.label), "port") {
			t.Errorf("Service tab still offers %q; ports belong on the node", it.label)
		}
	}
}
