// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"nvpair-shared/discovery"
)

// TestBrokerAdvertisesNodeRecord exercises the register->advertise path
// end to end: the broker spawns node-info and the promoted daemon, registers
// node-info's service broker-locally, and the daemon folds that into its
// registry and advertises ONE _nvpair-node record carrying the schema key and
// ni=14318. This is the new consolidated discovery record (distinct from
// node-info's own transitional _nvpair-node-info advertisement).
func TestBrokerAdvertisesNodeRecord(t *testing.T) {
	_, msgs, stderr, cleanup := startBrokerWith(t, "--node-info-path", nodeInfoBin)
	t.Cleanup(cleanup)
	// Drain the broker's stdout/stderr so it doesn't stall on a full pipe while
	// we browse. Both channels close on cleanup, ending these goroutines.
	go func() {
		for range msgs {
		}
	}()
	go func() {
		for range stderr {
		}
	}()

	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	instance := strings.Trim(host, ".")

	// The scanner first announces the base node record, then re-announces after
	// node-info registers ni=. Startup ordering can therefore expose a valid
	// base record for a moment; poll the browser's current view until the
	// enriched update (with ni=14318) has folded in, rather than treating the
	// first packet as final.
	browser := discovery.New(nodeRecordService, testDomain,
		discovery.WithInterval(time.Second),
		discovery.WithScanTimeout(500*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go browser.Run(ctx, nil)

	deadline := time.Now().Add(20 * time.Second)
	var entry *discovery.Node
	for time.Now().Before(deadline) {
		nodes := browser.Nodes()
		for i := range nodes {
			if nodes[i].ID == instance {
				nn := nodes[i]
				entry = &nn
			}
		}
		if entry != nil && strings.Contains(strings.Join(entry.TXT, ";"), "ni=14318") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if entry == nil {
		t.Fatal("did not discover this node's _nvpair-node record within timeout")
	}
	txt := strings.Join(entry.TXT, ";")
	if !strings.Contains(txt, "ni=14318") {
		t.Errorf("_nvpair-node TXT missing ni=14318 (register->advertise path): %v", entry.TXT)
	}
	if !strings.Contains(txt, "v=1") {
		t.Errorf("_nvpair-node TXT missing schema v=1: %v", entry.TXT)
	}
}
