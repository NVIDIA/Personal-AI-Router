// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nvpair-shared/enginesettings"
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

// seedSettings puts a settings snapshot in the cache, as a fetch or a push
// would, so a test can press an edit key without a broker behind it.
func seedSettings(d *nodeDetail, snap enginesettings.Snapshot) {
	if d.settings == nil {
		d.settings = map[string]enginesettings.Snapshot{}
	}
	d.settings[snap.Engine] = snap
}

// ollamaSettings is an editable snapshot for the engine the detail tests use.
func ollamaSettings() enginesettings.Snapshot {
	return enginesettings.Snapshot{
		Engine:   "ollama",
		Revision: 7,
		Editable: true,
		Settings: enginesettings.Config{
			ServerPort: 11434,
			ProxyPort:  11435,
			LaunchText: "OLLAMA_KEEP_ALIVE=5m",
		},
	}
}

// TestSettingsKeysAskTheBackendBeforeOpening checks an edit key fetches the
// snapshot rather than opening a field against values this screen guessed.
//
// The revision is the point. A field opened without one has nothing to write
// against, and the backend's whole defence against two clients overwriting each
// other is that every write carries the revision it was based on.
func TestSettingsKeysAskTheBackendBeforeOpening(t *testing.T) {
	d := localDetail()
	d.engines = []engineStatus{{Engine: "ollama", Installed: true, Port: 11434}}
	d.refreshEngines()

	cmd := d.handleEngineKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("e")})
	if d.mode != detailInputNone {
		t.Errorf("opened a field before the snapshot arrived (mode %v)", d.mode)
	}
	if cmd == nil {
		t.Fatal("no settings request was issued")
	}
	if d.settingsWanted == nil || d.settingsWanted.mode != detailInputEnginePort {
		t.Fatalf("the requested edit was not remembered: %+v", d.settingsWanted)
	}
}

// TestSettingsEditorsAreDistinct checks the three fields are told apart, so a
// typed value cannot be applied to the wrong one.
func TestSettingsEditorsAreDistinct(t *testing.T) {
	d := localDetail()
	d.engines = []engineStatus{{Engine: "ollama", Installed: true, Port: 11434}}
	d.refreshEngines()
	seedSettings(d, ollamaSettings())

	cases := []struct {
		key   string
		mode  detailInputMode
		value string
		label string
	}{
		{"e", detailInputEnginePort, "11434", "engine"},
		{"p", detailInputProxyPort, "11435", "proxy"},
		{"a", detailInputLaunchArgs, "OLLAMA_KEEP_ALIVE=5m", "arguments"},
	}
	for _, tc := range cases {
		d.mode = detailInputNone
		d.handleEngineKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)})
		if d.mode != tc.mode {
			t.Fatalf("%q opened mode %v, want %v", tc.key, d.mode, tc.mode)
		}
		if got := d.input.Value(); got != tc.value {
			t.Errorf("%q seeded with %q, want %q", tc.key, got, tc.value)
		}
		if !strings.Contains(d.inputLabel(), tc.label) {
			t.Errorf("label %q does not say which field", d.inputLabel())
		}
	}
}

// TestSettingsRefusalComesFromTheBackend checks an engine the backend will not
// let us configure is refused in the backend's own words.
//
// This screen used to decide for itself, refusing every port change on a peer
// because the old RPC had no remote form. The settings path does, so the
// judgement belongs to the side that knows why — an adopted engine, an
// unreachable settings worker — rather than to a rule here that would drift.
func TestSettingsRefusalComesFromTheBackend(t *testing.T) {
	d := remoteDetail()
	d.engines = []engineStatus{{Engine: "ollama", Port: 11434}}
	d.refreshEngines()

	snap := ollamaSettings()
	snap.Editable = false
	snap.Adopted = true
	snap.Reason = "this engine was started outside PAIR"
	seedSettings(d, snap)

	d.handleEngineKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if d.mode != detailInputNone {
		t.Error("opened an editor for an engine the backend said is not editable")
	}
	if got := d.status.render(); !strings.Contains(got, "started outside PAIR") {
		t.Errorf("status %q does not carry the backend's reason", got)
	}
}

