// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"nvpair-shared/cableprobe"
)

const cableReviewBlocker = "Passive port review only. Current-run cable probing and one-shot raw-network permission approval are not implemented. No cable or RDMA correctness is established."

func validateCableSelection(p cableprobe.ReviewRequest) bool {
	if len(p.NodeIDs) < 2 || len(p.NodeIDs) > 3 || len(p.Ports) > 6 {
		return false
	}
	counts, ports := map[string]int{}, map[cableprobe.PortRef]bool{}
	for _, id := range p.NodeIDs {
		if !cableIdentifier(id, 128) {
			return false
		}
		if _, exists := counts[id]; exists {
			return false
		}
		counts[id] = 0
	}
	for _, port := range p.Ports {
		if _, exists := counts[port.NodeID]; !exists {
			return false
		}
		if !cableIdentifier(port.SwitchID, 128) || !cableIdentifier(port.PortName, 128) || ports[port] {
			return false
		}
		ports[port] = true
		counts[port.NodeID]++
	}
	for _, count := range counts {
		if count < 1 || count > 2 {
			return false
		}
	}
	return true
}

func (m *Manager) runCableReview(ctx context.Context, msg *Message) {
	var request cableProductReviewRequest
	decoder := json.NewDecoder(bytes.NewReader(msg.Params))
	decoder.DisallowUnknownFields()
	if len(msg.Params) > 4096 || decoder.Decode(&request) != nil || !validateCableSelection(request.ReviewRequest) {
		m.codec.RespondError(msg.ID, -32602, "Select two or three distinct nodes and one or two native physical ports per node.")
		return
	}
	if decoder.Decode(new(any)) != io.EOF {
		m.codec.RespondError(msg.ID, -32602, "A single cable selection is required.")
		return
	}
	result, err := m.cables.review(ctx, request)
	m.respondOrErr(msg, result, err)
}

// No approval token or runnable state is stored. Every review is read-only and
// unavailable until the participant adapter and finite lifecycle are supported.
func (m *Manager) reviewCables(ctx context.Context, selection cableprobe.ReviewRequest) (cableprobe.Review, error) {
	result, _, err := m.reviewCablesDetailed(ctx, selection)
	return result, err
}

