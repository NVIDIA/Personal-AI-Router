// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"nvpair-shared/enginesettings"
	"nvpair-shared/noderec"
	"nvpair-tui/rpc"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	require.Len(t, d.models.Models, 2, "refreshed pair from discovery")
	assert.Contains(t, d.View(), "new-model", "a model that appeared on the peer is not on screen")
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

	assert.Equal(t, []string{"mine"}, d.models.Models, "another node's discovery entry overwrote this one")
}

// TestUnreportedLoadStateIsUnknown is the regression guard for "not loaded"
// shown for a model whose engine cannot say. An engine with no loaded endpoint
// is left out of loadedByEngine, and reading that as an empty list marked every
// one of its models as not in memory.
func TestUnreportedLoadStateIsUnknown(t *testing.T) {
	d := localDetail()
	d.models = modelsResult{
		ModelsByEngine: map[string][]string{
			"ollama":   {"idle", "resident"},
			"llamacpp": {"unreported"},
		},
		LoadedByEngine: map[string][]string{"ollama": {"resident"}},
	}
	d.refreshModels()

	got := map[string]string{}
	for _, row := range d.modelTable.Rows() {
		got[row[0]] = row[2]
	}
	want := map[string]string{"idle": "no", "resident": "yes", "unreported": "?"}
	for model, loaded := range want {
		assert.Equal(t, loaded, got[model], "%s loaded state", model)
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

	assert.Equal(t, []string{"authoritative"}, d.models.Models, "local detail took models from discovery")
}

// TestRemoteDetailPollsEngines checks a peer's engine state is re-read on a
// tick. engine:state-changed carries a local snapshot with no node on it, so it
// cannot be attributed to a peer — polling is the only way an open remote
// screen notices an engine starting or stopping over there.
func TestRemoteDetailPollsEngines(t *testing.T) {
	d := remoteDetail()
	cmd, _ := d.update(detailEnginesTickMsg{gen: d.telemetryGen})
	assert.NotNil(t, cmd, "remote detail did not re-read engines on its tick")

	// A superseded screen's tick is dropped, matching the telemetry chain.
	cmd, _ = d.update(detailEnginesTickMsg{gen: d.telemetryGen + 1})
	assert.Nil(t, cmd, "a stale chain's tick was extended")

	// This machine has real pushes, so it must not poll.
	local := localDetail()
	cmd, _ = local.update(detailEnginesTickMsg{gen: local.telemetryGen})
	assert.Nil(t, cmd, "local detail polls engines despite receiving engine:state-changed")
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
	require.Greater(t, modelsAt, 0, "Models heading: %s", strings.Join(lines, "\n"))
	assert.Empty(t, strings.TrimSpace(lines[modelsAt-1]), "blank line before the Models heading")
}

// TestLocalDetailShowsBothPorts is the guard for the two ports being managed in
// different places: on this machine they sit side by side on the engine's row.
func TestLocalDetailShowsBothPorts(t *testing.T) {
	d := localDetail()
	require.NotNil(t, d.proxy, "no proxy tracker on the local node")
	d.proxy.apply(proxyStatusMsg{idx: 0, ready: true, port: 11435})
	d.engines = []engineStatus{{Engine: "ollama", Installed: true, Running: true, Port: 11434}}
	d.refreshEngines()

	cols := detailEngineColumns(100, false)
	titles := make([]string, 0, len(cols))
	for _, c := range cols {
		titles = append(titles, c.Title)
	}
	joined := strings.Join(titles, " ")
	require.Contains(t, joined, "ENGINE PORT", "local engine columns show both ports")
	require.Contains(t, joined, "PROXY PORT", "local engine columns show both ports")

	row := d.engineTable.Rows()[0]
	assert.Equal(t, "11434", row[4], "engine port cell")
	assert.Equal(t, "11435", row[5], "proxy port cell")
}

// TestRemoteDetailHidesProxyPort checks a peer's endpoints are not presented as
// ours to configure.
func TestRemoteDetailHidesProxyPort(t *testing.T) {
	d := remoteDetail()
	assert.Nil(t, d.proxy, "a remote node should carry no proxy tracker")
	for _, c := range detailEngineColumns(100, true) {
		assert.NotEqual(t, "PROXY PORT", c.Title, "remote engine table offers a proxy port column")
	}

	d.engines = []engineStatus{{Engine: "ollama", Port: 11434}}
	d.refreshEngines()
	assert.Len(t, d.engineTable.Rows()[0], 5, "remote row cells")
}

// TestProxyPortCellStates checks the cell distinguishes a live endpoint from a
// port whose proxy is down, and from an engine no proxy fronts.
func TestProxyPortCellStates(t *testing.T) {
	d := localDetail()

	d.proxy.apply(proxyStatusMsg{idx: 0, ready: true, port: 11435})
	assert.Equal(t, "11435", d.proxyPortCell("ollama"), "live endpoint")

	d.proxy.engines[0].ready = false
	assert.Contains(t, d.proxyPortCell("ollama"), "down", "endpoint with the proxy down")

	assert.Equal(t, "-", d.proxyPortCell("not-an-engine"), "engine with no proxy")
}

// TestProxyIndexForEngine pins the engine-to-proxy pairing the proxy port
// column depends on.
func TestProxyIndexForEngine(t *testing.T) {
	p := newProxyTracker()
	assert.Equal(t, 0, p.indexForEngine("ollama"))
	assert.Equal(t, 1, p.indexForEngine("lmstudio"))
	// Case and padding must not decide whether a port renders.
	assert.Equal(t, 0, p.indexForEngine("  OLLAMA "), "normalisation")
	assert.Equal(t, -1, p.indexForEngine("vllm"), "unknown engine")
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
	assert.Equal(t, detailInputNone, d.mode, "opened a field before the snapshot arrived")
	require.NotNil(t, cmd, "no settings request was issued")
	require.NotNil(t, d.settingsWanted, "the requested edit was not remembered")
	require.Equal(t, detailInputEnginePort, d.settingsWanted.mode, "the requested edit was not remembered")
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
		require.Equal(t, tc.mode, d.mode, "%q editor mode", tc.key)
		assert.Equal(t, tc.value, d.input.Value(), "%q initial value", tc.key)
		assert.Contains(t, d.inputLabel(), tc.label, "label identifies field")
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
	assert.Equal(t, detailInputNone, d.mode, "opened an editor for an engine the backend said is not editable")
	assert.Contains(t, d.status.render(), "started outside PAIR", "status carries the backend's reason")
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
			require.True(t, ok, "the request was rejected")
			assert.Equal(t, snap.Revision, req.ExpectedRevision, "snapshot revision")
			assert.Equal(t, tc.resolution, req.Resolution)
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
	require.True(t, ok, "the request was rejected")
	assert.Equal(t, typed, req.Settings.LaunchText, "text exactly as typed")
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

	require.Equal(t, settingsWrite, verdict, "a clean preview writes; problem %q", problem)
	assert.Equal(t, normalized.LaunchText, sent.Settings.LaunchText, "apply normalized draft")
	assert.Empty(t, sent.Resolution, "resolution survived into the commit; the draft is already settled")
	assert.Equal(t, uint64(7), sent.ExpectedRevision, "revision the draft was based on")
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

	require.Equal(t, uint64(8), d.settings["ollama"].Revision, "cached revision after a push for this machine")

	// And a push for a different machine is still ignored.
	other := ollamaSettings()
	other.NodeID = "some-other-node"
	other.Revision = 99
	d.handleNotification(settingsPush(other).Msg)
	assert.Equal(t, uint64(8), d.settings["ollama"].Revision, "a peer's snapshot overwrote this machine's")
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
	d.settingsAwaited = &awaitedSettings{engine: "ollama", requestID: "req-1"}

	cmd, _ := d.update(engineSettingsAppliedMsg{
		engine: "ollama",
		err:    errors.New("settings changed on this device; reload before applying"),
	})

	assert.NotContains(t, d.settings, "ollama", "the rejected snapshot is still cached, so the next attempt repeats the failure")
	assert.NotNil(t, cmd, "nothing re-read the settings, so the next edit has nothing to write against")
	assert.Contains(t, d.status.render(), "try again", "status tells the operator what to do")
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
	require.NotEmpty(t, first.RequestID, "the commit carries no request identifier; the backend will refuse it")
	assert.LessOrEqual(t, len(first.RequestID), 128, "broker's identifier limit")

	// Asserted on the wire form, not the Go field. `requestId` is omitempty, so
	// an unset one does not travel as an empty string — it disappears from the
	// object altogether, which is precisely how this shipped: the struct had
	// the field, the JSON did not, and the broker refused the call.
	wire, err := json.Marshal(first)
	require.NoError(t, err, "marshal")
	assert.Contains(t, string(wire), `"requestId"`, "request sent to the broker has requestId")

	// A different change must not reuse it: the backend refuses an identifier
	// replayed with settings that do not match its receipt.
	_, second, _ := judgeSettingsPreview(previewOf(11501))
	assert.NotEqual(t, first.RequestID, second.RequestID, "two different changes share one identifier; the second would be refused")
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

	verdict, _, _ := judgeSettingsPreview(restarting)
	require.Equal(t, settingsConfirmFirst, verdict, "a restarting change was not held for confirmation")

	require.Nil(t, d.applySettingsPreview(restarting), "a restarting change was sent without asking")
	require.NotNil(t, d.settingsConfirm, "no confirmation was armed")
	assert.Contains(t, d.status.render(), "restart", "prompt says the engine will restart")

	// Anything other than y walks away, and the armed request goes with it.
	assert.Nil(t, d.resolveSettingsConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}), "a non-confirming key still applied the change")
	assert.Nil(t, d.settingsConfirm, "the armed request outlived the cancellation")

	d.applySettingsPreview(restarting)
	armed := d.settingsConfirm
	require.NotNil(t, armed, "armed the wrong request")
	require.Equal(t, normalized.ServerPort, armed.Settings.ServerPort, "armed the wrong request")
	// The identifier is minted when the change is judged, not when it is sent,
	// so confirming is a replay of the arming rather than a second write.
	assert.NotEmpty(t, armed.RequestID, "the armed request has no identifier, so confirming it would be refused")
	assert.NotNil(t, d.resolveSettingsConfirm(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}), "y did not apply the armed change")
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
			assert.Nil(t, cmd, "a rejected draft was sent anyway")
			assert.Nil(t, d.settingsConfirm, "a rejected draft was armed for confirmation")
			assert.Contains(t, d.status.render(), tc.want)
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
		require.Equal(t, first, joinSettingsErrors(errs), "attempt %d stays stable", i)
	}
	assert.Contains(t, first, "unbalanced quote", "every field's message")
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
					assert.Failf(t, "binding uses a shift-dependent key", "%s: binding %q, key %q", where, b.Help().Desc, k)
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
				Phase:               settingsPhaseSucceeded,
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
				Phase:               settingsPhaseSucceeded,
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
				Phase:               settingsPhaseSucceeded,
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
				Phase:    settingsPhaseSucceeded,
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
			snap := tc.snapshot
			snap.RequestID = "req-1"
			d.settingsAwaited = &awaitedSettings{engine: snap.Engine, requestID: "req-1"}
			d.reportSettingsOutcome(snap)

			assert.Equal(t, tc.wantKind, d.status.kind)
			got := d.status.render()
			for _, want := range tc.wantHas {
				assert.Contains(t, got, want)
			}
			for _, unwanted := range tc.wantNotIn {
				assert.NotContains(t, got, unwanted)
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
		RequestID:          "ours",
		Phase:              settingsPhaseSucceeded,
		Settings:           enginesettings.Config{ProxyPort: 11434},
		EffectiveProxyPort: 11999,
	}

	d.reportSettingsOutcome(snap)
	assert.Empty(t, d.status.render(), "reported a change this screen did not make")

	// The same engine, changed by someone else while this screen waits: the
	// desktop app, say. Matching on the engine alone consumed the await and
	// reported their result as ours.
	d.settingsAwaited = &awaitedSettings{engine: "ollama", requestID: "ours"}
	theirs := snap
	theirs.RequestID = "theirs"
	d.reportSettingsOutcome(theirs)
	assert.Empty(t, d.status.render(), "reported another client's change as this screen's")
	require.NotNil(t, d.settingsAwaited, "another client's snapshot spent this screen's await")

	// And it speaks exactly once for a change it did make.
	d.reportSettingsOutcome(snap)
	require.NotEmpty(t, d.status.render(), "said nothing about this screen's own change")
	d.status = toast{}
	d.reportSettingsOutcome(snap)
	assert.Empty(t, d.status.render(), "repeated the outcome on a later snapshot")
}

