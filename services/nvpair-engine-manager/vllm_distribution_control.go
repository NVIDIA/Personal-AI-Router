// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	controlVLLMExportPlanPath  = "/v1/models/distribution/export-plan"
	controlVLLMExportChunkPath = "/v1/models/distribution/export-chunk"
	controlVLLMReceivePath     = "/v1/models/distribution/receive"
)

func decodeVLLMDistributionJSON(w http.ResponseWriter, r *http.Request, out any) error {
	reader := http.MaxBytesReader(w, r.Body, maxControlBody)
	defer reader.Close()
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}

func (s *controlServer) handleVLLMExportPlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req vllmDistributionPlanRequest
	if err := decodeVLLMDistributionJSON(w, r, &req); err != nil {
		http.Error(w, "invalid vLLM export-plan request", http.StatusBadRequest)
		return
	}
	plan, err := s.exec.exportVLLMDistributionPlan(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(plan)
}

func (s *controlServer) handleVLLMExportChunk(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req vllmDistributionChunkRequest
	if err := decodeVLLMDistributionJSON(w, r, &req); err != nil {
		http.Error(w, "invalid vLLM export-chunk request", http.StatusBadRequest)
		return
	}
	chunk, err := s.exec.exportVLLMDistributionChunk(r.Context(), req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-NVPAIR-Chunk-SHA256", chunk.SHA256)
	w.Header().Set("X-NVPAIR-Chunk-Offset", strconv.FormatInt(chunk.Offset, 10))
	w.Header().Set("X-NVPAIR-Chunk-Length", strconv.Itoa(len(chunk.Data)))
	w.Header().Set("Content-Length", strconv.Itoa(len(chunk.Data)))
	_, _ = w.Write(chunk.Data)
}

func (s *controlServer) handleVLLMReceive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.manager == nil {
		http.Error(w, "remote distribution unavailable", http.StatusServiceUnavailable)
		return
	}
	var req vllmDistributionRequest
	if err := decodeVLLMDistributionJSON(w, r, &req); err != nil || validateVLLMDistributionRequest(req) != nil {
		http.Error(w, "invalid vLLM distribution request", http.StatusBadRequest)
		return
	}
	s.streamOp(w, r, req.OperationID, "vllm", "distribute", func(ctx context.Context) (streamFrame, error) {
		result, err := s.manager.receiveVLLMDistribution(ctx, req)
		return streamFrame{Type: "result", OpID: req.OperationID, Engine: "vllm", Op: "distribute", Result: result}, err
	})
}

type remoteVLLMDistributionSource struct{ client *remoteClient }

func (s remoteVLLMDistributionSource) exportPlan(ctx context.Context, req vllmDistributionPlanRequest) (vllmDistributionPlan, error) {
	return s.client.vllmExportPlan(ctx, req)
}

func (s remoteVLLMDistributionSource) exportChunk(ctx context.Context, req vllmDistributionChunkRequest) (vllmDistributionChunk, error) {
	return s.client.vllmExportChunk(ctx, req)
}

func (m *Manager) receiveVLLMDistribution(ctx context.Context, req vllmDistributionRequest) (json.RawMessage, error) {
	if err := validateVLLMDistributionRequest(req); err != nil {
		return nil, err
	}
	if req.Cancel {
		return m.exec.cancelVLLMDistribution(ctx, req)
	}
	if req.SourceNode == m.exec.vllmNodeID {
		return nil, errors.New("vLLM distribution source and target must be different nodes")
	}
	peer, ok := m.peers.lookup(req.SourceNode)
	if !ok {
		return nil, fmt.Errorf("source node %s is not a discovered ec peer", req.SourceNode)
	}
	var client *remoteClient
	var err error
	if req.SourceAddress != "" {
		// The pinned mTLS identity, not the address, authenticates the source.
		client, err = m.remoteClientVia(peer, req.SourceAddress)
	} else {
		client, err = m.remoteClient(ctx, peer)
	}
	if err != nil {
		return nil, err
	}
	return m.exec.receiveVLLMDistribution(ctx, remoteVLLMDistributionSource{client: client}, req)
}

// routeVLLMDistribution holds the fabric lane between source and target for one
// copy, or reports that the pair shares no fabric and the copy uses the
// management network. release must run once the copy ends.
func (m *Manager) routeVLLMDistribution(ctx context.Context, source, target, model, operationID string) (string, func() error, error) {
	if m.exec == nil || m.exec.fabric == nil {
		return "", func() error { return nil }, nil
	}
	lane, err := m.exec.fabric.laneBetween(ctx, source, target)
	if errors.Is(err, errNoFabricLane) {
		return "", func() error { return nil }, nil
	}
	if err != nil {
		return "", nil, err
	}
	holdID := newOpID()
	owner := fabricTransferHold{OperationID: operationID, Source: source, Target: target, Model: model}
	if err := m.exec.fabric.acquireTransferHold(ctx, lane.OperationID, lane.Qualification, holdID, owner); err != nil {
		return "", nil, err
	}
	return lane.SourceAddress, func() error { return m.exec.fabric.releaseTransferHold(lane.OperationID, holdID, owner) }, nil
}

