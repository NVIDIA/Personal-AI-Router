// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"nvpair-shared/errors"
)

// broker_errors.go holds the broker's ERRORS-BROKER ROLE: it forwards the
// errors:report / errors:clear notifications its supervised workers emit
// into the nvpair-errors datastore, stamping the origin nodeId and the
// clearedBy attribution. nvpair-errors then dedups and pushes an
// errors:update snapshot, which onErrorsUpdate relays to the client.
//
// The wire shapes (errors.ServiceError / errors.ClearParams) are the
// single source of truth shared with nvpair-errors and every producer, so
// the broker never re-describes the JSON here.

// nowMillis is the timestamp helper for any ServiceError the broker has to
// stamp (a producer that left Timestamp at zero).
func nowMillis() int64 { return time.Now().UnixMilli() }

// subprocessCrashedID is the sticky error id for a crashed worker. One id
// per worker (no timestamp suffix) so repeated crashes upsert the same
// entry rather than piling up — the UI sees a single "X is down" that the
// recovery path clears.
func subprocessCrashedID(name string) string {
	return "supervisor:subprocess-crashed:" + name
}

// crashReporter builds the supervisor onCrash / onRecovered callbacks for a
// named worker. onCrash emits a sticky supervisor:subprocess-crashed:<name>
// error into the nvpair-errors pipeline (severity "error", action "none" so
// the UI renders it without any frontend change); onRecovered clears that
// entry once the worker has come back healthy. A flapping worker just
// refreshes the same id's timestamp.
func (b *Broker) crashReporter(name string) (onCrash func(attempt int), onRecovered func()) {
	id := subprocessCrashedID(name)
	onCrash = func(attempt int) {
		b.forwardErrorsReport(errors.ServiceError{
			ID:        id,
			Message:   fmt.Sprintf("subprocess %s exited unexpectedly", name),
			Timestamp: nowMillis(),
			NodeID:    b.nodeID,
			Severity:  "error",
			Action:    "none",
		})
	}
	onRecovered = func() {
		b.forwardErrorsClear(id)
	}
	return onCrash, onRecovered
}

// supervisedWorkerCallbacks builds the supervisor callbacks for a normal
// supervised worker: onCrash drops the worker's broker handle (so status
// reads like proxy:get-status reflect reality during the down/backoff
// window instead of returning the crashed process's stale cached state —
// the spawn closure re-sets the handle on a successful restart) and then
// reports the crash; onRecovered clears the crash error once the worker
// has stayed up long enough to be considered healthy.
func (b *Broker) supervisedWorkerCallbacks(name string, clearHandle func()) (onCrash func(attempt int), onRecovered func()) {
	report, recovered := b.crashReporter(name)
	onCrash = func(attempt int) {
		if clearHandle != nil {
			clearHandle()
		}
		report(attempt)
	}
	return onCrash, recovered
}

// errorsCrashFallback is nvpair-errors' onCrash. nvpair-errors is the pipeline's
// own sink, so it cannot report its own death into itself; instead of the
// normal pipeline report it logs to stderr and drops the dead handle (so
// any other worker's report cleanly no-ops via forwardErrorsReport's
// nil guard until nvpair-errors is back). The supervisor still auto-restarts
// it; on recovery producers re-emit and resurrect their entries per the
// ack-until-reemit contract.
func (b *Broker) errorsCrashFallback(attempt int) {
	b.setErrors(nil)
	slog.Error("nvpair-errors crashed; service-error pipeline unavailable until it restarts", "attempt", attempt)
}

// dispatchErrorsNotif handles the two error-pipeline notifications a
// producer can emit on its own stdout (errors:report / errors:clear). It
// is called from each worker reader's notification path. It returns true
// if the method was an error-pipeline method (claimed here), so the caller
// stops processing it; false otherwise so the caller continues its own
// routing. source is the worker name, used only for log context.
func (b *Broker) dispatchErrorsNotif(source, method string, params json.RawMessage) bool {
	switch method {
	case methodErrorsReport:
		var e errors.ServiceError
		if err := json.Unmarshal(params, &e); err != nil {
			slog.Warn("bad errors:report params", "source", source, "err", err)
			return true
		}
		// Producers may stamp NodeID themselves (so origin survives in the
		// cross-node sync); when they don't, the broker fills in the local
		// node id so the upsert isn't anonymous and a later errors:clear
		// (also keyed by the local node id) can match it. Timestamp is
		// filled the same defensive way nvpair-errors expects.
		if e.NodeID == "" {
			e.NodeID = b.nodeID
		}
		if e.Timestamp == 0 {
			e.Timestamp = nowMillis()
		}
		b.forwardErrorsReport(e)
		return true

	case methodErrorsClear:
		var p errors.ClearParams
		if err := json.Unmarshal(params, &p); err != nil {
			slog.Warn("bad errors:clear params", "source", source, "err", err)
			return true
		}
		// A producer-supplied ClearedBy is ignored — the broker is the
		// only writer of that field (see errors.ClearParams).
		b.forwardErrorsClear(p.ID)
		return true
	}
	return false
}

// forwardErrorsReport sends an errors:report notification to nvpair-errors.
// A no-op (with a debug log) when nvpair-errors is unavailable so callers
// don't each need the nil guard — the alternative is every producer's
// report vanishing with a hard error rather than a benign drop.
func (b *Broker) forwardErrorsReport(e errors.ServiceError) {
	if e.ID == ollamaHostAliasBlockedID {
		copy := e
		b.ollamaHostAliasErrorMu.Lock()
		b.ollamaHostAliasError = &copy
		b.ollamaHostAliasErrorMu.Unlock()
	}
	ep := b.getErrors()
	if ep == nil {
		slog.Debug("dropping errors:report — nvpair-errors unavailable", "id", e.ID)
		return
	}
	if err := ep.Notify(methodErrorsReport, e); err != nil {
		slog.Warn("failed to forward errors:report to nvpair-errors", "id", e.ID, "err", err)
	}
}

