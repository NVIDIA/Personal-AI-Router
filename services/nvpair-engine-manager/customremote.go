// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// customremote.go carries Custom-endpoint configuration between paired nodes.
// It is the same carrier as the remote-engine-settings surface it mirrors,
// and it keeps the same boundary: engine-manager owns trust (peer resolution,
// pinned mTLS, per-handshake pin checks) and never interprets the payload.
// The Custom configuration itself lives in each node's broker; what crosses
// here is a request, not an event — which is what makes carrying an add's
// API key through these handlers sanctioned plumbing rather than a leak. What
// the handlers must never do is echo that key back in a response body, an
// error string, or a log line, and none of them do.
//
// Origin half (runCustomRemote): the broker relays custom:remote-* stdio
// requests here verbatim, nodeId included; we resolve the target in the
// ec peer directory, rebuild a body from only the operation-relevant fields
// (the nodeId is addressing, never payload), and dial the peer's pinned
// control surface. Anything the peer cannot serve — offline, unclustered, no
// custom routes because it predates this feature — answers with one unified
// unavailable message so a version-skewed fleet reads as "update the other
// device", not as an empty endpoint list.
//
// Target half (customRoutes): pin-gated POSTs that forward the validated
// body to this node's broker through customRelay and surface the broker's
// answer as the HTTP response. The origin is already past our pin gate by
// the time a handler runs; the broker then applies its own self-forward
// guard against this node's facades.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// customPath is the control-surface prefix for custom endpoint operations,
// parallel to settingsPath. No new discovery service key: these routes ride
// the existing ec surface (port, TLS, pin gate) that remote engine management
// already established.
const customPath = "/v1/custom-endpoints/"

// customUnavailable is the one message for "the peer cannot serve this
// request right now" — offline, not clustered with us, or too old to have
// the routes. It mirrors "settings unavailable: …" so both surfaces fail a
// shared UI pattern.
const customUnavailable = "custom endpoints unavailable: device is offline or does not support custom endpoint management"

// customAddBody is the wire shape of a remote add and remote edit. It matches
// the broker's customAddParams fields that a peer is allowed to send (no id:
// the broker derives the entry key from host:port/name itself), and both the
// origin marshal and the target's strict decode share it so the two halves
// cannot drift. APIKey is part of the body by design — that is the one hop
// the key takes outside a node's own config file. ReplaceId and KeyAction
// carry an edit's intent: which entry is being edited and whether its stored
// credential survives; dropping them here would make peer-driven edits fork
// entries and disarm endpoints.
type customAddBody struct {
	Name      string `json:"name,omitempty"`
	URL       string `json:"url"`
	Port      int    `json:"port"`
	APIKey    string `json:"apiKey,omitempty"`
	ReplaceId string `json:"replaceId,omitempty"`
	KeyAction string `json:"keyAction,omitempty"`
}

// customRefreshBody is the wire shape of a remote refresh. Absent id means
// "every tracked endpoint"; the owner's broker scopes the probe cycle with
// whatever it receives, so no field is required to address the whole set.
type customRefreshBody struct {
	ID string `json:"id,omitempty"`
}

// customRemoveBody is the wire shape of a remote remove: the entry key as it
// appears in list views.
type customRemoveBody struct {
	ID string `json:"id"`
}

// customSetPortBody is the wire shape of a remote Custom facade port change.
// Its Port is the facade's listen port on the owning node, not an endpoint's
// port; the two share the "port" spelling because the relay reads one params
// struct, and the op says which one is meant.
type customSetPortBody struct {
	Port int `json:"port"`
}

// customRemoteParam is the input for the custom:remote-* stdio methods. The
// broker relays the client's params verbatim — nodeId included, because that
// is how we pick the peer — and everything except nodeId is repurposed into
// the rebuilt HTTP body rather than forwarded as-is. The field set is
// deliberately explicit: an editor field that is not rebuilt here quietly
// vanishes on the peer path, so add/edit/refresh inputs all have to appear.
// Port carries an endpoint's port for add and the Custom facade's port for
// custom:remote-set-port.
type customRemoteParam struct {
	NodeID    string `json:"nodeId"`
	Name      string `json:"name,omitempty"`
	URL       string `json:"url,omitempty"`
	Port      int    `json:"port,omitempty"`
	APIKey    string `json:"apiKey,omitempty"`
	ID        string `json:"id,omitempty"`
	ReplaceId string `json:"replaceId,omitempty"`
	KeyAction string `json:"keyAction,omitempty"`
}

// handleCustomMessage claims the custom carrier's stdio frames before the
// generic notification/log and request-dispatch paths, so the reply
// correlation and the remote ops do not have to share the engine:* switch.
func (m *Manager) handleCustomMessage(ctx context.Context, msg *Message) bool {
	if msg.Method == "custom:relay-reply" && msg.IsNotification() {
		m.customRelay.reply(msg.Params)
		return true
	}
	if !msg.IsRequest() {
		return false
	}
	switch msg.Method {
	// One line on purpose: the contract checker reads every method literal that
	// shares a `case` line, and wrapping this list hides the wrapped names from
	// the generated services-api.md surface index.
	case "custom:remote-list", "custom:remote-add", "custom:remote-refresh", "custom:remote-remove", "custom:remote-get-port", "custom:remote-set-port":
		go m.runCustomRemote(ctx, msg)
		return true
	}
	return false
}

