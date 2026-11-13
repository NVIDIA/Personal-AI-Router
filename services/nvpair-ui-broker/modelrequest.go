// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"nvpair-shared/modelrequest"
)

// relayModelPreparation carries operational metadata between supervised
// workers. Inference content remains exclusively in the proxy data plane.
func (b *Broker) relayModelPreparation(method string, params json.RawMessage) bool {
	if method != modelrequest.Request && method != modelrequest.Cancel {
		return false
	}
	var request modelrequest.Preparation
	if json.Unmarshal(params, &request) != nil || request.ID == "" || request.Engine == "" {
		return true
	}
	profile, ok := engineProxyProfileFor(request.Engine)
	if !ok || !profile.LoadOnRequest {
		return true
	}
	worker := b.getEngineMgr()
	target := modelrequest.EngineRequest
	if method == modelrequest.Cancel {
		target = modelrequest.EngineCancel
	}
	if worker != nil {
		if err := worker.Notify(target, request); err == nil {
			return true
		}
	}
	if proxy := b.engineProxyHandle(profile); proxy != nil {
		_ = proxy.peer.Notify(modelrequest.ReplyMethod, modelrequest.Reply{ID: request.ID, Engine: request.Engine, Error: "engine manager is unavailable", Status: http.StatusServiceUnavailable})
	}
	return true
}

func (b *Broker) relayModelPrepared(method string, params json.RawMessage) bool {
	if method != modelrequest.EngineReply {
		return false
	}
	var reply modelrequest.Reply
	if json.Unmarshal(params, &reply) != nil {
		return true
	}
	profile, ok := engineProxyProfileFor(reply.Engine)
	if !ok || !profile.LoadOnRequest {
		return true
	}
	if proxy := b.engineProxyHandle(profile); proxy != nil {
		_ = proxy.peer.Notify(modelrequest.ReplyMethod, params)
	}
	return true
}

func (b *Broker) resetModelPreparations() {
	for _, profile := range engineProxyProfiles {
		if !profile.LoadOnRequest {
			continue
		}
		if proxy := b.engineProxyHandle(profile); proxy != nil {
			_ = proxy.peer.Notify(modelrequest.Reset, nil)
		}
	}
}

func (b *Broker) cancelOrphanedModelPreparations() {
	if manager := b.getEngineMgr(); manager != nil {
		_ = manager.Notify(modelrequest.EngineCancelAll, nil)
	}
}

// Publish before forwarding the model-list event to clients. The acknowledged
// update prevents a newly downloaded model from briefly disappearing between
// the UI inventory and the scanner's next enrichment sweep.
func (b *Broker) publishLocalModelInventory(raw json.RawMessage) {
	if b.nodeID == "" {
		return
	}
	var inventory struct {
		RequestableByEngine map[string][]string `json:"requestableByEngine"`
	}
	if json.Unmarshal(raw, &inventory) != nil {
		return
	}
	for _, profile := range engineProxyProfiles {
		models, known := inventory.RequestableByEngine[profile.Name]
		if !profile.LoadOnRequest || !known {
			continue
		}
		b.callProxyManual(b.engineProxyHandle(profile), profile.Name, "node/set-local-models", modelrequest.LocalInventory{Engine: profile.Name, ID: b.nodeID, Models: append([]string{}, models...), EnginePort: int(b.engineProxy(profile).backendPort.Load()), Enabled: b.engineProxy(profile).requestEnabled.Load()}, "model-inventory")
	}
}
