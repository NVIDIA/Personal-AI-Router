// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The newline-delimited JSON-RPC 2.0 codec is single-sourced in
// nvpair-shared/jsonrpc. These local aliases keep this package's call sites and
// tests unchanged after removing the copy-pasted per-service codec. Unlike the
// other workers, engine-manager exchanges larger frames, so NewCodec uses the
// 8 MiB variant, and it relies on the recoverable *DecodeError from Read.

import (
	"io"

	"nvpair-shared/jsonrpc"
)

type (
	Message     = jsonrpc.Message
	RPCError    = jsonrpc.RPCError
	Codec       = jsonrpc.Codec
	DecodeError = jsonrpc.DecodeError
)

// maxFrameBytes caps a single inbound JSON-RPC frame. The orchestrator is the
// sole, trusted peer and only sends small engine:* requests, so 8 MiB is far
// above any real frame. An inbound frame larger than this surfaces as a
// terminal read error and manager.readLoop exits cleanly.
const maxFrameBytes = 8 << 20 // 8 MiB

// NewCodec wraps rw with the shared codec at engine-manager's 8 MiB frame cap.
func NewCodec(rw io.ReadWriter) *Codec { return jsonrpc.NewCodecMaxFrame(rw, maxFrameBytes) }