// TestAcceptedIsNotApplied is the regression guard for a verdict reported
// before the backend had one.
//
// The broker publishes a snapshot the moment it accepts a write — "applying",
// new settings saved, old port still in force — and only then stops the
// engine and does the work. Reading that as the outcome reported "saved"
// immediately; on a proxy change it reported a healthy move as a failure,
// since accepted and effective ports differ by definition at that point; and
// it spent the await, so the real verdict was never reported.
func TestAcceptedIsNotApplied(t *testing.T) {
	d := localDetail()
	d.settingsAwaited = &awaitedSettings{engine: "ollama", requestID: "req-1"}

	applying := enginesettings.Snapshot{
		Engine:    "ollama",
		RequestID: "req-1",
		Phase:     settingsPhaseApplying,
		Settings:  enginesettings.Config{ProxyPort: 11500, ServerPort: 11435},
		// Still where it was: the change has been accepted, not carried out.
		EffectiveProxyPort: 11434,
	}
	d.reportSettingsOutcome(applying)
	assert.Empty(t, d.status.render(), "reported an outcome while the change was still applying")
	require.NotNil(t, d.settingsAwaited, "the applying snapshot spent the await, so the real verdict will be ignored")

	// A never-configured engine reports no phase at all; that is not a verdict
	// either.
	idle := applying
	idle.Phase = ""
	d.reportSettingsOutcome(idle)
	require.NotNil(t, d.settingsAwaited, "an empty phase was read as a verdict")

	// The terminal snapshot for the same request is the one that speaks.
	done := applying
	done.Phase = settingsPhaseSucceeded
	done.EffectiveProxyPort = 11500
	d.reportSettingsOutcome(done)
	assert.Contains(t, d.status.render(), "saved", "terminal snapshot reports a save")

	// And a genuine failure after "applying" is reported, not lost.
	d = localDetail()
	d.settingsAwaited = &awaitedSettings{engine: "ollama", requestID: "req-2"}
	applying.RequestID = "req-2"
	d.reportSettingsOutcome(applying)
	failed := applying
	failed.Phase = settingsPhaseFailed
	failed.Error = "engine did not become ready"
	d.reportSettingsOutcome(failed)
	assert.Contains(t, d.status.render(), "did not become ready", "failure after applying")
}

