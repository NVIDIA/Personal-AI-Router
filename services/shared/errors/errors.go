// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package errors holds the on-wire shapes used by the nvpair-errors
// subprocess and every component that talks to it (the supervising
// broker playing the broker role, plus producers that emit
// errors:report / errors:clear notifications on their existing stdio).
//
// The package is intentionally tiny — it does NOT include a producer
// client library, only the structs. Producers emit by calling whatever
// JSON-RPC encoder they already use; sharing the wire types here is
// just a single-source-of-truth for the field names and JSON tags so
// the broker, nvpair-errors, and the integration tests can't drift.
//
// See nvpair-errors/README.md for the full protocol and its
// "Cross-node propagation" section, whose replace-by-origin contract keeps
// later cross-node additions additive.
package errors

// ServiceError is the on-wire shape every producer emits and every
// consumer reads.
//
// Required fields: ID, Message, Timestamp. Optional fields pass
// through verbatim — nvpair-errors is a passive datastore that does not
// validate or interpret severity / action / engineType / operation /
// modelName beyond storing them.
//
// NodeID is the ORIGIN node id — the node that produced the error.
// It is preserved verbatim when an error propagates to a peer
// nvpair-errors; the broker uses it for loop-prevention (only
// propagate entries whose NodeID == local-node-id outbound).
//
// Severity and Action ship as plain strings rather than typed enums
// because the canonical value set is not finalized yet. Current
// placeholder values are "info"/"warning"/"error" and
// "dismiss"/"retry"/"none"; a final enum can replace these without a
// wire-shape change.
type ServiceError struct {
	ID         string `json:"id"`
	Message    string `json:"message"`
	Timestamp  int64  `json:"timestamp"`
	NodeID     string `json:"nodeId,omitempty"`
	Severity   string `json:"severity,omitempty"`
	Action     string `json:"action,omitempty"`
	EngineType string `json:"engineType,omitempty"`
	Operation  string `json:"operation,omitempty"`
	ModelName  string `json:"modelName,omitempty"`
}

// ClearParams is the payload of the errors:clear method.
//
// ClearedBy is stamped by the supervising broker with
// the local node id on every outgoing clear so cross-node clear
// propagation can use it for loop prevention. nvpair-errors
// ignores it for now — clear is just delete-by-id. Producers do not
// set this field; the broker is the only writer.
type ClearParams struct {
	ID        string `json:"id"`
	ClearedBy string `json:"clearedBy,omitempty"`
}

// SyncEnvelope is the body of the cross-node push: POST /v1/errors.
// It carries the FULL set of a single node's local-origin errors so
// the receiver can reconcile authoritatively for that NodeID — upsert
// every entry present and evict any previously-stored entry for the
// same NodeID that is absent from Errors. That replace-by-origin
// semantics is what makes the push idempotent and self-healing: a
// dropped packet, a cleared error, or a late-joining peer all converge
// on the next push without a separate clear or initial-sync channel.
//
// NodeID is the origin node id (the sender). It is the authority for
// which stored entries the receiver may reconcile; the receiver stamps
// every entry in Errors with this NodeID so a buggy or hostile peer
// cannot inject entries attributed to a third node.
//
// Errors is always the sender's complete local-origin list (entries
// whose own NodeID equals the sender's). A node never forwards errors
// it learned from a peer, which is the loop-prevention guarantee.
type SyncEnvelope struct {
	NodeID string         `json:"nodeId"`
	Errors []ServiceError `json:"errors"`
}
