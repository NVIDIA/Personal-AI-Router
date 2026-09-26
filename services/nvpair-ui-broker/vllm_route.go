// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"time"

	"nvpair-shared/engines"
)

// replyVLLMGroupRoute answers Engine Manager's serving-group route gate. It
// reconciles the vLLM advertisement now rather than on the next tick, then
// reports whether the local proxy lists this node among the model's inference
// owners. Only the current Engine Manager process that asked is answered.
func (b *Broker) replyVLLMGroupRoute(origin *rpcWorker, raw json.RawMessage) {
	var p struct {
		RequestID string `json:"requestId"`
		Model     string `json:"model"`
	}
	if json.Unmarshal(raw, &p) != nil || len(p.RequestID) != 32 || p.Model == "" || len(p.Model) > 512 {
		return
	}
	b.workersMu.Lock()
	current := origin != nil && b.engineMgr == origin
	b.workersMu.Unlock()
	if !current {
		return
	}
	b.reconcileAdvertiseVLLM(&http.Client{Timeout: 2 * time.Second})
	_ = origin.Notify("engine:vllm-group-route-state", map[string]any{"requestId": p.RequestID, "routable": b.localVLLMModelOwner(p.Model)})
}

func (b *Broker) localVLLMModelOwner(model string) bool {
	proxy := b.engineProxyHandle(vllmProxyProfile)
	if proxy == nil {
		return false
	}
	params, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return false
	}
	result, rpcErr, err := proxy.Call(context.Background(), engines.AddressMethod(vllmProxyProfile.Name, "node/model-owners"), params)
	if err != nil || rpcErr != nil {
		return false
	}
	var owners struct {
		Nodes []string `json:"nodes"`
	}
	return json.Unmarshal(result, &owners) == nil && slices.Contains(owners.Nodes, b.nodeID)
}