// TestSlowApplyIsStillApplying checks a deadline on the apply reply is not
// reported as a failure.
//
// Accepted work is target-owned: the broker finishes a port swap whether or
// not anyone is still listening. Treating the deadline as a rejection reported
// failure on the path where the change was working, then dropped the await and
// the cached snapshot, so the real verdict that followed went unreported.
func TestSlowApplyIsStillApplying(t *testing.T) {
	d := localDetail()
	d.engines = []engineStatus{{Engine: "ollama", Installed: true}}
	d.refreshEngines()
	seedSettings(d, ollamaSettings())
	d.settingsAwaited = &awaitedSettings{engine: "ollama", requestID: "req-1"}

	cmd, _ := d.update(engineSettingsAppliedMsg{engine: "ollama", err: context.DeadlineExceeded})
	assert.Nil(t, cmd, "a deadline triggered a re-read, as though the write had been rejected")
	require.NotNil(t, d.settingsAwaited, "a deadline cleared the await, so the verdict that follows will be ignored")
	assert.Contains(t, d.settings, "ollama", "a deadline discarded the snapshot the in-flight write was based on")
	got := d.status.render()
	assert.NotContains(t, got, "fail", "change is still applying")
	assert.Contains(t, got, "applying", "change is still applying")

	d.reportSettingsOutcome(enginesettings.Snapshot{
		Engine: "ollama", RequestID: "req-1", Phase: settingsPhaseSucceeded,
	})
	assert.Contains(t, d.status.render(), "saved", "verdict after a deadline")
}

