// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// customrelay.go is the target-side half of the remote Custom-endpoint
// carrier. A peer that POSTs to this node's /v1/custom-endpoints/* surface is
// not served by engine-manager: the broker owns the Custom configuration, so
// the control handler hands the operation down the local stdio link as a
// `custom:relay-request` notification, correlated by a random id, and waits
// for the broker's `custom:relay-reply` to settle the HTTP response.
//
// The shape is settingsRelay's, with two deliberate deltas. There is no
// cancel-on-timeout: a Custom mutation is a file write plus an advisory prober
// nudge that settles in milliseconds, so a reply arriving after the budget is
// a stale answer to a caller who already gave up — it drops, and no second
// state-mutating command is issued. And the pending cap answers here instead
// of only at the broker, so a wedged broker link cannot grow the queue
// between the two processes.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// customRelayBudget bounds one peer-driven custom operation end to end. The
// work is a file write and a prober nudge; thirty seconds is the point where
// "slow" definitively means "wedged", and it stays under the caller's own
// response-header budget so the HTTP half cannot hang on after this one dies.
const customRelayBudget = 30 * time.Second

// customRelayPendingLimit caps in-flight peer-driven custom operations per
// process, mirroring the settings relay cap and the broker's own
// customRelayLimit.
const customRelayPendingLimit = 64

// customRelayFrame is one correlated custom operation on the
// engine-manager↔broker stdio link, sent as a `custom:relay-request`
// notification. The field names are pinned by the mirror struct in
// nvpair-ui-broker/customremote.go — the same convention customSetModelsPayload
// follows with the broker's push payload — and the pair is declared in
// desktop/docs/service-contract-exceptions.json.
type customRelayFrame struct {
	ID      string          `json:"id"`
	Op      string          `json:"op"`
	Request json.RawMessage `json:"request,omitempty"`
	Caller  string          `json:"caller,omitempty"`
}

// customReply is the `custom:relay-reply` body. A non-empty Error is the
// scrubbed, cross-network-safe failure text the broker chose to expose; the
// raw persistence error (which quotes local paths) never reaches this link.
type customReply struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error,omitempty"`
}

type customRelay struct {
	mu      sync.Mutex
	pending map[string]chan customReply
	send    func(string, any) error
}

// call relays one custom operation to the broker and waits for the answer.
// It runs on the HTTP handler's goroutine; the reply arrives on the codec
// read loop via handleCustomMessage, which is why correlation is by id and
// the channel is buffered — a late reply must never block the read loop.
func (r *customRelay) call(ctx context.Context, op string, request json.RawMessage, caller string) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, customRelayBudget)
	defer cancel()
	id := newOpID()
	ch := make(chan customReply, 1)
	r.mu.Lock()
	if len(r.pending) >= customRelayPendingLimit {
		r.mu.Unlock()
		return nil, fmt.Errorf("too many custom endpoint requests")
	}
	if r.pending == nil {
		r.pending = make(map[string]chan customReply)
	}
	r.pending[id] = ch
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(r.pending, id); r.mu.Unlock() }()
	if err := r.send("custom:relay-request", customRelayFrame{ID: id, Op: op, Request: request, Caller: caller}); err != nil {
		return nil, err
	}
	select {
	case reply := <-ch:
		if reply.Error != "" {
			return nil, fmt.Errorf("%s", reply.Error)
		}
		return reply.Result, nil
	case <-ctx.Done():
		// No cancel notification (settings has one): see the file header.
		// The deferred delete above means a reply after this point finds no
		// waiter and drops in reply().
		return nil, ctx.Err()
	}
}

// reply correlates an inbound `custom:relay-reply` with its waiter. Unknown
// or already-settled ids are ignored, which is what makes a reply to a
// timed-out call vanish instead of surfacing on an unrelated request.
func (r *customRelay) reply(raw json.RawMessage) {
	var reply customReply
	if json.Unmarshal(raw, &reply) != nil {
		return
	}
	r.mu.Lock()
	ch := r.pending[reply.ID]
	r.mu.Unlock()
	if ch != nil {
		select {
		case ch <- reply:
		default:
		}
	}
}
