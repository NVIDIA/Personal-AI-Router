// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Exact pinned identities of the fixed Qwen3.8 serving-group profile. Both
 * values are wire constants shared with the Engine Manager plan validator;
 * they select a profile, they never grant one.
 */
export const VLLM_QWEN38_MODEL =
    'nvidia/Qwen3.8-Flash-Next-NVFP4@fc694b54fb0174e0913e6adf86691ef85a4ead47'
export const VLLM_QWEN38_RUNTIME = '0.28.1rc1.dev361+gd4d703caf'
