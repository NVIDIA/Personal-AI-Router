// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"regexp"
	"time"
)

var onboardingRelayID = regexp.MustCompile(`^[a-f0-9]{32}$`)

// Internal, request-correlated fixed cluster calls. Results can carry a PIN;
// they return only to the owning Engine Manager stdio channel, never fan out.
func (b *Broker) replyOnboardingCluster(origin *rpcWorker, raw json.RawMessage) {
	var request struct {
		RequestID string          `json:"requestId"`
		Method    string          `json:"method"`
		Params    json.RawMessage `json:"params"`
	}
	if len(raw) > 64<<10 || json.Unmarshal(raw, &request) != nil || !onboardingRelayID.MatchString(request.RequestID) {
		return
	}
	allowed := map[string]bool{"cluster:get-node-id": true, "nodes:get-initial": true, "cluster:invite-node": true, "cluster:invite-status": true, "cluster:cancel-invite": true}
	b.workersMu.Lock()
	current := origin != nil && b.engineMgr == origin
	cm := b.clusterMgr
	b.workersMu.Unlock()
	if !current {
		return
	}
	reply := map[string]any{"requestId": request.RequestID}
	if !allowed[request.Method] {
		reply["error"] = "unsupported onboarding cluster operation"
	} else if cm == nil {
		reply["error"] = "PAIR cluster manager is unavailable"
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		result, rpcErr, err := cm.peer.Call(ctx, request.Method, request.Params)
		if err != nil || rpcErr != nil {
			reply["error"] = "PAIR pairing operation failed; inspect its nonsecret status"
			if rpcErr != nil {
				reply["errorCode"] = rpcErr.Code
			}
		} else {
			reply["result"] = result
		}
	}
	_ = origin.Notify("engine:onboarding-cluster-response", reply)
}
