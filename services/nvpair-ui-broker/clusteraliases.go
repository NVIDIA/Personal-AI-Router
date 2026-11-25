// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"nvpair-shared/noderec"
)

// The broker carries configured cluster model aliases from the settings
// worker to the two consumers that act on them. It is a courier, not a
// compiler: the settings service owns persistence and validation, the proxy's
// cluster router owns alias semantics, and the broker only makes sure what
// was stored arrives.
//
// Two destinations, because a running proxy and this node's own announcement
// learn aliases from nothing else:
//
//   - the proxy's cluster router, over cluster/set-aliases — that is how
//     this node routes alias names;
//   - engine-manager's LAN surface, over node/model-aliases — engine-manager
//     republishes the list on /v1/models, where a peer's scanner picks it up
//     under the same pinned-mTLS gate as the model inventory and files it on
//     this node's directory entry. That is how an alias configured here
//     reaches the routers on other nodes.
//
// And two deliveries each, because a consumer that starts after the last edit
// misses nothing else and must miss nothing here either: the settings list is
// read once at each consumer's bring-up (covering cold start and every
// respawn), and each connection/cluster-model-aliases push is forwarded on
// change. Without the first, a consumer started after the last edit would act
// on a stale list until someone re-saved it; without the second, an edit
// would not propagate until the next restart. Neither delivery is gated on
// the broker's spawn-time cluster-router preference: where a cluster endpoint
// came from — started with it, or lifted at runtime over
// router/set-enabled — is not the courier's business, and an endpoint lifted
// at runtime is served by these same deliveries.

// modelAliasesPayload is the alias list on the wire at both ends of the courier
// step: what the settings worker answers or pushes, and what both consumers
// decode. A synonym is two names — what a caller may say and which model that
// is — and both ends spell them the same way, so what follows checks a payload
// rather than translating one.
type modelAliasesPayload struct {
	Aliases []noderec.ModelAlias `json:"aliases"`
}

// aliasesToForwardedWire checks a couriered alias list and hands on a copy of
// it, delivered whole or not at all.
//
// The check is not ceremony. A document written for another shape still decodes
// cleanly here: Go's decoder ignores an unknown "providers" key, so a
// provider-citing payload would arrive with an empty target on every row, and an
// empty target is not a broken alias, it is an inert one. The cluster would
// quietly stop routing names the operator still believes configured, which is
// the one failure this courier exists to exclude. A copied list is what keeps
// the same rule true on the way out: an alias means the same thing on every
// computer that hears it, so half a list is a different routing answer, not a
// partial one.
func aliasesToForwardedWire(raw json.RawMessage) (json.RawMessage, error) {
	var in modelAliasesPayload
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	for i, alias := range in.Aliases {
		if strings.TrimSpace(alias.Name) == "" {
			return nil, fmt.Errorf("aliases[%d]: name must not be empty", i)
		}
		if strings.TrimSpace(alias.Model) == "" {
			return nil, fmt.Errorf("aliases[%d] (%s): model must name the model this alias stands for", i, alias.Name)
		}
	}
	out := modelAliasesPayload{Aliases: make([]noderec.ModelAlias, len(in.Aliases))}
	copy(out.Aliases, in.Aliases)
	return json.Marshal(out)
}

// clusterModelAliasesPayload reads the configured alias list from the
// settings worker and translates it for delivery. The empty list is a real
// answer (nobody configured aliases), delivered as an empty list so both
// consumers clear any previously delivered set instead of keeping a stale one.
func (b *Broker) clusterModelAliasesPayload() (json.RawMessage, bool) {
	settings := b.getSettings()
	if settings == nil {
		slog.Warn("cluster model aliases not synced: settings worker unavailable")
		return nil, false
	}
	result, rpcErr, err := settings.Call(context.Background(), "settings/get-cluster-model-aliases", nil)
	if err != nil || rpcErr != nil {
		slog.Warn("failed to read cluster model aliases", "err", err, "rpcErr", rpcErr)
		return nil, false
	}
	payload, err := aliasesToForwardedWire(result)
	if err != nil {
		slog.Warn("cluster model aliases payload did not decode; not delivered", "err", err)
		return nil, false
	}
	return payload, true
}

// syncClusterModelAliasesToProxy delivers the configured alias list to a
// proxy's router. Called on every successful proxy bring-up and again when a
// relayed router/set-enabled write ends with the endpoint up: either way the
// router has just become able to route alias names and owns none until this
// lands, so an edit made while it could not route still arrives before that
// name has to be answered. As close as a separate process allows: the push
// lands after the bring-up, and the router serves from the catalog it builds
// per request, so the first alias use after this returns sees the configured
// list.
func (b *Broker) syncClusterModelAliasesToProxy(pp *proxyProcess) {
	payload, ok := b.clusterModelAliasesPayload()
	if !ok {
		return
	}
	if _, rpcErr, err := pp.Call(context.Background(), "cluster/set-aliases", payload); err != nil || rpcErr != nil {
		slog.Warn("failed to sync cluster model aliases to the router", "err", err, "rpcErr", rpcErr)
	}
}

// syncClusterModelAliasesToEngineManager delivers the configured alias list
// to a freshly spawned engine-manager, before anything can fetch /v1/models
// from it. spawnEngineMgr calls it on the same rule as the alias-reservation
// re-push above: a respawned engine-manager starts with an empty announcement,
// and a peer's scanner that fetched during the gap would otherwise serve this
// node's routers a directory entry without the aliases configured here.
func (b *Broker) syncClusterModelAliasesToEngineManager() {
	em := b.getEngineMgr()
	if em == nil {
		slog.Warn("cluster model aliases not announced: engine-manager unavailable")
		return
	}
	payload, ok := b.clusterModelAliasesPayload()
	if !ok {
		return
	}
	if err := em.Notify("node/model-aliases", json.RawMessage(payload)); err != nil {
		slog.Warn("failed to announce cluster model aliases through engine-manager", "err", err)
	}
}

// forwardClusterModelAliasesPush delivers a settings change push to the live
// consumers: the proxy's router (local routing picks the edit up on its next
// catalog rebuild) and engine-manager's announcement (peer scanners pick it up
// on their next model sweep of this node). Both handles are resolved at call
// time: this runs on the settings worker's notification goroutine, and a
// consumer respawn in flight will receive the same list from its own bring-up
// sync instead.
func (b *Broker) forwardClusterModelAliasesPush(params json.RawMessage) {
	payload, err := aliasesToForwardedWire(params)
	if err != nil {
		slog.Warn("cluster model aliases push did not decode; not delivered", "err", err)
		return
	}
	if em := b.getEngineMgr(); em != nil {
		if err := em.Notify("node/model-aliases", json.RawMessage(payload)); err != nil {
			slog.Debug("failed to forward cluster model aliases to engine-manager", "err", err)
		}
	}
	for _, profile := range engineProxyProfiles {
		pp := b.engineProxyHandle(profile)
		if pp == nil {
			continue
		}
		if _, rpcErr, err := pp.Call(context.Background(), "cluster/set-aliases", payload); err != nil || rpcErr != nil {
			slog.Warn("failed to forward cluster model aliases to the router", "err", err, "rpcErr", rpcErr)
		}
		return // one proxy process hosts every facade; the first handle found is the only one
	}
}
