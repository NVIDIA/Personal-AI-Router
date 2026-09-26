// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package enginelogs defines the bounded Engine Manager log wire contract.
package enginelogs

const (
	// MaxLines is the largest retained per-engine snapshot.
	MaxLines = 2000
	// MaxLineBytes matches the managed-process stdout/stderr scanner ceiling.
	MaxLineBytes = 256 << 10
	// MaxSnapshotTextBytes caps the aggregate UTF-8 child-output text retained
	// in one snapshot. The frame cap below covers encoding/json's worst-case
	// string escaping plus line metadata and the JSON-RPC envelope.
	MaxSnapshotTextBytes = 1 << 20
	// MaxBrokerFrameBytes is the broker's Engine Manager stdout frame ceiling.
	// It matches Engine Manager's established 8 MiB JSON-RPC ceiling because
	// diagnostic responses can exceed 2 MiB; only this broker worker is widened.
	MaxBrokerFrameBytes = 8 << 20
)
