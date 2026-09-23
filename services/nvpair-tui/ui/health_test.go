// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"testing"

	"nvpair-shared/engines"
	svcerrors "nvpair-shared/errors"
)

// The Overview's proxy row must key on the identity the broker actually stamps
// on a proxy crash.
//
// One nvpair-proxy process hosts every engine's facade under one supervisor, so
// the broker reports one crash for the process. Keying on the per-engine
// ComponentName was silent in both directions: the real crash entry matched no
// row, so a dead proxy was invisible in the one view meant to show worker
// liveness, and the per-engine rows could never leave "ok". Nothing else in the
// build catches that — the mismatch compiles and every other test passes — so
// this assertion is what makes the identity rule in nvpair-shared/engines
// enforceable rather than advisory.
func TestHealthProxyRowMatchesTheBrokerCrashIdentity(t *testing.T) {
	found := false
	for _, w := range healthWorkers {
		if w == engines.ProxyComponent {
			found = true
		}
		for _, e := range engines.All() {
			if w == e.ComponentName() {
				t.Errorf("health row %q keys on a per-facade identity; the broker reports proxy crashes against %q",
					w, engines.ProxyComponent)
			}
		}
	}
	if !found {
		t.Fatalf("no health row keys on %q, so a proxy crash would have no row at all: %v",
			engines.ProxyComponent, healthWorkers)
	}

	// End to end through the real matcher, with the id the broker builds.
	v := newHealthView(nil)
	v.localNodeUUID = "self-uuid"
	v.rebuildCrashes([]svcerrors.ServiceError{{
		ID:      crashPrefix + engines.ProxyComponent,
		Message: "proxy crashed",
		NodeID:  "self-uuid",
	}})
	if _, down := v.crashed[engines.ProxyComponent]; !down {
		t.Fatalf("a proxy crash did not register: %v", v.crashed)
	}
}

// TestHealthRebuildCrashesFiltersByUUID: the Overview
// keeps only local-origin crashes, keyed on this host's stable UUID (the value
// the broker stamps on local reports). A peer's crash must be dropped, and a
// local UUID-stamped crash must NOT be misclassified as remote.
func TestHealthRebuildCrashesFiltersByUUID(t *testing.T) {
	v := newHealthView(nil)
	v.localNodeUUID = "self-uuid"

	crash := func(worker, nodeID string) svcerrors.ServiceError {
		return svcerrors.ServiceError{ID: crashPrefix + worker, Message: worker + " crashed", NodeID: nodeID}
	}
	v.rebuildCrashes([]svcerrors.ServiceError{
		crash("scanner", "self-uuid"),      // local crash — keep
		crash("ollama-proxy", "peer-uuid"), // a peer's crash — drop
	})

	if _, down := v.crashed["scanner"]; !down {
		t.Fatal("local UUID-stamped crash should be surfaced, not filtered as remote")
	}
	if _, down := v.crashed["ollama-proxy"]; down {
		t.Fatal("a peer's crash must be filtered out of the local health view")
	}
}

// TestHealthRebuildCrashesBeforeIdentity: before the local UUID resolves, all
// crashes are kept (fail-open) so the view isn't blank during startup.
func TestHealthRebuildCrashesBeforeIdentity(t *testing.T) {
	v := newHealthView(nil)
	v.rebuildCrashes([]svcerrors.ServiceError{
		{ID: crashPrefix + "scanner", Message: "x", NodeID: "whatever-uuid"},
	})
	if _, down := v.crashed["scanner"]; !down {
		t.Fatal("crashes should be kept until the local UUID is known")
	}
}