// TestSettingsApplyBudgetIsTheBackends checks the apply call is given the time
// the backend works to, not the one sized for control calls.
func TestSettingsApplyBudgetIsTheBackends(t *testing.T) {
	assert.Equal(t, enginesettings.CallBudget, settingsApplyBudget(enginesettings.Request{Engine: "ollama"}), "local apply budget")
	assert.Equal(t, enginesettings.RelayBudget, settingsApplyBudget(enginesettings.Request{NodeID: "peer", Engine: "ollama"}), "relayed apply budget")
	assert.Greater(t, settingsApplyBudget(enginesettings.Request{}), callTimeout, "an apply is allowed more than a control call")
}

// TestSettingsPromptsHoldTheKeyboard checks the two settings waiting states
// keep the shell's global keys away.
//
// With a restart confirmation armed, q quit instead of answering it and tab
// left the restart armed behind a prompt no longer on screen. With a settings
// read in flight, the shell was free to act on keys that belong to the field
// about to open.
func TestSettingsPromptsHoldTheKeyboard(t *testing.T) {
	d := localDetail()
	d.settingsConfirm = &enginesettings.Request{Engine: "ollama"}
	assert.True(t, d.CapturingInput(), "an armed restart confirmation does not hold the keyboard")

	d = localDetail()
	d.settingsWanted = &pendingSettingsEdit{engine: "ollama", mode: detailInputEnginePort}
	assert.True(t, d.CapturingInput(), "a settings read in flight does not hold the keyboard")
}

// TestSettingsWriteFollowsTheFieldNotTheCursor is the regression guard for a
// value typed for one engine being written to another.
//
// A field can open after a settings read, and the cursor is free to move while
// the read is in flight. Building the request from the selection wrote the
// value typed into Ollama's field into LM Studio's settings.
func TestSettingsWriteFollowsTheFieldNotTheCursor(t *testing.T) {
	d := localDetail()
	d.engines = []engineStatus{
		{Engine: "ollama", DisplayName: "Ollama", Installed: true, Port: 11435},
		{Engine: "lmstudio", DisplayName: "LM Studio", Installed: true, Port: 1235},
	}
	d.refreshEngines()
	seedSettings(d, ollamaSettings())
	lms := ollamaSettings()
	lms.Engine = "lmstudio"
	lms.Revision = 3
	seedSettings(d, lms)

	// Open Ollama's engine port field, then move the cursor to LM Studio.
	d.openSettingsField(d.settings["ollama"], detailInputEnginePort)
	d.engineTable.SetCursor(1)
	sel := d.selectedEngine()
	require.NotNil(t, sel, "setup: cursor is not on LM Studio")
	require.Equal(t, "lmstudio", sel.Engine, "setup: cursor is not on LM Studio")

	require.NotNil(t, d.submitSettings(detailInputEnginePort, "11600", "11600"), "the submit issued no preview")
	// The submit names the engine it built the request for. Resolved from the
	// cursor, this read "checking LM Studio settings".
	got := d.status.render()
	assert.Contains(t, got, "Ollama", "write follows the field's engine")
	assert.NotContains(t, got, "LM Studio", "write follows the field's engine")
	assert.Empty(t, d.settingsEngine, "the field's engine outlived the submit")
}

