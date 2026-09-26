// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Selecting the retained model a PAIR-managed vLLM will serve on its next
 * owned start. The request names one canonical model id that is already in
 * PAIR's downloaded library for the target node; never a path, URL or command.
 */
export interface VllmModelSelectionRequest {
    nodeId: string
    /** Canonical retained id: owner/model@revision, or local:<sha256>. */
    model: string
}

export interface VllmModelSelectionResult {
    nodeId: string
    model: string
    /** The selection PAIR reports in the refreshed engine status after the call. */
    selectedModel: string
}
