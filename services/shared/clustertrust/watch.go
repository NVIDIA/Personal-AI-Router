// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clustertrust

import (
	"context"
	"log/slog"
	"time"
)

// RefreshInterval is the cadence at which a cluster-scoped service re-derives
// its membership from the cluster dir. It bounds how long a service can lag a
// create/join/leave, so it is deliberately short.
//
// One Refresh is the pin-set reload these services already performed per request
// (a trusted/ directory read plus one read per pin file), plus two small reads:
// node.crt and admission.json. The certificate is only re-parsed when its bytes
// change, so the steady-state cost is I/O against warm page cache rather than
// X.509 work. It is not free, though: the proxies call Refresh while resolving
// candidates, which puts those two extra reads on the inference request path, so
// if that ever shows up in a profile the fix is to coalesce refreshes within a
// short window rather than to lengthen this interval — a stale membership answer
// is the failure this package exists to prevent.
//
// This poll is the single mechanism by which a service notices a membership
// change. There is intentionally no second push-based path: the service that
// writes the cluster dir (nvpair-cluster-manager) is a sibling process, so any
// notification it sent would be one more thing that can be missed or arrive
// before the write lands — which is the failure this design removes. Convergence
// here depends only on the durable state on disk.
const RefreshInterval = 2 * time.Second

// Watch re-derives membership every RefreshInterval until ctx is done, calling
// onChange with the new answer whenever it flips. onChange runs on the watch
// goroutine and must not block; it may be nil for a service that only needs its
// gates to follow membership (every gate reads live state, so most services need
// no callback at all).
//
// A service starts this once and thereafter binds its listeners and builds its
// peer URLs from the live Mesh, so a node that starts unclustered and joins a
// cluster — in either order relative to the cluster-manager writing the dir —
// converges within one interval instead of needing a process restart.
//
// The transition is detected against this watcher's own last-seen answer rather
// than a value Refresh hands back. Every gate in every service refreshes the
// same Mesh on its own schedule, so a shared one-shot "did it change?" is
// consumed by whichever caller happens to run first — which is how a scanner
// that refreshes from four other paths silently never observed its own node
// joining a cluster. A private baseline cannot be stolen.
func (m *Mesh) Watch(ctx context.Context, onChange func(clustered bool)) {
	if m == nil || m.dir == "" {
		return
	}
	ticker := time.NewTicker(RefreshInterval)
	defer ticker.Stop()
	last := m.Clustered()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Refresh()
			clustered := m.Clustered()
			if clustered == last {
				continue
			}
			last = clustered
			slog.Info("cluster membership changed",
				"clustered", clustered, "clusterUuid", m.NodeUUID(), "pinnedPeers", m.PeerCount())
			if onChange != nil {
				onChange(clustered)
			}
		}
	}
}
