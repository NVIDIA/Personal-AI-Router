// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The newline-delimited JSON-RPC 2.0 codec is single-sourced in
// nvpair-shared/jsonrpc. These local aliases keep this package's call sites and
// tests unchanged after removing the copy-pasted per-service codec.

import "nvpair-shared/jsonrpc"

type (
	Message  = jsonrpc.Message
	RPCError = jsonrpc.RPCError
	Codec    = jsonrpc.Codec
)

var NewCodec = jsonrpc.NewCodec
