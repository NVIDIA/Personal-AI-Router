// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { NodeStatuses } from '@/shared/constants/nodes'
import type { ModelAlias } from '@/shared/types/cluster'
import { SystemTopology } from '@/shared/types/hardware'
import { PlatformDisplayName } from '@/shared/types/platform'

export type NodeStatus = (typeof NodeStatuses)[number]

export interface NodeItem {
    id: string
    name: string
    status: NodeStatus
    ipAddress: string
    port: number
    allIpAddresses: string[]
    topology: SystemTopology
    os: PlatformDisplayName
    /**
     * That node's own cluster model alias document, as it announced it — the
     * list its owner edits, not the merged list this computer routes by. It is
     * announced by the node's engine manager, so it survives that node's
     * engines being stopped, and it is absent rather than empty when the node
     * has reported nothing (an older build, or a model read that has not
     * answered). The distinction matters because the editor behind it saves the
     * whole list it shows: it will replace a stated empty and never a silence.
     *
     * This computer's own list is not read from here — it comes from this node's
     * settings service, which is authoritative for it.
     */
    aliases?: ModelAlias[]
}