// TestSettingsWriteCarriesTheRevisionAndResolution checks the two things a
// settings write cannot be wrong about.
//
// The revision is what makes a concurrent edit fail instead of silently
// winning. The resolution decides which side gives way when the numeric port
// field and the port inside the command text disagree: editing the field
// should rewrite the command, and editing the command should move the field.
// Send the wrong one and the backend quietly rewrites what the operator typed.
func TestSettingsWriteCarriesTheRevisionAndResolution(t *testing.T) {
	d := localDetail()
	d.engines = []engineStatus{{Engine: "ollama", Installed: true, Port: 11434}}
	d.refreshEngines()
	snap := ollamaSettings()
	seedSettings(d, snap)

	cases := []struct {
		name       string
		mode       detailInputMode
		value      string
		resolution string
		check      func(enginesettings.Config) error
	}{
		{
			name:       "the port field wins over the command",
			mode:       detailInputEnginePort,
			value:      "11500",
			resolution: resolutionServer,
		},
		{
			name:       "the command wins over the port field",
			mode:       detailInputLaunchArgs,
			value:      "OLLAMA_HOST=127.0.0.1:11500",
			resolution: resolutionLaunch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, ok := d.settingsRequest(snap, tc.mode, tc.value)
			if !ok {
				t.Fatal("the request was rejected")
			}
			if req.ExpectedRevision != snap.Revision {
				t.Errorf("revision %d, want the snapshot's %d",
					req.ExpectedRevision, snap.Revision)
			}
			if req.Resolution != tc.resolution {
				t.Errorf("resolution %q, want %q", req.Resolution, tc.resolution)
			}
		})
	}
}

// TestLaunchTextIsSentAsTyped checks the arguments field is not tidied here.
//
// Whitespace is significant to a tokenizer, and the backend normalizes to rules
// this screen does not carry. Trimming on the way out would disagree with it
// and, worse, would disagree invisibly.
func TestLaunchTextIsSentAsTyped(t *testing.T) {
	d := localDetail()
	snap := ollamaSettings()
	const typed = `  OLLAMA_ORIGINS="https://example.com"  `

	req, ok := d.settingsRequest(snap, detailInputLaunchArgs, typed)
	if !ok {
		t.Fatal("the request was rejected")
	}
	if req.Settings.LaunchText != typed {
		t.Errorf("sent %q, want the text exactly as typed", req.Settings.LaunchText)
	}
}

// TestSettingsApplyUsesTheNormalizedDraft checks the commit sends what the
// backend validated, not what was typed.
//
// The preview returns normalized settings — quoting settled, ports reconciled
// — and applying the raw draft instead would save text that was never checked,
// with the preview's approval standing behind it.
func TestSettingsApplyUsesTheNormalizedDraft(t *testing.T) {
	d := localDetail()
	d.engines = []engineStatus{{Engine: "ollama", Installed: true}}
	d.refreshEngines()

	typed := enginesettings.Config{ServerPort: 11434, ProxyPort: 11435, LaunchText: "--flag   x"}
	normalized := enginesettings.Config{ServerPort: 11434, ProxyPort: 11435, LaunchText: "--flag x"}

	verdict, sent, problem := judgeSettingsPreview(enginePreviewMsg{
		request: enginesettings.Request{
			Engine:           "ollama",
			ExpectedRevision: 7,
			Settings:         typed,
			Resolution:       resolutionLaunch,
		},
		preview: enginesettings.Preview{Settings: normalized},
	})

	if verdict != settingsWrite {
		t.Fatalf("a clean preview did not write (verdict %v, problem %q)", verdict, problem)
	}
	if sent.Settings.LaunchText != normalized.LaunchText {
		t.Errorf("applied %q, want the normalized %q",
			sent.Settings.LaunchText, normalized.LaunchText)
	}
	if sent.Resolution != "" {
		t.Errorf("resolution %q survived into the commit; the draft is already settled",
			sent.Resolution)
	}
	if sent.ExpectedRevision != 7 {
		t.Errorf("revision %d, want the one the draft was based on", sent.ExpectedRevision)
	}
	_ = d
}

// settingsPush builds an engine:settings-changed notification.
func settingsPush(snap enginesettings.Snapshot) NotificationMsg {
	params, _ := json.Marshal(snap)
	return NotificationMsg{Msg: &rpc.Message{Method: "engine:settings-changed", Params: params}}
}

