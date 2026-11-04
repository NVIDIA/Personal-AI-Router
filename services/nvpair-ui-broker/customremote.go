// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Remote Custom-endpoint editing (Task 11), the two halves of one carrier
// that mirrors the remote-engine-settings relay exactly:
//
//   - Origin half: a custom/list|refresh|add|remove|get-port|set-port whose
//     nodeId names a *different* node is relayed to the local engine-manager
//     as `custom:remote-<op>`.
//     The broker never dials a peer itself and never decides whether the
//     target is trusted — resolving (and refusing) peers is engine-manager's
//     pin gate, same as the engine:remote-* relay it copies.
//   - Owner half: the target's engine-manager forwards the peer's
//     control-surface POST as a `custom:relay-request` notification, and this
//     broker executes it through the same custom plane a local edit uses —
//     validation, the self-forward guard evaluated against *this* node's
//     facades, persistence under manualMutationMu, prober nudge, candidate
//     bookkeeping.
//
// The API key travels in exactly one place: the add request's body, from the
// origin desktop over the pinned mTLS control channel into this node's 0600
// store. It never appears in a reply (views carry keySet) or an audit line
// (they carry op, caller, and endpoint id). The audit rule follows the
// AGENTS convention: operational metadata — caller identity, op, endpoint
// key — never payload.

import (
	"encoding/json"
	"log/slog"
	"strings"
)

// customRelayLimit bounds concurrently executing peer-driven custom
// operations, mirroring the settings relay's cap. Custom mutations are fast
// and small (one file write plus an advisory prober push), so reaching 64
// in flight means the prober link is wedged, not that peers are busy.
const customRelayLimit = 64

// customRelayFrame is one correlated custom operation over the
// engine-manager↔broker stdio link, sent as a `custom:relay-request`
// notification and answered with `custom:relay-reply`. The field names are
// pinned by the mirror struct in nvpair-engine-manager/customrelay.go — the
// same convention customEndpointStatus follows with the prober's
// CustomEndpointStatus — and the pair is declared in
// desktop/docs/service-contract-exceptions.json.
type customRelayFrame struct {
	ID      string          `json:"id"`
	Op      string          `json:"op"`
	Request json.RawMessage `json:"request,omitempty"`
	Caller  string          `json:"caller,omitempty"`
}

