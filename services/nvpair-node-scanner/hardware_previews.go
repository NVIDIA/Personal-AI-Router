// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"slices"
	"sync"
	"time"

	"nvpair-shared/noderec"
)

// refreshHardwarePreviewsLoop supplies display-only hardware for unverified
// discoveries. It never feeds the directory, authenticated liveness, or scheduler.
func (d *daemon) refreshHardwarePreviewsLoop(ctx context.Context) {
	ticker := time.NewTicker(telemetryRefreshInterval)
	defer ticker.Stop()
	sem := make(chan struct{}, noderec.HardwarePreviewConcurrency)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.dispatchHardwarePreviews(ctx, time.Now(), sem, &workers)
		}
	}
}

func (d *daemon) dispatchHardwarePreviews(ctx context.Context, now time.Time, sem chan struct{}, workers *sync.WaitGroup) {
	d.peerMu.Lock()
	defer d.peerMu.Unlock()
	for _, node := range d.dir.snapshot(noderec.ServiceNodeInfo) {
		if node.IdentityVerification != noderec.IdentityUnverified || node.DiscoveryRecordID == "" || len(node.CandidateIPs()) == 0 {
			continue
		}
		candidate := d.candidates[node.DiscoveryRecordID]
		if candidate == nil {
			continue
		}
		target := telemetryTargetKey(node.CandidateIPs(), node.Services[noderec.ServiceNodeInfo].Port) + "\x00" + node.HostUUID + "\x00" + node.ClusterUUID
		token, due := d.previewRetries.claim(node.DiscoveryRecordID, target, now)
		if !due {
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			ok := false
			defer func() {
				d.previewRetries.finish(node.DiscoveryRecordID, token, ok || ctx.Err() != nil, time.Now())
			}()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			ok = d.refreshHardwarePreview(ctx, node, candidate)
		}()
	}
}

// currentHardwarePreview must run under peerMu. The candidate pointer rejects
// results from a removed and rediscovered record, even if its endpoint is reused.
func (d *daemon) currentHardwarePreview(target noderec.DirectoryNode, candidate *discoveryCandidate) bool {
	current, present := d.dir.get(target.DiscoveryRecordID)
	return present && candidate != nil && target.IdentityVerification == noderec.IdentityUnverified &&
		d.candidates[target.DiscoveryRecordID] == candidate &&
		current.IdentityVerification == noderec.IdentityUnverified &&
		current.HostUUID == target.HostUUID && current.ClusterUUID == target.ClusterUUID &&
		current.Services[noderec.ServiceNodeInfo].Port == target.Services[noderec.ServiceNodeInfo].Port &&
		slices.Equal(current.CandidateIPs(), target.CandidateIPs())
}

func (d *daemon) refreshHardwarePreview(ctx context.Context, target noderec.DirectoryNode, candidate *discoveryCandidate) bool {
	d.peerMu.Lock()
	current := d.currentHardwarePreview(target, candidate)
	d.peerMu.Unlock()
	if !current {
		return false
	}

	type previewSample struct {
		info       noderec.NodeInfo
		receivedAt time.Time
	}
	// One deadline covers remembered-address failure and every alternative, so a
	// multihomed preview cannot occupy a worker beyond the display read budget.
	readCtx, cancel := context.WithTimeout(ctx, noderec.HardwarePreviewTimeout)
	defer cancel()
	ask := func(host string) (previewSample, bool) {
		info, ok := noderec.ReadHardwarePreview(readCtx, host, target.Services[noderec.ServiceNodeInfo].Port)
		return previewSample{info: info, receivedAt: time.Now()}, ok
	}
	key := hostKey{hostUUID: target.DiscoveryRecordID, service: noderec.ServiceNodeInfo}
	_, sample, fetched := askRemembered(&d.previewHosts, key, target.CandidateIPs(), ask)
	if !fetched {
		return false
	}
	d.peerMu.Lock()
	defer d.peerMu.Unlock()
	if ctx.Err() != nil || !d.currentHardwarePreview(target, candidate) {
		d.previewHosts.forget(key)
		return false
	}
	// The outer record describes an unverified announcement. Public response
	// identity and membership claims have already been stripped by the reader.
	target.ClusterUUID = ""
	d.emitNodeInfo(target, sample.info.Aged(time.Since(sample.receivedAt)))
	return true
}

func (d *daemon) forgetHardwarePreview(recordID string) {
	d.previewRetries.forget(recordID)
	d.previewHosts.forget(hostKey{hostUUID: recordID, service: noderec.ServiceNodeInfo})
}