// TestLateSettingsReplyDoesNotOpenAnotherNodesEditor is the regression guard
// for one machine's settings being written to another.
//
// A settings read is not cancelled when its screen closes. Its reply landed on
// whichever detail screen was open by then, opened that node's field prefilled
// with the first node's value, and cached the first node's revision against it.
func TestLateSettingsReplyDoesNotOpenAnotherNodesEditor(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)
	open := func(key string) *nodeDetail {
		d := newNodeDetail(nil, nodeRow{key: key, name: key, presence: presenceOnline})
		d.SetSize(100, 30)
		d.engines = []engineStatus{{Engine: "ollama", Installed: true}}
		d.refreshEngines()
		v.detail = d
		require.NotNil(t, d.editSetting(&d.engines[0], detailInputEnginePort), "%s did not start a settings read", key)
		return d
	}

	open("node-a")
	v.Update(tea.KeyMsg{Type: tea.KeyEsc})
	require.Nil(t, v.detail, "node A's detail did not close while its settings read was pending")
	second := open("node-b")

	stale := ollamaSettings()
	stale.NodeID = "node-a"
	stale.Settings.ServerPort = 11500
	v.Update(detailReply{node: "node-a", msg: engineSettingsMsg{snapshot: stale}})
	v.Update(detailReply{node: "node-a", msg: engineSettingsMsg{err: errors.New("node-a went away")}})
	v.Update(detailReply{node: "node-a", msg: enginePreviewMsg{request: enginesettings.Request{NodeID: "node-a", Engine: "ollama"}}})

	assert.Equal(t, detailInputNone, second.mode, "node A's reply opened node B's editor with port %q", second.input.Value())
	assert.NotContains(t, second.settings, "ollama", "node A's settings reply was cached on node B")
	assert.NotNil(t, second.settingsWanted, "node A's reply consumed node B's pending settings edit")

	// And node B's own reply still does what it is for; without this the
	// assertions above would pass if every reply were dropped.
	own := ollamaSettings()
	own.NodeID = "node-b"
	v.Update(detailReply{node: "node-b", msg: engineSettingsMsg{snapshot: own}})
	assert.Equal(t, detailInputEnginePort, second.mode, "node B's own reply opens its field")
	assert.Equal(t, "11434", second.input.Value(), "node B's own reply opens its field")
}

// TestLateOperationReplyStaysWithItsNode is the regression guard for one
// machine's outcome being reported on another's screen.
//
// A slow operation's reply outlives the screen that sent it. Start something
// on node A, open node B, and A's "start failed" landed on B.
func TestLateOperationReplyStaysWithItsNode(t *testing.T) {
	b := newNodeDetail(nil, nodeRow{key: "node-b", name: "B", presence: presenceOnline})
	b.SetSize(100, 30)
	b.engines = []engineStatus{{Engine: "ollama", DisplayName: "Ollama", Installed: true}}
	b.refreshEngines()

	failed := classifyOpResult("start", "ollama", "start", errors.New("engine did not come up"))
	b.update(detailReply{node: "node-a", msg: failed})
	assert.NotContains(t, b.status.render(), "failed", "node A's outcome was reported on node B")

	b.update(detailReply{node: "node-b", msg: failed})
	assert.Contains(t, b.status.render(), "start (Ollama) failed", "node B's own outcome is reported")
}

// TestDetailRepliesCarryTheirNode checks the other half of the envelope: a
// reply leaves addressed to the screen that asked, whatever screen is open by
// the time it lands.
func TestDetailRepliesCarryTheirNode(t *testing.T) {
	a := newNodeDetail(nil, nodeRow{key: "node-a", name: "A", presence: presenceOnline})
	reply := engineOpMsg{what: "start", engine: "ollama"}
	got := a.own(func() tea.Msg { return reply })()
	tagged, ok := got.(detailReply)
	if assert.True(t, ok, "reply is addressed to its node") {
		assert.Equal(t, "node-a", tagged.node)
		assert.Equal(t, reply, tagged.msg)
	}
	assert.Nil(t, a.own(nil), "no command was turned into one")
	assert.Nil(t, a.own(func() tea.Msg { return nil })(), "a command with nothing to say was given an envelope")
}

// TestSecondSettingsChangeWaitsForTheFirst is the regression guard for a
// second save replacing the first before its verdict came back. The screen
// follows one outstanding write, so the first outcome was never reported.
func TestSecondSettingsChangeWaitsForTheFirst(t *testing.T) {
	d := localDetail()
	d.engines = []engineStatus{
		{Engine: "ollama", DisplayName: "Ollama", Installed: true},
		{Engine: "lmstudio", DisplayName: "LM Studio", Installed: true},
	}
	d.refreshEngines()
	d.settingsAwaited = &awaitedSettings{engine: "ollama", requestID: "first"}

	assert.Nil(t, d.editSetting(&d.engines[1], detailInputEnginePort), "a second settings change started while the first was still applying")
	assert.Equal(t, detailInputNone, d.mode, "a second settings field was opened while the first change was applying")
	assert.Nil(t, d.settingsWanted, "a second settings field was opened while the first change was applying")
	assert.Contains(t, d.status.render(), "wait for the Ollama settings change", "refusal says what to wait for")
}

