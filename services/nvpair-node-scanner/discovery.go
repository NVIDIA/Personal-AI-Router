// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The mDNS browser now lives in the shared nvpair-shared/discovery package (the
// mDNS dedup) — nvpair-node-scanner was the canonical source it was lifted from, so
// behavior is identical (5s scans, the shared miss threshold, order-insensitive
// compare, uuid= collision warning, evict at threshold). These aliases keep
// scanner.go and main.go on the local names. The order-insensitivity and
// UUIDFromTXT tests moved to the shared package.

import "nvpair-shared/discovery"

type (
	RawNode        = discovery.Node
	DiscoveryEvent = discovery.Event
	Discovery      = discovery.Browser
)

// NewDiscovery builds the node browser with the shared defaults.
var NewDiscovery = discovery.New

// WithLivenessProbe re-exports the shared option so the daemon can wire its
// TCP-probe-before-evict anti-flap guard — the guard the proxies
// used to run themselves before discovery was consolidated onto the daemon.
var WithLivenessProbe = discovery.WithLivenessProbe

// WithKeyFunc re-exports the shared option so the daemon keys its node map by
// uuid= rather than the mDNS instance name (matching cluster-manager). Two hosts
// that share a hostname but hold distinct UUIDs must not collapse into one entry.
var WithKeyFunc = discovery.WithKeyFunc

// UUIDFromTXT re-exports the shared uuid= extractor, used as the browser's key
// function (see WithKeyFunc).
var UUIDFromTXT = discovery.UUIDFromTXT

// sameStringSet re-exports the shared order-insensitive multiset compare so the
// daemon's model-inventory change detection (directory.applyModels / sameByEngine)
// uses one implementation rather than a local copy.
var sameStringSet = discovery.SameStringSet
