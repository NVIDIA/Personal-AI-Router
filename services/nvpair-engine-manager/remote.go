// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// remote.go implements the engine:remote-* methods: the local engine-manager
// acting as a client that drives a peer's ec surface on behalf of a UI. The
// broker relays engine:* verbatim, so these arrive here directly. Each resolves
// the target node in the ec peer directory, dials it over pinned mTLS, and (for
// the streaming ops) relays progress up as engine:remote-progress before
// settling the request with the terminal result.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
)

// remoteParam is the shared input for the engine:remote-* methods. Fields not
// relevant to a given method are ignored.
type remoteParam struct {
	Node        string          `json:"node"`
	Engine      string          `json:"engine,omitempty"`
	Start       bool            `json:"start,omitempty"`
	Port        int             `json:"port,omitempty"`
	Model       string          `json:"model,omitempty"`
	OperationID string          `json:"operationId,omitempty"`
	SourceNode  string          `json:"sourceNode,omitempty"`
	Cancel      bool            `json:"cancel,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
}

// remoteProgress is the engine:remote-progress notification payload the broker
// forwards to a subscribed UI.
type remoteProgress struct {
	OpID    string `json:"opId"`
	Node    string `json:"node"`
	Engine  string `json:"engine,omitempty"`
	Op      string `json:"op,omitempty"`
	Stage   string `json:"stage,omitempty"`
	Percent int    `json:"percent,omitempty"`
	Message string `json:"message,omitempty"`
	Network string `json:"network,omitempty"`
}

// newOpID mints a random correlation id for a remote operation.
func newOpID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func remotePullOperationID(p remoteParam) (string, error) {
	if p.Engine != "vllm" || p.OperationID == "" {
		return newOpID(), nil
	}
	if !vllmPullOperationToken.MatchString(p.OperationID) {
		return "", errors.New("vLLM pull requires a 32-character lowercase hexadecimal operationId")
	}
	return p.OperationID, nil
}

// runRemote dispatches an engine:remote-* request. It runs on its own goroutine
// (like the other long ops) so the read loop stays responsive during a
// multi-minute remote install/pull.
func (m *Manager) runRemote(ctx context.Context, msg *Message) {
	var p remoteParam
	if !m.parse(msg, &p) {
		return
	}
	if p.Node == "" {
		m.codec.RespondError(msg.ID, -32602, "node is required")
		return
	}
	if msg.Method == "engine:remote-vllm-qwen38-prepare" {
		p.Engine = "vllm"
	}
	if msg.Method != "engine:remote-get-installed" && msg.Method != "engine:remote-cancel-pull" && !(msg.Method == "engine:remote-vllm-qwen38-prepare" && p.Cancel) && !(msg.Method == "engine:remote-distribute-model" && p.Cancel) {
		operation := "perform " + msg.Method + " for"
		if err := m.exec.rejectVLLMGroupMutation(p.Engine, operation); err != nil {
			m.codec.RespondError(msg.ID, -32000, err.Error())
			return
		}
	}
	peer, ok := m.peers.lookup(p.Node)
	if !ok {
		m.codec.RespondError(msg.ID, -32000, "node "+p.Node+" is not a discovered ec peer")
		return
	}
	client, err := m.remoteClient(ctx, peer)
	if err != nil {
		m.codec.RespondError(msg.ID, -32000, err.Error())
		return
	}

	switch msg.Method {
	case "engine:remote-vllm-qwen38-prepare":
		request := vllmQwen38PrepareRequest{OperationID: p.OperationID, Cancel: p.Cancel}
		if !vllmQwen38OperationID.MatchString(request.OperationID) {
			m.codec.RespondError(msg.ID, -32602, "Qwen3.8 preparation requires a 32-character lowercase hexadecimal operationId")
			return
		}
		terminal, err := client.stream(ctx, controlQwen38PreparePath, request, m.remoteProgressFn(request.OperationID, peer.nodeID))
		if err != nil {
			m.codec.RespondError(msg.ID, -32000, err.Error())
			return
		}
		var result vllmQwen38PrepareResult
		if len(terminal.Result) == 0 || json.Unmarshal(terminal.Result, &result) != nil {
			m.codec.RespondError(msg.ID, -32000, "remote Qwen3.8 preparation returned an invalid terminal result")
			return
		}
		m.codec.Respond(msg.ID, result)

	case "engine:remote-get-installed":
		res, err := client.getEngines(ctx)
		m.respondOrErr(msg, res, err)

	case "engine:remote-install":
		if p.Engine == "" {
			m.codec.RespondError(msg.ID, -32602, "engine is required")
			return
		}
		opID := newOpID()
		body := installRequest{OpID: opID, Engine: p.Engine, Start: p.Start}
		terminal, err := client.stream(ctx, controlInstallPath, body, m.remoteProgressFn(opID, peer.nodeID))
		if err != nil {
			m.codec.RespondError(msg.ID, -32000, err.Error())
			return
		}
		m.codec.Respond(msg.ID, map[string]any{"opId": opID, "status": terminal.Status})

	case "engine:remote-update":
		if p.Engine == "" {
			m.codec.RespondError(msg.ID, -32602, "engine is required")
			return
		}
		opID := newOpID()
		terminal, err := client.stream(ctx, controlUpdatePath, updateRequest{OpID: opID, Engine: p.Engine}, m.remoteProgressFn(opID, peer.nodeID))
		if err != nil {
			m.codec.RespondError(msg.ID, -32000, err.Error())
			return
		}
		m.codec.Respond(msg.ID, map[string]any{"opId": opID, "status": terminal.Status})

	case "engine:remote-pull-model":
		if p.Engine == "" {
			m.codec.RespondError(msg.ID, -32602, "engine is required")
			return
		}
		if p.Model == "" && len(p.Params) == 0 {
			m.codec.RespondError(msg.ID, -32602, "model or params is required")
			return
		}
		opID, err := remotePullOperationID(p)
		if err != nil {
			m.codec.RespondError(msg.ID, -32602, err.Error())
			return
		}
		body := pullRequest{OpID: opID, Engine: p.Engine, Model: p.Model, Params: p.Params}
		terminal, err := client.stream(ctx, controlPullPath, body, m.remoteProgressFn(opID, peer.nodeID))
		if err != nil {
			m.codec.RespondError(msg.ID, -32000, err.Error())
			return
		}
		m.codec.Respond(msg.ID, map[string]any{"opId": opID, "result": terminal.Result})

	case "engine:remote-distribute-model":
		if p.Engine != "vllm" || p.SourceNode == "" || p.SourceNode == p.Node || !vllmPullOperationToken.MatchString(p.OperationID) {
			m.codec.RespondError(msg.ID, -32602, "remote distribution requires distinct target/source nodes and exact vLLM model/operation identities")
			return
		}
		if _, _, err := parseExactVLLMHFModel(p.Model); err != nil {
			m.codec.RespondError(msg.ID, -32602, "remote distribution requires an exact immutable vLLM model")
			return
		}
		opID := newOpID()
		body := vllmDistributionRequest{OpID: opID, Engine: "vllm", OperationID: p.OperationID, SourceNode: p.SourceNode, Model: p.Model, Cancel: p.Cancel}
		release := func() error { return nil }
		owner := fabricTransferHold{OperationID: p.OperationID, Source: p.SourceNode, Target: p.Node, Model: p.Model}
		if p.Cancel && m.exec != nil && m.exec.fabric != nil {
			var bindErr error
			release, bindErr = m.exec.fabric.transferHoldRelease(owner)
			if bindErr != nil {
				m.codec.RespondError(msg.ID, -32000, bindErr.Error())
				return
			}
		} else if !p.Cancel {
			// This controller owns the fabric; the target only dials the lane it is given.
			address, done, routeErr := m.routeVLLMDistribution(ctx, p.SourceNode, p.Node, p.Model, p.OperationID)
			if routeErr != nil {
				m.codec.RespondError(msg.ID, -32000, routeErr.Error())
				return
			}
			body.SourceAddress, release = address, done
		}
		terminal, err := client.stream(ctx, controlVLLMReceivePath, body, m.remoteProgressFn(opID, peer.nodeID))
		err = m.settleRemoteVLLMDistribution(p, terminal, err, release)
		if err != nil {
			m.codec.RespondError(msg.ID, -32000, err.Error())
			return
		}
		m.codec.Respond(msg.ID, map[string]any{"opId": opID, "operationId": p.OperationID, "result": terminal.Result})

	case "engine:remote-load-model", "engine:remote-unload-model", "engine:remote-delete-model", "engine:remote-cancel-pull":
		if p.Engine == "" {
			m.codec.RespondError(msg.ID, -32602, "engine is required")
			return
		}
		if p.Model == "" {
			m.codec.RespondError(msg.ID, -32602, "model is required")
			return
		}
		if msg.Method == "engine:remote-cancel-pull" && p.Engine == "vllm" {
			if !vllmPullOperationToken.MatchString(p.OperationID) {
				m.codec.RespondError(msg.ID, -32602, "vllm, model and operationId are required")
				return
			}
			res, err := client.postJSON(ctx, controlCancelPullPath, "vllm", cancelPullRequest{Engine: "vllm", Model: p.Model, OperationID: p.OperationID})
			m.respondOrErr(msg, res, err)
			return
		}
		path := controlLoadPath
		switch msg.Method {
		case "engine:remote-unload-model":
			path = controlUnloadPath
		case "engine:remote-delete-model":
			path = controlDeletePath
		case "engine:remote-cancel-pull":
			path = controlCancelPullPath
		}
		res, err := client.postJSON(ctx, path, p.Engine, modelActionRequest{Engine: p.Engine, Model: p.Model})
		m.respondOrErr(msg, res, err)

	case "engine:remote-start":
		if p.Engine == "" {
			m.codec.RespondError(msg.ID, -32602, "engine is required")
			return
		}
		res, err := client.postJSON(ctx, controlStartPath, p.Engine, startRequest{Engine: p.Engine, Port: p.Port})
		m.respondOrErr(msg, res, err)

	case "engine:remote-stop":
		if p.Engine == "" {
			m.codec.RespondError(msg.ID, -32602, "engine is required")
			return
		}
		res, err := client.postJSON(ctx, controlStopPath, p.Engine, stopRequest{Engine: p.Engine})
		m.respondOrErr(msg, res, err)

	default:
		m.codec.RespondError(msg.ID, -32601, "method not found: "+msg.Method)
	}
}

// A transport failure does not prove the target stopped. A terminal error does:
// the peer sends it only after its receive worker returns. A later exact cancel
// can clear a hold left by an uncertain stream.
func (m *Manager) settleRemoteVLLMDistribution(p remoteParam, terminal streamFrame, streamErr error, release func() error) error {
	bound := terminal.OpID == p.OperationID && terminal.Engine == "vllm" && terminal.Op == "distribute"
	if streamErr != nil {
		if !p.Cancel && bound && terminal.Type == "error" {
			return errors.Join(streamErr, release())
		}
		return streamErr
	}
	if !bound || terminal.Type != "result" {
		return errors.New("remote distribution returned an invalid terminal operation")
	}
	if p.Cancel {
		if err := validateVLLMDistributionCancellation(terminal.Result, p.OperationID); err != nil {
			return err
		}
		return release()
	}
	var result vllmModel
	_, revision, _ := parseExactVLLMHFModel(p.Model)
	if json.Unmarshal(terminal.Result, &result) != nil || result.ID != p.Model || result.Revision != revision ||
		result.Source != "huggingface" || result.Bytes <= 0 || !validSHA256(result.Digest) || !validSHA256(result.MetadataSHA256) {
		return errors.New("remote distribution returned an invalid exact model receipt")
	}
	return release()
}

// remoteProgressFn returns an onProgress callback that relays a peer's stream
// frames up as engine:remote-progress notifications. Operation-aware peers keep
// their exact effect identity; older streams fall back to our correlation id.
func (m *Manager) remoteProgressFn(opID, node string) func(streamFrame) {
	return func(f streamFrame) {
		progressID := opID
		if f.Engine == "vllm" && (f.Op == "pull" || f.Op == "distribute") && vllmPullOperationToken.MatchString(f.OpID) {
			progressID = f.OpID
		}
		p := remoteProgress{
			OpID: progressID, Node: node, Engine: f.Engine, Op: f.Op,
			Stage: f.Stage, Message: f.Message, Network: f.Network,
		}
		if wirePercentIncluded(f.Percent) {
			p.Percent = f.Percent
		}
		_ = m.codec.Notify("engine:remote-progress", p)
	}
}
