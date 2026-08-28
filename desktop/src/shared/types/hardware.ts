// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export interface GpuInfo {
    id: string
    name: string
    vramTotal: number // in bytes
}

export interface StorageInfo {
    name: string
    capacity: number // in bytes
}

export interface CPUInfo {
    model: string
    cores: number
    threads: number
}

export interface SystemTopology {
    cpu: CPUInfo
    gpus: GpuInfo[]
    ram: number
    storage: StorageInfo[]
    // Backend-provided list of inference-ready hardware ids (node-level). The
    // node charts show exactly the GPUs whose id is in this list and do NO
    // client-side classification. When absent (the backend does not report
    // readiness yet) the UI shows all GPUs. See the routing limitations in
    // docs/services-parity.md.
    inferenceHardwareIds?: string[]
}
