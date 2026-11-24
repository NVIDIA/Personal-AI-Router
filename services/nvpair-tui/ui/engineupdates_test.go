// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	"nvpair-tui/rpc"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const managedUpdateResult = `{"updateAvailable":{
	"currentVersion":"0.9.5", "latestVersion":"0.10.0", "installType":"managed",
	"releaseUrl":"https://github.com/ollama/ollama/releases/tag/v0.10.0"
}}`

// Each test answers the broker calls made by the detail screen, without running
// a service or fetching any vendor release.
func engineUpdateBroker(t *testing.T) (*rpc.Client, *rpc.Codec) {
	t.Helper()
	controller, broker := net.Pipe()
	t.Cleanup(func() {
		assert.NoError(t, broker.Close())
		assert.NoError(t, controller.Close())
	})
	require.NoError(t, broker.SetDeadline(time.Now().Add(5*time.Second)))
	client := rpc.NewClient(controller, controller)
	go client.Run(t.Context())
	return client, rpc.NewCodec(broker, broker)
}

func answerUpdateCall(t *testing.T, cmd tea.Cmd, broker *rpc.Codec, method string, params map[string]string, response rpc.Message) tea.Msg {
	t.Helper()
	require.NotNil(t, cmd)
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	request, err := broker.Read()
	require.NoError(t, err)
	require.Equal(t, method, request.Method)
	var got map[string]string
	require.NoError(t, json.Unmarshal(request.Params, &got))
	assert.Equal(t, params, got)
	response.JSONRPC, response.ID = "2.0", request.ID
	require.NoError(t, broker.Write(&response))
	select {
	case result := <-done:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("the detail screen did not receive the broker reply")
		return nil
	}
}

func TestEngineUpdates_ShowExternalInstructionsForThePairedNode(t *testing.T) {
	t.Setenv(disableUpdateCheckEnv, "")
	client, broker := engineUpdateBroker(t)
	d := remoteDetail()
	d.client, d.node.membership = client, membershipMember
	const external = `{"updateAvailable":{
		"currentVersion":"0.0.17-4", "latestVersion":"0.1.1-2", "installType":"external",
		"releaseUrl":"https://lmstudio.ai/changelog",
		"downloadUrl":"https://lmstudio.ai/docs/cli/daemon/daemon-update"
	}}`
	check, _ := d.update(detailEnginesMsg{engines: []engineStatus{{Engine: "lmstudio", Installed: true}}})
	d.update(answerUpdateCall(t, check, broker, "engine:remote-check-updates", map[string]string{"engine": "lmstudio", "node": "peer"}, rpc.Message{Result: json.RawMessage(external)}))

	assert.Contains(t, d.View(), "https://lmstudio.ai/docs/cli/daemon/daemon-update")
}

func TestEngineUpdates_KeepLastOfferWhenALifecycleCheckFails(t *testing.T) {
	t.Setenv(disableUpdateCheckEnv, "")
	client, broker := engineUpdateBroker(t)
	d := localDetail()
	d.client = client
	params := map[string]string{"engine": "ollama"}
	engine := engineStatus{Engine: "ollama", Installed: true}
	inventory := func() tea.Cmd {
		cmd, _ := d.update(detailEnginesMsg{engines: []engineStatus{engine}})
		return cmd
	}
	d.update(answerUpdateCall(t, inventory(), broker, "engine:check-updates", params, rpc.Message{Result: json.RawMessage(managedUpdateResult)}))
	require.Nil(t, inventory(), "repeated inventory should reuse the confirmed offer")

	engine.Running = true
	d.update(answerUpdateCall(t, inventory(), broker, "engine:check-updates", params, rpc.Message{Error: &rpc.RPCError{Code: -32000, Message: "vendor unreachable"}}))
	assert.Contains(t, d.View(), "0.10.0")
	assert.NotContains(t, d.View(), "vendor unreachable")
	require.Nil(t, inventory())

	engine.Running = false
	d.update(answerUpdateCall(t, inventory(), broker, "engine:check-updates", params, rpc.Message{Result: json.RawMessage(`{"updateAvailable":null}`)}))
	assert.NotContains(t, d.View(), "0.10.0")
}

func TestEngineUpdates_IgnoreOffersFromAPreviousInstallation(t *testing.T) {
	t.Setenv(disableUpdateCheckEnv, "")
	d := localDetail()
	client, broker := engineUpdateBroker(t)
	d.client = client
	installed := detailEnginesMsg{engines: []engineStatus{{Engine: "ollama", Installed: true}}}
	params := map[string]string{"engine": "ollama"}
	check, _ := d.update(installed)
	oldOffer := answerUpdateCall(t, check, broker, "engine:check-updates", params, rpc.Message{Result: json.RawMessage(managedUpdateResult)})

	d.update(detailEnginesMsg{})
	check, _ = d.update(installed)
	d.update(answerUpdateCall(t, check, broker, "engine:check-updates", params, rpc.Message{Result: json.RawMessage(`{"updateAvailable":null}`)}))
	d.update(oldOffer)

	assert.NotContains(t, d.View(), "0.10.0")
}

func TestEngineUpdates_CheckEachInstalledEngineEverySixHours(t *testing.T) {
	t.Setenv(disableUpdateCheckEnv, "")
	d := localDetail()
	client, calls := removalBroker(t, "")
	d.client = client
	checks := func(cmd tea.Cmd) {
		t.Helper()
		require.NotNil(t, cmd)
		batch, ok := cmd().(tea.BatchMsg)
		require.True(t, ok, "each installed engine should have its own check")
		var checked []string
		for _, check := range batch {
			d.update(check())
			method, params := receivedCall(t, calls)
			assert.Equal(t, "engine:check-updates", method)
			checked = append(checked, params["engine"])
		}
		assert.ElementsMatch(t, []string{"ollama", "lmstudio", "llamacpp", "vllm"}, checked)
	}
	now := time.Now()
	initial, _ := d.update(detailEnginesMsg{engines: []engineStatus{
		{Engine: "ollama", Installed: true}, {Engine: "lmstudio", Installed: true},
		{Engine: "llamacpp", Installed: true}, {Engine: "vllm", Installed: true},
	}})
	checks(initial)
	early, _ := d.update(TickMsg{Now: now.Add(5 * time.Hour)})
	assert.Nil(t, early)
	due, _ := d.update(TickMsg{Now: now.Add(6 * time.Hour)})
	checks(due)
}

func TestEngineUpdates_SkipChecksForUnpairedPeersAndWhenDisabled(t *testing.T) {
	engine := detailEnginesMsg{engines: []engineStatus{{Engine: "ollama", Installed: true}}}
	t.Run("unpaired peer", func(t *testing.T) {
		t.Setenv(disableUpdateCheckEnv, "")
		d := remoteDetail()
		d.client, _ = engineUpdateBroker(t)
		cmd, _ := d.update(engine)
		assert.Nil(t, cmd)
	})
	t.Run("disabled on this machine", func(t *testing.T) {
		t.Setenv(disableUpdateCheckEnv, "1")
		d := localDetail()
		d.client, _ = engineUpdateBroker(t)
		cmd, _ := d.update(engine)
		assert.Nil(t, cmd)
	})
}