// progressPush builds a progress notification from its fields as sent.
func progressPush(method string, fields map[string]any) NotificationMsg {
	params, _ := json.Marshal(fields)
	return NotificationMsg{Msg: &rpc.Message{Method: method, Params: params}}
}

// TestInstallProgressStaysOnThisMachine checks that a local install's progress
// is not reported on a peer's screen. The payload carries no node, so shown
// there it read as the peer installing something.
func TestInstallProgressStaysOnThisMachine(t *testing.T) {
	frame := progressPush("engine:install-progress",
		map[string]any{"engine": "ollama", "stage": "downloading", "percent": 40})

	peer := remoteDetail()
	peer.update(frame)
	assert.NotContains(t, peer.status.render(), "install", "this machine's install was reported on a peer's screen")

	local := localDetail()
	local.update(frame)
	assert.Contains(t, local.status.render(), "downloading (40%)", "this machine's install progress is shown")
}

// TestProgressWithoutAPercentShowsNone is the regression guard for progress
// frames read as stuck downloads. The engine manager leaves the percent out
// while progress is indeterminate and sends -1 on failure; printed, those read
// "(0%)" and "(-1%)".
func TestProgressWithoutAPercentShowsNone(t *testing.T) {
	peer := remoteDetail()
	peer.update(progressPush("engine:remote-progress",
		map[string]any{"node": "peer", "engine": "ollama", "op": "pull", "stage": "pulling manifest"}))
	got := peer.status.render()
	assert.Contains(t, got, "pulling manifest", "indeterminate remote progress")
	assert.NotContains(t, got, "%", "indeterminate remote progress")
	peer.update(progressPush("engine:remote-progress",
		map[string]any{"node": "peer", "engine": "ollama", "op": "pull", "stage": "downloading", "percent": 25}))
	assert.Contains(t, peer.status.render(), "downloading (25%)", "reported percent is kept")

	local := localDetail()
	local.update(progressPush("engine:pull-progress",
		map[string]any{"engine": "ollama", "stage": "pulling manifest"}))
	assert.NotContains(t, local.status.render(), "%", "indeterminate pull progress")

	local.update(progressPush("engine:install-progress",
		map[string]any{"engine": "ollama", "stage": "failed", "percent": -1, "error": "disk full"}))
	got = local.status.render()
	assert.NotContains(t, got, "%", "failed install")
	assert.Contains(t, got, "install", "failed install")
	assert.Contains(t, got, "failed: disk full", "failed install")
}

// TestHiddenListsRefuseTheirKeys is the regression guard for keys acting on a
// list the frame had no room to show: on a short terminal a delete could be
// armed against a model the operator could not see.
func TestHiddenListsRefuseTheirKeys(t *testing.T) {
	d := localDetail()
	d.engines = []engineStatus{{Engine: "ollama", DisplayName: "Ollama", Installed: true, Running: true}}
	models := []string{"model-a", "model-b", "model-c"}
	d.models = modelsResult{Models: models, ModelsByEngine: map[string][]string{"ollama": models}}
	d.refreshEngines()
	d.refreshModels()

	modelsOnly, both := 0, 0
	for h := 30; h > 0; h-- {
		d.SetSize(100, h)
		d.View()
		if d.modelsHidden && !d.enginesHidden && modelsOnly == 0 {
			modelsOnly = h
		}
		if d.enginesHidden && both == 0 {
			both = h
		}
	}
	require.NotZero(t, modelsOnly, "some height must hide only the models")
	require.NotZero(t, both, "some height must hide both lists")

	d.SetSize(100, 30)
	d.View()
	require.False(t, d.enginesHidden, "a tall screen still counted a list as hidden")
	require.False(t, d.modelsHidden, "a tall screen still counted a list as hidden")

	d.SetSize(100, modelsOnly)
	d.View()
	d.pane = detailModels
	d.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	assert.Nil(t, d.pending, "a delete was armed against a model list that was not on screen")
	assert.Contains(t, d.status.render(), "too little room to show the models", "refusal says why")

	d.SetSize(100, both)
	d.View()
	d.pane = detailEngines
	cmd, _ := d.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
	assert.Nil(t, cmd, "an engine was started from an engine list that was not on screen")
}