// TestLocalSettingsPushIsNotDiscarded is the regression guard for a second
// save that could never succeed.
//
// The broker stamps every snapshot with this node's UUID. Matching that
// against the node argument the local RPCs take — which is empty, precisely
// because they are local — discarded every push for this machine. The cached
// revision then stayed at whatever the first read returned, so the save after
// a successful one was rejected as stale, and stayed rejected until the screen
// was closed and reopened.
func TestLocalSettingsPushIsNotDiscarded(t *testing.T) {
	const uuid = "33983c39-c0a6-41d2-9488-455b5e61e25f"
	d := newNodeDetail(nil, nodeRow{key: uuid, name: "this-host", self: true, presence: presenceOnline})
	d.SetSize(100, 30)
	seedSettings(d, ollamaSettings()) // revision 7

	moved := ollamaSettings()
	moved.NodeID = uuid
	moved.Revision = 8
	d.handleNotification(settingsPush(moved).Msg)

	if got := d.settings["ollama"].Revision; got != 8 {
		t.Fatalf("cached revision is %d after a push for this machine, want 8", got)
	}

	// And a push for a different machine is still ignored.
	other := ollamaSettings()
	other.NodeID = "some-other-node"
	other.Revision = 99
	d.handleNotification(settingsPush(other).Msg)
	if got := d.settings["ollama"].Revision; got != 8 {
		t.Errorf("a peer's snapshot overwrote this machine's: revision %d", got)
	}
}

// TestFailedSaveReloadsTheSnapshot checks a rejected write leaves the screen
// able to try again.
//
// A revision the backend will not accept is not recoverable by repeating the
// same write: without dropping it, every later attempt fails identically and
// the only way out is to leave the screen.
func TestFailedSaveReloadsTheSnapshot(t *testing.T) {
	d := localDetail()
	d.engines = []engineStatus{{Engine: "ollama", Installed: true}}
	d.refreshEngines()
	seedSettings(d, ollamaSettings())
	d.settingsAwaited = "ollama"

	cmd, _ := d.update(engineSettingsAppliedMsg{
		engine: "ollama",
		err:    errors.New("settings changed on this device; reload before applying"),
	})

	if _, still := d.settings["ollama"]; still {
		t.Error("the rejected snapshot is still cached, so the next attempt repeats the failure")
	}
	if cmd == nil {
		t.Error("nothing re-read the settings, so the next edit has nothing to write against")
	}
	if got := d.status.render(); !strings.Contains(got, "try again") {
		t.Errorf("status %q does not tell the operator what to do: %q", got, "try again")
	}
}

// TestSettingsCommitCarriesARequestIdentifier is the regression guard for a
// save that failed after the check had passed.
//
// The identifier is the backend's idempotency key: it records a receipt against
// it, returns the original outcome when one is replayed, and refuses a replay
// carrying different settings. A commit without one is rejected outright — and
// because only the commit needs it, the preview succeeded first, so the
// interface reported the settings as valid and then refused to save them.
func TestSettingsCommitCarriesARequestIdentifier(t *testing.T) {
	previewOf := func(port int) enginePreviewMsg {
		return enginePreviewMsg{
			request: enginesettings.Request{Engine: "ollama", ExpectedRevision: 7},
			preview: enginesettings.Preview{
				Settings: enginesettings.Config{ServerPort: port, ProxyPort: 11434},
			},
		}
	}

	_, first, _ := judgeSettingsPreview(previewOf(11500))
	if first.RequestID == "" {
		t.Fatal("the commit carries no request identifier; the backend will refuse it")
	}
	if len(first.RequestID) > 128 {
		t.Errorf("identifier is %d characters; the broker allows 128", len(first.RequestID))
	}

	// Asserted on the wire form, not the Go field. `requestId` is omitempty, so
	// an unset one does not travel as an empty string — it disappears from the
	// object altogether, which is precisely how this shipped: the struct had
	// the field, the JSON did not, and the broker refused the call.
	wire, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(wire), `"requestId"`) {
		t.Errorf("the request sent to the broker has no requestId: %s", wire)
	}

	// A different change must not reuse it: the backend refuses an identifier
	// replayed with settings that do not match its receipt.
	_, second, _ := judgeSettingsPreview(previewOf(11501))
	if second.RequestID == first.RequestID {
		t.Error("two different changes share one identifier; the second would be refused")
	}
}

