// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// JSON-RPC error codes for the local interface (§7.6 of the spec). The standard
// codes come from JSON-RPC 2.0; the -32001/-32002/-32004 codes are app-specific
// and live in the reserved server-error range (-32000..-32099).
//
// The error-with-data responder itself lives in the shared codec as
// Codec.RespondErrorData (nvpair-shared/jsonrpc).
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	codeUnknownInvite  = -32001 // inviteId not found or evicted
	codeInvalidState   = -32002 // invite exists but call invalid for its state
	codePrecondition   = -32004 // a required precondition isn't met
)
