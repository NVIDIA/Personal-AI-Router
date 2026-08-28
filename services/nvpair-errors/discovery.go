// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Cross-node error sync runs no mDNS browse of its own:
// peers arrive from the broker's discovery relay as a discovery:nodes snapshot,
// which the Manager diffs into these shapes and feeds to PeerSync. The shared
// shapes are kept as local aliases so peersync.go / manager.go read naturally.

import "nvpair-shared/discovery"

type (
	RawNode        = discovery.Node
	DiscoveryEvent = discovery.Event
)