// TestSettingsRestartIsConfirmed checks a change that restarts the engine asks
// first, and that the confirmation applies the same request it armed.
func TestSettingsRestartIsConfirmed(t *testing.T) {
	d := localDetail()
	d.engines = []engineStatus{{Engine: "ollama", Installed: true}}
	d.refreshEngines()

	normalized := enginesettings.Config{ServerPort: 11500, ProxyPort: 11435}
	restarting := enginePreviewMsg{
		request: enginesettings.Request{Engine: "ollama", ExpectedRevision: 7},
		preview: enginesettings.Preview{Settings: normalized, Restart: true},
	}

	if verdict, _, _ := judgeSettingsPreview(restarting); verdict != settingsConfirmFirst {
		t.Fatalf("a restarting change was not held for confirmation (verdict %v)", verdict)
	}

	if cmd := d.applySettingsPreview(restarting); cmd != nil {
		t.Fatal("a restarting change was sent without asking")
	}
	if d.settingsConfirm == nil {
		t.Fatal("no confirmation was armed")
	}
	if got := d.status.render(); !strings.Contains(got, "restart") {
		t.Errorf("prompt %q does not say the engine will restart", got)
	}

	// Anything other than y walks away, and the armed request goes with it.
	if cmd := d.resolveSettingsConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}); cmd != nil {
		t.Error("a non-confirming key still applied the change")
	}
	if d.settingsConfirm != nil {
		t.Error("the armed request outlived the cancellation")
	}

	d.applySettingsPreview(restarting)
	armed := d.settingsConfirm
	if armed == nil || armed.Settings.ServerPort != normalized.ServerPort {
		t.Fatalf("armed the wrong request: %+v", armed)
	}
	// The identifier is minted when the change is judged, not when it is sent,
	// so confirming is a replay of the arming rather than a second write.
	if armed.RequestID == "" {
		t.Error("the armed request has no identifier, so confirming it would be refused")
	}
	if cmd := d.resolveSettingsConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}); cmd == nil {
		t.Error("y did not apply the armed change")
	}
}

// TestSettingsPreviewFailuresAreExplained checks a rejected draft says why and
// saves nothing.
func TestSettingsPreviewFailuresAreExplained(t *testing.T) {
	cases := []struct {
		name    string
		preview enginesettings.Preview
		err     error
		want    string
	}{
		{
			name:    "a field the backend rejected",
			preview: enginesettings.Preview{Errors: map[string]string{"launchText": "unbalanced quote"}},
			want:    "unbalanced quote",
		},
		{
			name:    "the two ports disagree",
			preview: enginesettings.Preview{Conflict: &enginesettings.Conflict{ServerPort: 11434, LaunchPort: 11500}},
			want:    "11500",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := localDetail()
			d.engines = []engineStatus{{Engine: "ollama", Installed: true}}
			d.refreshEngines()

			cmd := d.applySettingsPreview(enginePreviewMsg{
				request: enginesettings.Request{Engine: "ollama"},
				preview: tc.preview,
				err:     tc.err,
			})
			if cmd != nil {
				t.Error("a rejected draft was sent anyway")
			}
			if d.settingsConfirm != nil {
				t.Error("a rejected draft was armed for confirmation")
			}
			if got := d.status.render(); !strings.Contains(got, tc.want) {
				t.Errorf("status %q does not mention %q", got, tc.want)
			}
		})
	}
}