// customRequestTarget reads the optional nodeId that addresses another node.
// It runs for all three custom methods before any per-method parsing: the
// nodeId is transport addressing, not configuration, and a client that
// never sends one lands on the unchanged local path.
func customRequestTarget(params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var p struct {
		NodeID string `json:"nodeId"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ""
	}
	return strings.TrimSpace(p.NodeID)
}

// handleCustomEndpoints dispatches the four broker-local custom/* methods,
// with one fork: a non-local nodeId turns the request into a peer relay.
// A nodeId equal to this node's own id is a local request wearing remote
// addressing (the desktop keys its per-node section by nodeId) and runs
// locally so a self-targeted edit cannot dial itself.
func (b *Broker) handleCustomEndpoints(msg *Message) {
	target := customRequestTarget(msg.Params)
	if target != "" && target != b.nodeID {
		b.relayCustomToPeer(msg, target)
		return
	}
	switch msg.Method {
	case "custom/list":
		b.customList(msg)
	case "custom/refresh":
		b.customRefresh(msg)
	case "custom/add":
		b.customAdd(msg)
	case "custom/remove":
		b.customRemove(msg)
	}
}

// relayCustomToPeer hands a peer-targeted custom operation to the local
// engine-manager, which resolves the target in its ec peer directory and
// dials it over pinned mTLS — or answers with the unified
// offline-or-unsupported message. Params ride verbatim, nodeId included:
// that is how engine-manager picks the peer, and it never survives into the
// HTTP body (remoteCustom rebuilds the body from op-relevant fields only).
func (b *Broker) relayCustomToPeer(msg *Message, target string) {
	worker := b.getEngineMgr()
	if worker == nil {
		_ = b.codec.RespondError(msg.ID, -32000, "custom endpoints unavailable: engine manager is not running")
		return
	}
	method := "custom:remote-" + strings.TrimPrefix(msg.Method, "custom/")
	err := worker.RelayRequest(method, msg.Params, func(result json.RawMessage, rpcErr *RPCError, err error) {
		if err != nil {
			_ = b.codec.RespondError(msg.ID, -32000, err.Error())
		} else if rpcErr != nil {
			_ = b.codec.RespondError(msg.ID, rpcErr.Code, rpcErr.Message)
		} else {
			_ = b.codec.Respond(msg.ID, result)
		}
	})
	if err != nil {
		_ = b.codec.RespondError(msg.ID, -32000, err.Error())
	}
}

// logCustomRemote writes the audit line for a peer-driven operation: op,
// caller, and (when the operation named an endpoint) the endpoint key. That
// is the whole record — payload, including the add body's apiKey, is never a
// log field on any path here.
func (b *Broker) logCustomRemote(op, caller, key string, err error) {
	attrs := []any{"op", op, "caller", caller}
	if key != "" {
		attrs = append(attrs, "id", key)
	}
	if err != nil {
		slog.Warn("custom endpoint change from paired node failed", append(attrs, "err", err)...)
		return
	}
	slog.Info("custom endpoint change from paired node", attrs...)
}

// customStoreError is the cross-network spelling of a persistence failure:
// the raw error quotes this node's config path (and through it the user's
// home directory), which a peer has no use for. Local replies keep the full
// text; anything leaving the node gets this.
func customStoreError() string {
	return "custom-endpoints persistence failed on this node"
}

// handleCustomRelay executes a peer-driven custom operation that arrived as
// a `custom:relay-request` notification from this node's engine-manager.
// It runs on the engine-manager notification pump and stays off it: mutations
// continue through applyCustom* and answer via callback, the same shape
// customAdd has always had, and the facade-port pair forks onto its own
// goroutine because it waits on the proxy instead. Trust is already decided — the
// request reached us through our own engine-manager's pin gate over local
// stdio, exactly like handleSettingsRelay's; the Caller field is audit
// context, not an input to a decision.
func (b *Broker) handleCustomRelay(raw json.RawMessage) {
	var frame customRelayFrame
	if json.Unmarshal(raw, &frame) != nil {
		return
	}
	worker := b.getEngineMgr()
	if worker == nil {
		return
	}
	reply := func(result any, errorText string) {
		_ = worker.Notify("custom:relay-reply", struct {
			ID     string `json:"id"`
			Result any    `json:"result"`
			Error  string `json:"error,omitempty"`
		}{ID: frame.ID, Result: result, Error: errorText})
	}
	b.customRelayMu.Lock()
	if b.customRelayInFlight >= customRelayLimit {
		b.customRelayMu.Unlock()
		reply(nil, "too many custom endpoint requests")
		return
	}
	b.customRelayInFlight++
	b.customRelayMu.Unlock()
	settle := func(result any, errorText string) {
		b.customRelayMu.Lock()
		b.customRelayInFlight--
		b.customRelayMu.Unlock()
		reply(result, errorText)
	}
	switch frame.Op {
	case "list":
		views, err := b.customListSnapshot()
		b.logCustomRemote(frame.Op, frame.Caller, "", err)
		if err != nil {
			settle(nil, customStoreError())
			return
		}
		settle(views, "")
	case "add":
		var params customAddParams
		if err := json.Unmarshal(frame.Request, &params); err != nil {
			settle(nil, "custom/add: invalid request")
			return
		}
		// One pipeline for both halves: applyCustomEdit carries the same
		// key precedence, replaceId resolution, candidate bookkeeping, and
		// prober nudges a local add runs — the relay is transport, not a
		// second implementation.
		b.applyCustomEdit(params, func(view customEndpointView, err error) {
			b.logCustomRemote(frame.Op, frame.Caller, view.ID, err)
			if err != nil {
				if isCustomAddRejection(err) {
					settle(nil, err.Error())
				} else {
					settle(nil, customStoreError())
				}
				return
			}
			settle(view, "")
		})
	case "refresh":
		var p struct {
			ID string `json:"id"`
		}
		if len(frame.Request) > 0 {
			if json.Unmarshal(frame.Request, &p) != nil {
				settle(nil, "custom/refresh: invalid request")
				return
			}
		}
		// Same pipeline the local reply path uses: a fresh scoped probe
		// when this node has a prober, cache truth otherwise, and only
		// this node's persisted config decides which ids may appear.
		b.refreshCustomSnapshot(b.getManualNodes(), p.ID, func(views []customEndpointView, err error) {
			b.logCustomRemote(frame.Op, frame.Caller, p.ID, err)
			if err != nil {
				settle(nil, customStoreError())
				return
			}
			settle(views, "")
		})
	case "remove":
		var p struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(frame.Request, &p) != nil || p.ID == "" {
			settle(nil, "custom/remove requires an id")
			return
		}
		b.applyCustomRemove(p.ID, func(removed bool, err error) {
			b.logCustomRemote(frame.Op, frame.Caller, p.ID, err)
			if err != nil {
				settle(nil, customStoreError())
				return
			}
			settle(map[string]bool{"removed": removed}, "")
		})

	// The facade-port pair, answered by the same custom plane a local edit
	// uses. Both fork onto their own goroutine, and the reason is a hard
	// dependency rather than tidiness: a rebind's conflict check asks this
	// node's engine-manager which engines are running, and that answer has to
	// arrive on the very pump running this switch. Held inline, the check would
	// wait out its timeout and see no engines at all — a peer-driven move
	// would skip the only check it has. Neither op is audited through
	// logCustomRemote: a section polls its port read every 30 s, so an Info
	// line per poll would bury the change history that audit line exists to
	// keep, and a rebind already logs itself (with from/to ports).
	case "get-port":
		go func() {
			settle(b.customProxyPortState(), "")
		}()
	case "set-port":
		go func() {
			var p struct {
				Port int `json:"port"`
			}
			if json.Unmarshal(frame.Request, &p) != nil || p.Port < 1 || p.Port > 65535 {
				settle(nil, "custom/set-port requires an explicit port between 1 and 65535")
				return
			}
			state, err := b.moveCustomFacadePort(p.Port)
			if err != nil {
				// Refusal text is kept verbatim, unlike a persistence failure:
				// it names a port and who has it, which is what the operator
				// across the network needs in order to pick another one.
				settle(nil, err.Error())
				return
			}
			settle(state, "")
		}()
	default:
		settle(nil, "unknown custom operation: "+frame.Op)
	}
}
