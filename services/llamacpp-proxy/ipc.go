// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The IPC transport (Unix socket / Windows named pipe) is single-sourced in
// nvpair-shared/ipc; the platform split lives there. dialIPC aliases it so call
// sites are unchanged.

import "nvpair-shared/ipc"

var dialIPC = ipc.Dial