// TestSettingsErrorsReadTheSameEveryTime checks the per-field errors are
// ordered, since a map would reshuffle the same failure between attempts.
func TestSettingsErrorsReadTheSameEveryTime(t *testing.T) {
	errs := map[string]string{
		"serverPort": "port in use",
		"launchText": "unbalanced quote",
		"proxyPort":  "port in use",
	}
	first := joinSettingsErrors(errs)
	for i := 0; i < 20; i++ {
		if got := joinSettingsErrors(errs); got != first {
			t.Fatalf("attempt %d rendered %q, want the stable %q", i, got, first)
		}
	}
	if !strings.Contains(first, "unbalanced quote") {
		t.Errorf("rendered %q, want every field's message", first)
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

// TestSettingsOutcomeReportsTheBoundPort is the regression guard for a change
// that was refused and reported as done.
//
// A running engine outranks the proxy for a port, so the backend binds
// elsewhere and reports the difference as the effective port. Treating the
// absence of an error as success meant asking for a port an engine held
// produced "updated" while the table went on showing the old one.
func TestSettingsOutcomeReportsTheBoundPort(t *testing.T) {
	cases := []struct {
		name      string
		snapshot  enginesettings.Snapshot
		wantKind  toastKind
		wantHas   []string
		wantNotIn []string
	}{
		{
			// The proxy's port is read live from the proxy process, not
			// observed in passing, so a difference here is real.
			name: "the endpoint could not take the port",
			snapshot: enginesettings.Snapshot{
				Engine:              "lmstudio",
				Settings:            enginesettings.Config{ProxyPort: 1235, ServerPort: 1236},
				EffectiveProxyPort:  1234,
				EffectiveServerPort: 1236,
			},
			wantKind: toastError,
			// Both numbers: which port it is on, and which one it could not have.
			wantHas:   []string{"1234", "1235"},
			wantNotIn: []string{"saved"},
		},
		{
			// The backend refused it, and says why. Its words, not a guess
			// assembled from the ports.
			name: "the backend refused the change",
			snapshot: enginesettings.Snapshot{
				Engine:   "ollama",
				Phase:    settingsPhaseFailed,
				Error:    "port 11500 is reserved by another service",
				Settings: enginesettings.Config{ProxyPort: 11434, ServerPort: 11500},
			},
			wantKind:  toastError,
			wantHas:   []string{"reserved by another service"},
			wantNotIn: []string{"saved"},
		},
		{
			// The engine's effective port is observed when the apply replies,
			// and an engine that restarts onto the new port finishes after
			// that -- LM Studio's server re-launches detached. Reading failure
			// into the lag reported a move that had happened as one that had
			// not, naming a port nothing was listening on.
			name: "the engine port reading lags a successful move",
			snapshot: enginesettings.Snapshot{
				Engine:              "ollama",
				Settings:            enginesettings.Config{ProxyPort: 11434, ServerPort: 11500},
				EffectiveProxyPort:  11434,
				EffectiveServerPort: 11435,
			},
			wantKind:  toastOK,
			wantHas:   []string{"saved"},
			wantNotIn: []string{"11435"},
		},
		{
			name: "honoured",
			snapshot: enginesettings.Snapshot{
				Engine:              "ollama",
				Settings:            enginesettings.Config{ProxyPort: 11434, ServerPort: 11500},
				EffectiveProxyPort:  11434,
				EffectiveServerPort: 11500,
			},
			wantKind: toastOK,
			wantHas:  []string{"saved"},
		},
		{
			name: "nothing bound yet",
			snapshot: enginesettings.Snapshot{
				Engine:   "ollama",
				Settings: enginesettings.Config{ProxyPort: 11434, ServerPort: 11500},
			},
			// A stopped engine has no port in force. Saying it "stayed on :0"
			// would be worse than not saying where it landed.
			wantKind:  toastOK,
			wantNotIn: []string{":0"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := localDetail()
			d.settingsAwaited = tc.snapshot.Engine
			d.reportSettingsOutcome(tc.snapshot)

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

// TestSettingsOutcomeIsSilentForSomeoneElsesChange checks the report is scoped
// to a change this screen made.
//
// These snapshots also arrive whenever the desktop app or another operator
// saves something, and a note on each would be noise about work the person at
// this terminal did not do.
func TestSettingsOutcomeIsSilentForSomeoneElsesChange(t *testing.T) {
	d := localDetail()
	snap := enginesettings.Snapshot{
		Engine:             "ollama",
		Settings:           enginesettings.Config{ProxyPort: 11434},
		EffectiveProxyPort: 11999,
	}

	d.reportSettingsOutcome(snap)
	if got := d.status.render(); got != "" {
		t.Errorf("reported %q for a change this screen did not make", got)
	}

	// And it speaks exactly once for a change it did make.
	d.settingsAwaited = "ollama"
	d.reportSettingsOutcome(snap)
	if d.status.render() == "" {
		t.Fatal("said nothing about this screen's own change")
	}
	d.status = toast{}
	d.reportSettingsOutcome(snap)
	if got := d.status.render(); got != "" {
		t.Errorf("repeated the outcome as %q on a later snapshot", got)
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
