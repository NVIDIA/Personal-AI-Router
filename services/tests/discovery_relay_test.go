// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBroker_AdvertisesNodeRecord exercises the register->advertise path
// end to end: the broker spawns node-info and the promoted daemon, registers
// node-info's service broker-locally, and the daemon folds that into its
// registry and advertises ONE _nvpair-node record carrying the schema key and
// ni=<this run's block ni port>. This is the new consolidated discovery record
// (distinct from node-info's own transitional _nvpair-node-info advertisement).
//
// The ni= match doubles as the discriminator that separates this run's record
// from a live PAIR on the same host (#226): the hostname-named instance can
// belong to either node, but only this run's daemon advertises the allocated
// block base.
func TestBroker_AdvertisesNodeRecord(t *testing.T) {
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
	wantTXT := fmt.Sprintf("ni=%d", testPortBase)
	entryText := browseForTXT(t, "_nvpair-node._tcp", instance, 20*time.Second, wantTXT)
	require.NotNil(t, entryText, "no _nvpair-node record for this host advertising %s within timeout", wantTXT)
	txt := strings.Join(entryText, ";")
	assert.Contains(t, txt, wantTXT, "_nvpair-node TXT missing %s (register->advertise path) (%v)", wantTXT, entryText)
	assert.Contains(t, txt, "v=1", "_nvpair-node TXT missing schema v=1 (%v)", entryText)
}
