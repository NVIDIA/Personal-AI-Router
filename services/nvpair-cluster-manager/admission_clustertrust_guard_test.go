// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"nvpair-shared/clustertrust"
)

// TestAdmissionReadSideMatchesWriter guards the admission schema that
// nvpair-shared/clustertrust duplicates from this package's admission_store.go.
// It drives the REAL writer (activateAdmission / clearAdmission) against the
// REAL reader (Mesh.Clustered) using the manager's own cluster dir, so a rename
// of admission.json or its clusterId/epoch JSON tags — or a change to the "active
// pair" semantics — fails HERE instead of silently making every node read as
// unclustered, which would resurrect the invite deadlock the read-side gate was
// added to fix.
//
// It also pins the read side's liveness: the same Mesh, opened once, must follow
// this writer's activate and clear. Every cluster-scoped service holds exactly
// such a long-lived Mesh, so a read side that could only answer at construction
// time would leave those services stuck in whatever mode they started in.
func TestAdmissionReadSideMatchesWriter(t *testing.T) {
	dir := t.TempDir()
	m := testManagerAt(t, dir, 15377)

	mesh := clustertrust.Open(m.clusterDir)
	activateTestCluster(t, m, "cluster-guard")

	mesh.Refresh()
	require.True(t, mesh.Clustered())

	// Teardown clears the active admission (keypair stays on disk). The read side
	// must now report the node as no longer a member.
	require.NoError(t, m.clearAdmission(), "clearAdmission")
	mesh.Refresh()
	require.False(t, mesh.Clustered())
}