// This node owns the fabric, so it routes a copy into itself.
func (m *Manager) distributeVLLMModelHere(ctx context.Context, req vllmDistributionRequest) (json.RawMessage, error) {
	if req.SourceAddress != "" {
		return nil, errors.New("the Engine Manager chooses the copy route; sourceAddress is not a request field")
	}
	if req.Cancel {
		release := func() error { return nil }
		if m.exec != nil && m.exec.fabric != nil {
			var err error
			release, err = m.exec.fabric.transferHoldRelease(fabricTransferHold{OperationID: req.OperationID, Source: req.SourceNode, Target: m.exec.vllmNodeID, Model: req.Model})
			if err != nil {
				return nil, err
			}
		}
		result, err := m.receiveVLLMDistribution(ctx, req)
		if err != nil {
			return nil, err
		}
		if err := validateVLLMDistributionCancellation(result, req.OperationID); err != nil {
			return nil, err
		}
		return result, release()
	}
	address, release, err := m.routeVLLMDistribution(ctx, req.SourceNode, m.exec.vllmNodeID, req.Model, req.OperationID)
	if err != nil {
		return nil, err
	}
	req.SourceAddress = address
	result, receiveErr := m.receiveVLLMDistribution(ctx, req)
	return result, errors.Join(receiveErr, release())
}

func validateVLLMDistributionCancellation(raw json.RawMessage, operationID string) error {
	var result struct {
		OperationID string `json:"operationId"`
		Cancelled   bool   `json:"cancelled"`
		Absent      bool   `json:"absent"`
	}
	if json.Unmarshal(raw, &result) != nil || result.OperationID != operationID || !result.Cancelled && !result.Absent {
		return errors.New("vLLM distribution cancellation did not confirm closure")
	}
	return nil
}

func (c *remoteClient) vllmExportPlan(ctx context.Context, req vllmDistributionPlanRequest) (vllmDistributionPlan, error) {
	var plan vllmDistributionPlan
	if c.readyHTTP == nil {
		return plan, errors.New("vLLM export-plan transport is unavailable")
	}
	// The source hashes the full retained model before sending response headers.
	// Keep that verification within the pinned long-operation header budget.
	inspection := *c
	inspection.http = c.readyHTTP
	raw, err := inspection.postJSONWithTimeout(ctx, controlVLLMExportPlanPath, "vllm", req, remoteActionTimeout)
	if err != nil {
		return plan, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&plan) != nil || decoder.Decode(new(any)) != io.EOF {
		return plan, errors.New("source returned an invalid vLLM distribution plan")
	}
	if err := validateVLLMDistributionPlan(plan, req); err != nil {
		return plan, err
	}
	return plan, nil
}

func (c *remoteClient) vllmExportChunk(ctx context.Context, req vllmDistributionChunkRequest) (vllmDistributionChunk, error) {
	var chunk vllmDistributionChunk
	raw, err := json.Marshal(req)
	if err != nil {
		return chunk, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+controlVLLMExportChunkPath, bytes.NewReader(raw))
	if err != nil {
		return chunk, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	response, err := remoteDo(c.http, httpReq)
	if err != nil {
		c.forgetAddress()
		return chunk, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := readRemoteBody(response.Body, maxRemoteResponseBytes)
		return chunk, fmt.Errorf("remote vLLM export chunk: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	length, parseErr := strconv.ParseInt(response.Header.Get("X-NVPAIR-Chunk-Length"), 10, 64)
	offset, offsetErr := strconv.ParseInt(response.Header.Get("X-NVPAIR-Chunk-Offset"), 10, 64)
	digest := response.Header.Get("X-NVPAIR-Chunk-SHA256")
	if parseErr != nil || offsetErr != nil || length != req.Length || offset != req.Offset || !validSHA256(digest) || length <= 0 || length > vllmDistributionChunkSize {
		return chunk, errors.New("remote vLLM chunk headers are invalid")
	}
	data, readErr := io.ReadAll(io.LimitReader(response.Body, vllmDistributionChunkSize+1))
	if readErr != nil || int64(len(data)) != length {
		return chunk, errors.New("remote vLLM chunk body is incomplete")
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != digest {
		return chunk, errors.New("remote vLLM chunk digest changed")
	}
	return vllmDistributionChunk{Offset: offset, Data: data, SHA256: digest}, nil
}
