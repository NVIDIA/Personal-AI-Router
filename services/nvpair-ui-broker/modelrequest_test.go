// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"nvpair-shared/modelrequest"
	"testing"
)

func TestModelPreparationRelay_ForwardsOnlyOperationalMetadataToEngineManager(t *testing.T) {
	broker := &Broker{}
	worker, codec := newTestRPCWorkerPipe(t)
	broker.setEngineMgr(worker)
	request := modelrequest.Preparation{ID: "request-1", Engine: "vllm", Model: "org/model"}
	raw, err := json.Marshal(request)
	require.NoError(t, err)
	received := make(chan *Message, 1)
	go func() {
		message, err := codec.Read()
		assert.NoError(t, err)
		received <- message
	}()
	require.True(t, broker.relayModelPreparation(modelrequest.Request, raw))
	message := <-received
	require.NotNil(t, message)
	assert.Equal(t, modelrequest.EngineRequest, message.Method)
	var decoded modelrequest.Preparation
	require.NoError(t, json.Unmarshal(message.Params, &decoded))
	assert.Equal(t, request, decoded)
}

func TestModelPreparationRelay_RepliesWhenOnlyTheVLLMFacadeIsEnabled(t *testing.T) {
	broker := &Broker{}
	worker, codec := newTestRPCWorkerPipe(t)
	profile := mustEngineProxyProfile("vllm")
	broker.setEngineProxyHandle(profile, &proxyProcess{peer: worker.peer})
	reply := modelrequest.Reply{ID: "request-1", Engine: "vllm", Port: 8001}
	raw, err := json.Marshal(reply)
	require.NoError(t, err)
	received := make(chan *Message, 1)
	go func() {
		message, err := codec.Read()
		assert.NoError(t, err)
		received <- message
	}()
	require.True(t, broker.relayModelPrepared(modelrequest.EngineReply, raw))
	message := <-received
	require.NotNil(t, message)
	assert.Equal(t, modelrequest.ReplyMethod, message.Method)
	var decoded modelrequest.Reply
	require.NoError(t, json.Unmarshal(message.Params, &decoded))
	assert.Equal(t, reply, decoded)
}

func TestModelPreparationRelay_ReportsAnUnavailableManager(t *testing.T) {
	broker := &Broker{}
	worker, codec := newTestRPCWorkerPipe(t)
	profile := mustEngineProxyProfile("vllm")
	broker.setEngineProxyHandle(profile, &proxyProcess{peer: worker.peer})
	request := modelrequest.Preparation{ID: "request-1", Engine: "vllm", Model: "org/model"}
	raw, err := json.Marshal(request)
	require.NoError(t, err)
	received := make(chan *Message, 1)
	go func() {
		message, err := codec.Read()
		assert.NoError(t, err)
		received <- message
	}()
	require.True(t, broker.relayModelPreparation(modelrequest.Request, raw))
	message := <-received
	require.NotNil(t, message)
	var reply modelrequest.Reply
	require.NoError(t, json.Unmarshal(message.Params, &reply))
	assert.Equal(t, request.ID, reply.ID)
	assert.Equal(t, http.StatusServiceUnavailable, reply.Status)
	assert.Equal(t, "engine manager is unavailable", reply.Error)
}

func TestLocalModelInventory_PublishesTheActualPortAndEnabledIntent(t *testing.T) {
	broker := &Broker{nodeID: "local-node"}
	profile := mustEngineProxyProfile("vllm")
	broker.engineProxy(profile).backendPort.Store(18001)
	broker.engineProxy(profile).requestEnabled.Store(true)
	worker, codec := newTestRPCWorkerPipe(t)
	broker.setEngineProxyHandle(profile, &proxyProcess{peer: worker.peer})
	received := make(chan *Message, 1)
	go func() {
		message, err := codec.Read()
		if !assert.NoError(t, err) {
			received <- nil
			return
		}
		assert.NoError(t, codec.Respond(message.ID, map[string]bool{"ok": true}))
		received <- message
	}()

	broker.publishLocalModelInventory(json.RawMessage(`{"requestableByEngine":{"vllm":["org/new-model"]}}`))

	message := <-received
	require.NotNil(t, message)
	assert.Equal(t, "vllm:node/set-local-models", message.Method)
	var inventory modelrequest.LocalInventory
	require.NoError(t, json.Unmarshal(message.Params, &inventory))
	assert.Equal(t, modelrequest.LocalInventory{Engine: "vllm", ID: "local-node", EnginePort: 18001, Enabled: true, Models: []string{"org/new-model"}}, inventory)
}

func TestEngineAvailability_UsesEnabledIntentIndependentlyOfTheServingProcess(t *testing.T) {
	for _, test := range []struct {
		name             string
		enabled, running bool
	}{
		{name: "enabled between model processes", enabled: true},
		{name: "disabled despite an external listener", running: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			broker := &Broker{nodeID: "local-node"}
			profile := mustEngineProxyProfile("vllm")
			worker, codec := newTestRPCWorkerPipe(t)
			broker.setEngineProxyHandle(profile, &proxyProcess{peer: worker.peer})
			received := make(chan *Message, 1)
			go func() {
				message, err := codec.Read()
				if !assert.NoError(t, err) {
					received <- nil
					return
				}
				assert.NoError(t, codec.Respond(message.ID, map[string]bool{"ok": true}))
				received <- message
			}()
			raw, err := json.Marshal(map[string]any{"engine": "vllm", "port": 18001, "installed": true, "enabled": test.enabled, "running": test.running})
			require.NoError(t, err)

			broker.forwardEngineNotification("engine:state-changed", raw)

			message := <-received
			require.NotNil(t, message)
			assert.Equal(t, "vllm:node/set-local-models", message.Method)
			var inventory modelrequest.LocalInventory
			require.NoError(t, json.Unmarshal(message.Params, &inventory))
			assert.Equal(t, test.enabled, inventory.Enabled)
			assert.Nil(t, inventory.Models, "lifecycle updates must retain cached capability")
		})
	}
}