// forwardErrorsClear sends an errors:clear notification to nvpair-errors,
// stamping ClearedBy with the local node id (the broker is the only writer
// of that attribution). No-op with a debug log when nvpair-errors is
// unavailable.
func (b *Broker) forwardErrorsClear(id string) {
	if id == ollamaHostAliasBlockedID {
		b.ollamaHostAliasErrorMu.Lock()
		b.ollamaHostAliasError = nil
		b.ollamaHostAliasErrorMu.Unlock()
	}
	ep := b.getErrors()
	if ep == nil {
		slog.Debug("dropping errors:clear — nvpair-errors unavailable", "id", id)
		return
	}
	if err := ep.Notify(methodErrorsClear, errors.ClearParams{ID: id, ClearedBy: b.nodeID}); err != nil {
		slog.Warn("failed to forward errors:clear to nvpair-errors", "id", id, "err", err)
	}
}

func (b *Broker) replayOllamaHostAliasError() {
	b.ollamaHostAliasErrorMu.Lock()
	defer b.ollamaHostAliasErrorMu.Unlock()
	if b.ollamaHostAliasError == nil {
		return
	}
	ep := b.getErrors()
	if ep == nil {
		return
	}
	if err := ep.Notify(methodErrorsReport, *b.ollamaHostAliasError); err != nil {
		slog.Warn("failed to replay OLLAMA_HOST alias warning", "err", err)
	}
}

// handleErrorsGetInitial answers a client's errors:get-initial request by
// relaying it to nvpair-errors and returning the snapshot it reports. When no
// datastore is supervised the broker returns a benign empty array rather
// than an error, so a client can always seed its view safely.
func (b *Broker) handleErrorsGetInitial(msg *Message) {
	ep := b.getErrors()
	if ep == nil {
		if err := b.codec.Respond(msg.ID, []errors.ServiceError{}); err != nil {
			slog.Warn("failed to respond to errors:get-initial", "err", err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), errorsCallTimeout)
	defer cancel()
	result, rpcErr, err := ep.Call(ctx, methodErrorsGetInitial, msg.Params)
	switch {
	case err != nil:
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("errors:get-initial failed: %v", err)); err != nil {
			slog.Warn("failed to relay errors:get-initial error", "err", err)
		}
	case rpcErr != nil:
		if err := b.codec.RespondError(msg.ID, rpcErr.Code, rpcErr.Message); err != nil {
			slog.Warn("failed to relay errors:get-initial error", "err", err)
		}
	default:
		if err := b.codec.Respond(msg.ID, result); err != nil {
			slog.Warn("failed to relay errors:get-initial result", "err", err)
		}
	}
}

// handleErrorsReport answers a client's errors:report request: it forwards
// the report into nvpair-errors via the same path supervised workers use,
// stamping the origin nodeId / timestamp when the client left them unset
// (exactly as dispatchErrorsNotif does for a worker), and acks null. Like
// errors:clear the ack is just a "received" confirmation; the authoritative
// state change comes back asynchronously as an errors:update the broker
// relays. This is the client-facing twin of a worker emitting errors:report
// on its stdout, letting a client surface its own operational errors through
// the same registry.
func (b *Broker) handleErrorsReport(msg *Message) {
	var e errors.ServiceError
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &e); err != nil {
			if err := b.codec.RespondError(msg.ID, -32602, "invalid errors:report params: "+err.Error()); err != nil {
				slog.Warn("failed to respond to errors:report", "err", err)
			}
			return
		}
	}
	if e.ID == "" {
		if err := b.codec.RespondError(msg.ID, -32602, "errors:report requires an id"); err != nil {
			slog.Warn("failed to respond to errors:report", "err", err)
		}
		return
	}
	if e.NodeID == "" {
		e.NodeID = b.nodeID
	}
	if e.Timestamp == 0 {
		e.Timestamp = nowMillis()
	}
	b.forwardErrorsReport(e)
	if err := b.codec.Respond(msg.ID, nil); err != nil {
		slog.Warn("failed to respond to errors:report", "err", err)
	}
}

// handleErrorsClear answers a client's errors:clear request: it forwards
// the clear into nvpair-errors (which stamps clearedBy via forwardErrorsClear)
// and acks the client with null. The authoritative state change comes back
// asynchronously as an errors:update the broker relays — the ack is just a
// "received" confirmation, matching the fire-and-forget clear contract.
func (b *Broker) handleErrorsClear(msg *Message) {
	var p errors.ClearParams
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			if err := b.codec.RespondError(msg.ID, -32602, "invalid errors:clear params: "+err.Error()); err != nil {
				slog.Warn("failed to respond to errors:clear", "err", err)
			}
			return
		}
	}
	if p.ID == "" {
		if err := b.codec.RespondError(msg.ID, -32602, "errors:clear requires an id"); err != nil {
			slog.Warn("failed to respond to errors:clear", "err", err)
		}
		return
	}
	b.forwardErrorsClear(p.ID)
	if err := b.codec.Respond(msg.ID, nil); err != nil {
		slog.Warn("failed to respond to errors:clear", "err", err)
	}
}