// runCustomRemote answers a custom:remote-* request from the local broker on
// behalf of the local UI. Async like runRemote: a peer write takes at most
// seconds, but "at most seconds" still must not stall the read loop.
func (m *Manager) runCustomRemote(ctx context.Context, msg *Message) {
	var p customRemoteParam
	if !m.parse(msg, &p) {
		return
	}
	if p.NodeID == "" {
		m.respondOrErr(msg, nil, fmt.Errorf("custom remote request requires nodeId"))
		return
	}
	// Shape first, before any peer resolution: a portless add or a keyless
	// remove is a local mistake (D2 keeps the port mandatory on every path),
	// and answering it here means we never dial on a request that was wrong
	// to begin with — whoever the target was.
	op := strings.TrimPrefix(msg.Method, "custom:remote-")
	var body any
	switch op {
	case "list":
		body = struct{}{}
	case "add":
		if p.URL == "" || p.Port <= 0 || p.Port > 65535 {
			m.respondOrErr(msg, nil, fmt.Errorf("custom add requires a url and an explicit port"))
			return
		}
		body = customAddBody{Name: p.Name, URL: p.URL, Port: p.Port, APIKey: p.APIKey,
			ReplaceId: p.ReplaceId, KeyAction: p.KeyAction}
	case "refresh":
		// No shape gate: a refresh is a read, scoped or whole-set, and both
		// are valid exactly as they arrive.
		body = customRefreshBody{ID: p.ID}
	case "get-port":
		// A facade port is a property of the node, not of an endpoint, so this
		// read carries nothing.
		body = struct{}{}
	case "set-port":
		// Shape first, for the same reason add does: a port that was never
		// typed is a local mistake, and we do not dial a peer on one.
		if p.Port < 1 || p.Port > 65535 {
			m.respondOrErr(msg, nil, fmt.Errorf("custom set-port requires an explicit port between 1 and 65535"))
			return
		}
		body = customSetPortBody{Port: p.Port}
	case "remove":
		if p.ID == "" {
			m.respondOrErr(msg, nil, fmt.Errorf("custom remove requires an id"))
			return
		}
		body = customRemoveBody{ID: p.ID}
	default:
		m.respondOrErr(msg, nil, fmt.Errorf("unknown custom remote method: %s", msg.Method))
		return
	}
	peer, ok := m.peers.lookup(p.NodeID)
	if !ok || m.mesh == nil {
		m.respondOrErr(msg, nil, fmt.Errorf("%s", customUnavailable))
		return
	}
	client, err := m.remoteClient(ctx, peer)
	if err != nil {
		// Not clustered / not a pinned peer: the trust refusal keeps its own
		// words, same as the settings and model surfaces, because "pair with
		// that device first" is a different user action than "update it".
		m.respondOrErr(msg, nil, err)
		return
	}
	result, err := client.postCustom(ctx, customPath+op, body)
	m.respondOrErr(msg, result, err)
}

// customRoutes registers the target-side surface. Split from the listener so
// tests exercise it over httptest, same as settingsRoutes.
func (s *controlServer) customRoutes(mux *http.ServeMux) {
	for _, op := range []string{"list", "add", "refresh", "remove", "get-port", "set-port"} {
		mux.HandleFunc(customPath+op, s.requirePinCaller(func(w http.ResponseWriter, r *http.Request, caller string) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", 405)
				return
			}
			if s.exec.customParent == nil {
				http.Error(w, "custom endpoint coordinator unavailable", 503)
				return
			}
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
			if err != nil {
				http.Error(w, "custom endpoint request body too large", 400)
				return
			}
			// list, refresh and get-port address a scope or a node, not a
			// payload: an empty body is the whole-set or no-argument request,
			// so it normalizes to the scope-free shape before the strict decode
			// instead of failing it. add, remove and set-port still 400 on an
			// empty body — their payload is required.
			if (op == "list" || op == "refresh" || op == "get-port") && len(bytes.TrimSpace(body)) == 0 {
				body = []byte("{}")
			}
			// Strict shape before the relay hop: unknown fields (a smuggled
			// nodeId, a stray model name) and trailing garbage answer 400
			// here, so what the broker's custom plane receives is always
			// well-formed and its validation stays about routing, not JSON.
			decoder := json.NewDecoder(bytes.NewReader(body))
			decoder.DisallowUnknownFields()
			var target any
			switch op {
			case "add":
				target = &customAddBody{}
			case "refresh":
				target = &customRefreshBody{}
			case "remove":
				target = &customRemoveBody{}
			case "set-port":
				target = &customSetPortBody{}
			default:
				target = &struct{}{}
			}
			if decoder.Decode(target) != nil || decoder.Decode(&struct{}{}) != io.EOF {
				http.Error(w, "invalid custom endpoint request", 400)
				return
			}
			// No mid-call pin recheck ticker (settings has one): the whole
			// operation is bounded well under a second by the relay budget,
			// and the per-handshake gate already decided trust.
			result, err := s.exec.customParent(r.Context(), op, json.RawMessage(body), caller)
			if err != nil {
				// 409 carries the broker's scrubbed failure text; the raw
				// persistence error never made it this far.
				http.Error(w, err.Error(), 409)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(result)
		}))
	}
}

// postCustom dials a peer's custom surface. Two failures mean "this peer
// cannot serve custom endpoints" rather than "the request was wrong" — the
// connection never came up, and a 404 is a peer without these routes — so
// both answer with the unified unavailable message. A pinned peer's refusal
// (403) or a broker failure (409) keeps its detail; those are real answers.
func (c *remoteClient) postCustom(ctx context.Context, path string, body any) (json.RawMessage, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	// c.http, never readyHTTP: custom operations never wait on engine
	// readiness, so they keep the ordinary 30s header budget.
	resp, err := c.http.Do(req)
	if err != nil {
		c.forgetAddress()
		return nil, fmt.Errorf("%s", customUnavailable)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s", customUnavailable)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("custom %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return json.RawMessage(data), nil
}
