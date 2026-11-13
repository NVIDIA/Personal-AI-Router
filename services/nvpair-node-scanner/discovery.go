// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// These aliases expose the shared mDNS browser to the scanner. The daemon keys
// discoveries by discoveryRecordId. Announcement expiry uses the shared scan
// timing and missed-scan limits, independently of authenticated peer availability.

import "nvpair-shared/discovery"

type (
	RawNode        = discovery.Node
	DiscoveryEvent = discovery.Event
	Discovery      = discovery.Browser
)

// NewDiscovery builds the node browser with the shared defaults.
var NewDiscovery = discovery.New

// WithKeyFunc lets the daemon key discoveries by discoveryRecordId, independently
// of their claimed UUIDs. Distinct connection candidates remain separate.
var WithKeyFunc = discovery.WithKeyFunc

// sameStringSet re-exports the shared order-insensitive multiset compare so the
// daemon's model-inventory change detection (directory.applyModels / sameByEngine)
// uses one implementation rather than a local copy.
var sameStringSet = discovery.SameStringSet
