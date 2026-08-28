// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"testing"

	svcerrors "nvpair-shared/errors"
)

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
		crash("scanner", "self-uuid"), // local crash — keep
		crash("proxy", "peer-uuid"),   // a peer's crash — drop
	})

	if _, down := v.crashed["scanner"]; !down {
		t.Fatal("local UUID-stamped crash should be surfaced, not filtered as remote")
	}
	if _, down := v.crashed["proxy"]; down {
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