// TestOpenDetailFollowsPresenceAndMembership is the regression guard for a
// detail screen that kept describing a peer as it was when the screen opened.
//
// Presence and membership are merged from feeds the detail does not read — the
// cluster roster among them — so a peer that left the cluster or dropped off
// the network went on reading "Member" and "Online" until the screen was
// reopened, with its engine controls still offered.
func TestOpenDetailFollowsPresenceAndMembership(t *testing.T) {
	v := newNodesView(nil)
	v.SetSize(100, 30)
	peer := availableNode{HostUUID: "peer", Name: "peer-host", IPAddress: "10.0.0.2", Port: 9000, Trusted: true}
	v.feeds.discovered = []availableNode{peer}
	v.rebuild()
	v.selectedKey = "peer"
	v.restoreSelection()
	v.openDetail()
	d := v.detail
	d.engines = []engineStatus{{Engine: "ollama", Installed: true, Running: true}}
	d.refreshEngines()
	require.Equal(t, membershipMember, d.node.membership, "setup: detail membership")
	require.Equal(t, presenceOnline, d.node.presence, "setup: detail presence")

	// The peer leaves the cluster but is still on the network.
	peer.Trusted = false
	v.Update(discoveryPush(peer))
	assert.NotEqual(t, membershipMember, d.node.membership, "the detail still reads Member after the peer left the cluster")
	assert.Empty(t, d.engines, "engine controls are still offered for a node no longer in the cluster")
	assert.Contains(t, d.View(), "not in this cluster", "engines pane says why the list went")

	// And then drops off the network entirely.
	v.Update(discoveryPush())
	assert.NotEqual(t, presenceOnline, d.node.presence, "the detail still reads Online after the node dropped out of every feed")
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
		assert.Equal(t, want, empty.engineLabel(engine), "engine %q has the same name without an engine list", engine)
		assert.NotEqual(t, engine, want, "display name differs from wire id")
	}

	// A genuinely unknown engine still has to render as something.
	assert.Equal(t, "some-new-engine", empty.engineLabel("some-new-engine"), "unknown engine id passes through")
}

// TestEmptyEngineListExplainsItself checks the empty states read differently.
// The manager answering with nothing, the manager not answering, and the node
// being one we cannot ask at all have different causes, and the operator's
// next move differs for each.
func TestEmptyEngineListExplainsItself(t *testing.T) {
	local := localDetail()
	assert.Contains(t, local.emptyEnginesHint(), "engine manager", "local hint points at the engine manager")

	member := remoteDetail()
	member.node.membership = membershipMember
	assert.Contains(t, member.emptyEnginesHint(), "this node", "remote member hint attributes the gap to the peer")

	assert.NotEqual(t, local.emptyEnginesHint(), member.emptyEnginesHint(), "local and remote read identically; the causes are different")
}

// TestUnpairedNodeIsNotCalledSilent is the regression guard for a machine that
// was answering perfectly well being reported as unresponsive.
//
// A peer's engines are fetched over pin-based mTLS, so a node outside the
// cluster cannot be asked at all. That call was made anyway, and its failure
// rendered as "not answering" — beside a hardware readout, polled over an
// endpoint that needs no pairing, visibly updating for the same machine.
func TestUnpairedNodeIsNotCalledSilent(t *testing.T) {
	cases := map[nodeMembership]string{
		membershipNone:    "not in this cluster",
		membershipForeign: "another cluster",
		membershipPending: "still pairing",
	}
	for membership, want := range cases {
		d := remoteDetail()
		d.node.membership = membership

		assert.Nil(t, d.enginesCmd(), "%v: asked for engines over a link that cannot carry the question", membership)
		got := d.emptyEnginesHint()
		assert.Contains(t, got, want, "%v: unpaired hint", membership)
		assert.NotContains(t, got, "not answering", "%v: hint calls a reachable node silent", membership)
		// Models come from what the node advertises over discovery, which
		// needs no pairing, so the models pane must not blame a stopped engine
		// for what it cannot see either way.
		d.node.presence = presenceOnline
		assert.NotContains(t, d.emptyModelsHint(), "start an engine", "%v: models hint claims an engine needs starting", membership)
	}

	// A member is still asked, and is still allowed to be silent.
	member := remoteDetail()
	member.node.membership = membershipMember
	assert.NotNil(t, member.enginesCmd(), "a cluster member was not asked for its engines")
}

