// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Per-service port overrides for a manually added node.
 *
 * The defaults `nvpair-manual-nodes` assumes describe a machine it cannot
 * introspect: a peer may run an engine on a second port, a whole node may be
 * reachable only through a forwarded range, and a test may put two nodes on one
 * loopback. An unset field keeps that service's default, so an entry overrides
 * only what the operator meant to.
 *
 * `vllm` is carried and persisted but not probed yet; it is here so an entry
 * written today keeps its meaning when that engine lands.
 */
export interface ManualServicePorts {
    nodeInfo?: number
    cluster?: number
    ollama?: number
    lmstudio?: number
    vllm?: number
}
