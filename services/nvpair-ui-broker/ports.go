// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "nvpair-shared/noderec"

// portBlock is the broker's resolved view of the six-port service block. It
// comes from --port-base once, at construction; production always gets the
// canonical block (noderec.DefaultPortBase). A shifted block is deployment
// plumbing for running a second, fully isolated broker (e.g. the cross-process
// test suite) beside a live one — installer firewall rules and desktop pairing
// stay on the canonical ports.
type portBlock struct {
	nodeInfoHTTP       int // ni: node-info public HTTP + pinned peer inventory
	errorsHTTP         int // er: cross-node errors endpoint
	workloadHTTP       int // wl: inter-node workload relay
	clusterManagerHTTP int // cl: cluster-manager inter-node listener
	engineManagerHTTP  int // em: engine-manager LAN /v1/models surface
	engineControl      int // ec: engine-manager cluster mTLS remote control
}

// portsForBase is infallible: main validates the flag with
// noderec.ValidatePortBase before any broker is constructed, so the block is
// guaranteed to fit in the unprivileged port space.
func portsForBase(base int) portBlock {
	port := func(key noderec.ServiceKey) int {
		p, ok := noderec.ServicePort(base, key)
		if !ok {
			panic("portsForBase: " + string(key) + " is not a block service")
		}
		return p
	}
	return portBlock{
		nodeInfoHTTP:       port(noderec.ServiceNodeInfo),
		errorsHTTP:         port(noderec.ServiceErrors),
		workloadHTTP:       port(noderec.ServiceWorkload),
		clusterManagerHTTP: port(noderec.ServiceCluster),
		engineManagerHTTP:  port(noderec.ServiceEngineManager),
		engineControl:      port(noderec.ServiceEngineControl),
	}
}

// ports resolves the service block this broker hands its workers. A zero
// portBase — the shipped binary's default, and every in-process Broker that
// states no preference (including the bare &Broker{} test literals) — is the
// canonical block, so no consumer can ever see a half-built block.
func (b *Broker) ports() portBlock {
	return portsForBase(b.resolvedPortBase())
}

// resolvedPortBase is ports() without the block math, for callers that report
// the base itself.
func (b *Broker) resolvedPortBase() int {
	if b.portBase == 0 {
		return noderec.DefaultPortBase
	}
	return b.portBase
}

// all lists the block's ports in noderec's TXT emit order. It is the set the
// broker treats as its own reserved block when checking engine port requests.
func (p portBlock) all() []int {
	return []int{
		p.nodeInfoHTTP, p.errorsHTTP, p.workloadHTTP,
		p.clusterManagerHTTP, p.engineManagerHTTP, p.engineControl,
	}
}