// TestUnpairedHintAgreesWithTheModelList checks the engines pane does not deny
// what the pane below it is already showing.
//
// Discovery carries which engine serves each model and needs no pairing to do
// it, so an unpaired node's model list arrives with its ENGINE column filled
// in. "Its engines are not visible from here", printed directly above a list
// naming Ollama, claimed less than the screen displayed. What pairing actually
// buys is their state and their controls.
func TestUnpairedHintAgreesWithTheModelList(t *testing.T) {
	d := remoteDetail()
	d.node.membership = membershipNone
	d.models = modelsResult{ModelsByEngine: map[string][]string{
		"ollama": {"gemma3:4b", "gemma4:12b"},
	}}
	d.refreshModels()

	hint := d.emptyEnginesHint()
	assert.Contains(t, hint, "Ollama", "hint names the engine the model list attributes rows to")
	assert.Contains(t, hint, "manage", "hint says what pairing adds")

	// With nothing advertised there is nothing to name, and the sentence must
	// not trail off into an empty list.
	bare := remoteDetail()
	bare.node.membership = membershipNone
	assert.NotContains(t, bare.emptyEnginesHint(), "advertises", "hint claims advertised engines for a node reporting none")
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
			assert.NotContains(t, seen, k, "key must have one node binding; current verb %q", verb)
			seen[k] = verb
		}
	}

	shell := newGlobalKeyMap(len(defaultViews(nil)))
	for _, g := range []key.Binding{shell.NextTab, shell.PrevTab, shell.JumpTab, shell.Help, shell.Quit} {
		for _, k := range g.Keys() {
			assert.NotContains(t, seen, k, "shell binding %q must not clash with a node verb", g.Help().Desc)
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

	require.LessOrEqual(t, renderedRows(d.View()), budget, "setup already overflows")

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

	assert.GreaterOrEqual(t, d.hardwareHeight(), 4, "one row per reading")
	// The frame is what matters, not any particular table's height. Asserting on
	// the height instead measured a value SetSize wrote and the renderer then
	// overwrote — so it passed whether or not the frame actually fit.
	assert.LessOrEqual(t, renderedRows(d.View()), budget, "after the hardware block grew; the shell will delete the status line")
	assert.Contains(t, d.View(), "something to push off the bottom", "the status line was squeezed out by the hardware block")
}

// TestDetailResizesCatalogBrowser checks the browser follows a terminal resize
// rather than keeping the size it was opened at.
func TestDetailResizesCatalogBrowser(t *testing.T) {
	d := localDetail()
	d.engines = []engineStatus{{Engine: "ollama", Installed: true, Running: true}}
	d.refreshEngines()
	d.openCatalog()
	require.NotNil(t, d.catalog, "catalog did not open")

	d.SetSize(140, 40)
	assert.Equal(t, 140, d.catalog.width, "catalog width after resize")
	assert.Equal(t, 40, d.catalog.height, "catalog height after resize")
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
	got := d.selectedModel()
	require.NotNil(t, got, "setup: selected model")
	require.Equal(t, "charlie", got.model, "setup: selected model")

	// A background pull lands "alpha", which sorts first.
	d.models = modelsResult{
		ModelsByEngine: map[string][]string{"ollama": {"alpha", "bravo", "charlie", "delta"}},
	}
	d.refreshModels()

	got = d.selectedModel()
	require.NotNil(t, got, "nothing selected after the rebuild")
	assert.Equal(t, "charlie", got.model, "selection slid; a delete would now destroy the wrong model")
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
	assert.NotNil(t, d.selectedModel(), "no model selected after the inventory arrived (cursor %d); load, eject, and delete are all dead", d.modelTable.Cursor())

	// Same for the engines pane, which gates install/start/stop.
	d.engines = []engineStatus{{Engine: "ollama", Installed: true}}
	d.refreshEngines()
	assert.NotNil(t, d.selectedEngine(), "no engine selected after engines arrived (cursor %d)", d.engineTable.Cursor())
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
	require.NotNil(t, d.pending, "delete did not arm")
	assert.True(t, d.CapturingInput(), "an armed action does not capture input, so tab or a digit escapes it")
	assert.Contains(t, d.status.render(), "press y to confirm", "confirmation prompt is on screen")
	// The prompt must not expire out from under the armed state.
	assert.False(t, d.status.expired(), "the confirmation prompt expires while the action stays armed")
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
	assert.Nil(t, d.handleKeyForTest(t, "n"), "a non-confirming key ran the destructive action")
	assert.Nil(t, d.pending, "the action stayed armed after being cancelled")
	assert.Contains(t, d.status.render(), "cancelled", "cancelling said nothing")

	// y runs it and disarms.
	d = build()
	assert.NotNil(t, d.handleKeyForTest(t, "y"), "confirming produced no command; the delete never ran")
	assert.Nil(t, d.pending, "the action stayed armed after being confirmed")
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

	assert.Nil(t, d.pending, "armed a confirmation for an uninstall that cannot run on a peer")
	assert.Contains(t, d.status.render(), "machine running it", "explanation given")
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
		got, ok := parsePort(in)
		if assert.True(t, ok, "parsePort(%q)", in) {
			assert.Equal(t, want, got, "parsePort(%q)", in)
		}
	}
	for _, in := range []string{"", "0", "-1", "65536", "abc", "80x"} {
		_, ok := parsePort(in)
		assert.False(t, ok, "parsePort(%q) accepted an invalid port", in)
	}
}

// TestServiceTabNoLongerOffersPorts checks the ports are configured in exactly
// one place, not two.
func TestServiceTabNoLongerOffersPorts(t *testing.T) {
	v := newServiceView(nil)
	for _, it := range v.items {
		assert.NotContains(t, strings.ToLower(it.label), "port", "Service tab still offers %q; ports belong on the node", it.label)
	}
}
