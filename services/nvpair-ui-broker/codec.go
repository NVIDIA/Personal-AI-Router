// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The newline-delimited JSON-RPC 2.0 codec and the bidirectional Peer
// (id-correlation + read pump) are single-sourced in nvpair-shared/jsonrpc. These
// local aliases keep the broker's call sites unchanged. This covers the
// broker's client-facing Codec and the parent-side worker handles, which are
// built on Peer.

import (
	"io"

	"nvpair-shared/jsonrpc"
)

type (
	Message     = jsonrpc.Message
	RPCError    = jsonrpc.RPCError
	DecodeError = jsonrpc.DecodeError
	Codec       = jsonrpc.Codec
	Peer        = jsonrpc.Peer
)

var (
	NewCodec = jsonrpc.NewCodec
	NewPeer  = jsonrpc.NewPeer
)

// errPeerClosed is the sentinel a worker handle's Call/RelayRequest returns
// once the child's transport has gone away.
var errPeerClosed = jsonrpc.ErrPeerClosed

// readWriter adapts a child's separate stdout (read) and stdin (write) into the
// io.ReadWriter the shared Codec expects.
type readWriter struct {
	io.Reader
	io.Writer
}
