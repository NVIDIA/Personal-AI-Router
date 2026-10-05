// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err, "hostname")
	instance := strings.Trim(host, ".")

	// The scanner first announces the base node record, then re-announces after
	// node-info registers ni=. Startup ordering can therefore expose a valid
	// base record for a moment; wait for the enriched update instead of treating
	// the first packet as final.
	var entryText []string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		entry := browseForInstance(t, "_nvpair-node._tcp", instance, 2*time.Second)
		if entry != nil {
			entryText = entry.Text
			if strings.Contains(strings.Join(entry.Text, ";"), "ni=14318") {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	entry := entryText
	require.NotNil(t, entry, "did not discover this node's _nvpair-node record within timeout")
	txt := strings.Join(entry, ";")
	assert.Contains(t, txt, "ni=14318", "_nvpair-node TXT missing ni=14318 (register->advertise path) (%v)", entry)
	assert.Contains(t, txt, "v=1", "_nvpair-node TXT missing schema v=1 (%v)", entry)
}
