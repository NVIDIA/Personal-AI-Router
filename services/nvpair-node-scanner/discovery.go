// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// These aliases expose the shared mDNS browser to the scanner. The daemon keys
// discoveries by discoveryRecordId and supplies authenticated peer availability
// checks. Scan timing and missed-scan limits use the shared defaults.

import "nvpair-shared/discovery"

type (
	RawNode        = discovery.Node
	DiscoveryEvent = discovery.Event
	Discovery      = discovery.Browser
)

// NewDiscovery builds the node browser with the shared defaults.
var NewDiscovery = discovery.New

// WithLivenessProbe lets the daemon retain a discovery using its verified peer's
// authenticated inventory or approved inference activity.
var WithLivenessProbe = discovery.WithLivenessProbe

// WithKeyFunc lets the daemon key discoveries by discoveryRecordId, independently
// of their claimed UUIDs. Distinct connection candidates remain separate.
var WithKeyFunc = discovery.WithKeyFunc

// UUIDFromTXT extracts the claimed host UUID, including for filtering this host's
// own announcements. It does not determine the discovery record ID.
var UUIDFromTXT = discovery.UUIDFromTXT

// sameStringSet re-exports the shared order-insensitive multiset compare so the
// daemon's model-inventory change detection (directory.applyModels / sameByEngine)
// uses one implementation rather than a local copy.
var sameStringSet = discovery.SameStringSet