func (m *Manager) reviewCablesDetailed(ctx context.Context, selection cableprobe.ReviewRequest) (cableprobe.Review, []cableFactsReadStatus, error) {
	if !validateCableSelection(selection) || m.cableLocal == nil || !cableIdentifier(m.cableLocal.nodeID, 128) {
		return cableprobe.Review{}, nil, errors.New("valid selection and parent-owned host identity are required for cable review")
	}
	began := time.Now()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result := cableprobe.Review{ReviewID: "cable-review-" + rand.Text(), OwnerNodeID: m.cableLocal.nodeID,
		Targets: []cableprobe.Target{}, Available: false, Reason: cableReviewBlocker}
	m.mesh.Refresh()
	ownerPrincipal := m.mesh.NodeUUID()
	// Client reuses its pointer only while both public certificates match the
	// shared trust owner. Rotation/repinning creates a new generation, even when
	// the principal and address are unchanged. Merely holding this opens no socket.
	ownerClient, _ := m.remoteHTTP.Client(ownerPrincipal)
	resolved, principals := map[string]ecPeer{}, map[string]bool{}
	factsReads := make([]struct {
		until   time.Time
		client  *http.Client
		sampled bool
	}, len(selection.NodeIDs))
	statuses := make([]cableFactsReadStatus, len(selection.NodeIDs))
	for i := range statuses {
		statuses[i] = "unavailable"
	}
	for _, nodeID := range selection.NodeIDs {
		target := cableprobe.Target{NodeID: nodeID, Ports: []cableprobe.Port{}, RawPrivilege: "unknown", Reason: "Node is not a discovered, pinned cluster participant."}
		peer, known := m.peers.lookup(nodeID)
		if nodeID == result.OwnerNodeID {
			peer, known = ecPeer{nodeID: nodeID, clusterUUID: ownerPrincipal}, true
		}
		if known && m.mesh.Clustered() && cableIdentifier(peer.clusterUUID, 256) && m.mesh.HasPin(peer.clusterUUID) {
			if principals[peer.clusterUUID] || (nodeID != result.OwnerNodeID && peer.clusterUUID == ownerPrincipal) {
				return cableprobe.Review{}, nil, errors.New("distinct selected host identities must not share a paired principal")
			}
			principals[peer.clusterUUID] = true
			resolved[nodeID] = peer
		}
		result.Targets = append(result.Targets, target)
	}
	// At most three independent reads share the existing deadline. Each owns
	// its ordered result slot; all freshness and trust checks still run after
	// the reads join, without spending earlier leases on serial peer latency.
	var reads sync.WaitGroup
	for i := range result.Targets {
		target := &result.Targets[i]
		peer, known := resolved[target.NodeID]
		if !known {
			continue
		}
		reads.Go(func() {
			sampled := &factsReads[i]
			readBegan := time.Now()
			readCtx, readCancel := context.WithTimeout(ctx, 3*time.Second)
			// An omitted/null peer age must not default to a fresh zero-age snapshot.
			facts := cablePortSnapshot{AgeMs: -1}
			var err error
			if target.NodeID == result.OwnerNodeID {
				facts, err = m.cableLocal.snapshot(readCtx, m.mesh)
				sampled.client, sampled.sampled = ownerClient, true
			} else {
				var client *remoteClient
				client, err = m.remoteClient(readCtx, peer)
				if err == nil {
					sampled.client, sampled.sampled = client.http, true
					err = readCableJSON(readCtx, client.http, client.base+controlCablePortsPath, &facts)
					if err != nil {
						client.forgetAddress()
					}
				}
			}
			readCancel()
			if err != nil {
				// Transport errors may include peer data. Only fixed local reasons go to UI.
				target.Reason = cableFactsMessage(err)
				statuses[i] = cableFactsReadClassification(err)
				return
			}
			ports, err := selectCablePorts(facts, target.NodeID, peer.clusterUUID, selection.Ports, time.Since(readBegan))
			if err != nil {
				target.Reason = err.Error()
				statuses[i] = cableFactsReadClassification(err)
				return
			}
			target.Principal, target.Ports, target.Reason = peer.clusterUUID, ports, cableReviewBlocker
			statuses[i] = ""
			sampled.until = readBegan.Add(time.Duration(2000-facts.AgeMs) * time.Millisecond)
		})
	}
	reads.Wait()
	// Discovery and pairing may change while another participant is being read.
	// Retain no identity/port claims from a view that has since been withdrawn.
	m.mesh.Refresh()
	currentOwnerClient, ownerPinned := m.remoteHTTP.Client(ownerPrincipal)
	ownerUnchanged := ownerPinned && ownerClient != nil && currentOwnerClient == ownerClient
	for i := range result.Targets {
		target := &result.Targets[i]
		peer, known := resolved[target.NodeID]
		if !known {
			continue
		}
		current, found := m.peers.lookup(target.NodeID)
		currentClient, pinned := m.remoteHTTP.Client(peer.clusterUUID)
		collected := factsReads[i]
		certificateUnchanged := pinned && (!collected.sampled || (collected.client != nil && currentClient == collected.client))
		unchanged := target.NodeID == result.OwnerNodeID || (found && current.clusterUUID == peer.clusterUUID &&
			current.port == peer.port && slices.Equal(current.addresses, peer.addresses))
		if !m.mesh.Clustered() || m.mesh.NodeUUID() != ownerPrincipal || !ownerUnchanged || !certificateUnchanged || !unchanged {
			target.Principal, target.Ports, target.Reason = "", []cableprobe.Port{}, "Pairing or participant identity changed during the review; review again."
			statuses[i] = "binding-changed"
		} else if !collected.until.IsZero() && !time.Now().Before(collected.until) {
			target.Ports, target.Reason = []cableprobe.Port{}, "Port facts expired while other participants were being read; review again."
			statuses[i] = "expired"
		}
	}
	result.RemainingMs = max(0, (cableprobe.ReviewLifetime - time.Since(began)).Milliseconds())
	return result, statuses, nil
}

func selectCablePorts(facts cablePortSnapshot, nodeID, principal string, selected []cableprobe.PortRef, transit time.Duration) ([]cableprobe.Port, error) {
	invalid := classifyCableFactsRead("invalid", errors.New("Current complete port facts did not match the selected paired participant."))
	if facts.NodeID != nodeID || facts.Principal != principal || facts.ObservedAt <= 0 || facts.AgeMs < 0 ||
		transit < 0 || facts.Ports == nil || len(facts.Ports) > 128 {
		return nil, invalid
	}
	if facts.AgeMs >= 2000 || transit.Milliseconds() >= 2000-facts.AgeMs {
		return nil, classifyCableFactsRead("stale", errors.New("Current complete port facts did not match the selected paired participant."))
	}
	known := map[[2]string]cableprobe.Port{}
	indexes, names := map[int]bool{}, map[string]bool{}
	for _, port := range facts.Ports {
		key := [2]string{port.SwitchID, port.PortName}
		if _, exists := known[key]; exists {
			return nil, invalid
		}
		if !cableIdentifier(port.SwitchID, 128) || !cableIdentifier(port.PortName, 128) || len(port.Interfaces) < 1 || len(port.Interfaces) > 4 {
			return nil, invalid
		}
		for _, alias := range port.Interfaces {
			if !cableInterfaceValid(alias) || indexes[alias.Index] || names[alias.Name] {
				return nil, invalid
			}
			indexes[alias.Index], names[alias.Name] = true, true
		}
		known[key] = port
	}
	result := []cableprobe.Port{}
	for _, ref := range selected {
		if ref.NodeID != nodeID {
			continue
		}
		port, exists := known[[2]string{ref.SwitchID, ref.PortName}]
		if !exists {
			return nil, classifyCableFactsRead("port-unavailable", errors.New("A selected native physical port is missing or unsupported on this participant."))
		}
		result = append(result, port)
	}
	return result, nil
}
