// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The Custom facade's port is configurable, the same way an inference engine's
// proxy port is: the operator can move it, and it comes back where they left it.
//
// It cannot ride the engine settings plane. That plane describes a launch — a
// manifest, a server port, launch arguments, a revision to write them against —
// and Custom has none of those: it is a class of external endpoints, not a
// process this node starts. Its snapshot read begins with engine:get-launch,
// which has nothing to answer for an unmanaged engine kind, so a Custom port
// edit through engine:apply-settings would fail at the read.
//
// So this file keeps one narrow port pair, and it keeps the same authority the
// settings plane has: the proxy child owns the number. The broker asks the live
// facade to rebind (which binds first, persists the choice, and announces the
// new port), and it reads the live listen port when it is answering a query.
// There is no second copy of the port in broker state to drift, which is the
// reason the settings plane moved off engine:set-port in the first place.
//
// Two refusals keep the move from being self-defeating, both evaluated against
// this node's whole port picture rather than just what is listening:
// a port another engine's proxy or backend claims (a facade parked on a
// sibling's port cannot route to it and blocks it from starting), and a port one
// of this node's configured Custom endpoints forwards to (that is the
// self-forward loop custom/add refuses on the way in, created from the other
// end). Everything else is allowed, including a port some unrelated process
// holds: that loses a bind race, and the proxy reports it.
//
// Both methods answer for the addressed node, so a peer's section edits the peer.
// The carrier for that is customremote.go; the work below is identical either
// way, which is the point of the relay being transport rather than a second
// implementation.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// customProxyPortState is the reply to custom/get-port and the tail of a
// successful custom/set-port. Running says which kind of number Port is: true
// means a live facade is listening there, false means it is where the facade
// will land when it comes up (a planned fallback, or the persisted or stock
// port). The UI prints both the same way and says so, because "6767, nothing
// listening" is a fact the operator can act on and pretending otherwise would
// advertise a port that answers nothing.
type customProxyPortState struct {
	Port    int  `json:"port"`
	Running bool `json:"running"`
}

// handleCustomPortRPC dispatches the two Custom facade-port methods. It forks
// on the addressing nodeId before parsing anything, exactly like
// handleCustomEndpoints: a nodeId naming another node turns the whole call into
// a peer relay, and a nodeId naming this node is a local read wearing remote
// addressing.
//
// It runs on its own goroutine (see the dispatch in broker.go). A set is
// allowed to wait on the proxy: the rebind round-trip and the running-engine
// port lookup both have to finish before the caller learns whether the port
// they typed is the port the facade took.
func (b *Broker) handleCustomPortRPC(msg *Message) {
	target := customRequestTarget(msg.Params)
	if target != "" && target != b.nodeID {
		b.relayCustomToPeer(msg, target)
		return
	}
	switch msg.Method {
	case "custom/get-port":
		b.customGetPort(msg)
	case "custom/set-port":
		b.customSetPort(msg)
	}
}

func (b *Broker) customGetPort(msg *Message) {
	_ = b.codec.Respond(msg.ID, b.customProxyPortState())
}

func (b *Broker) customSetPort(msg *Message) {
	var params struct {
		Port int `json:"port"`
	}
	if len(msg.Params) > 0 {
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			_ = b.codec.RespondError(msg.ID, -32602, `custom/set-port: expected {"port": <int>}`)
			return
		}
	}
	state, err := b.moveCustomFacadePort(params.Port)
	if err != nil {
		slog.Warn("custom port change refused", "port", params.Port, "err", err)
		_ = b.codec.RespondError(msg.ID, -32000, err.Error())
		return
	}
	_ = b.codec.Respond(msg.ID, state)
}

// customProxyPortState reports the Custom facade's port as this node currently
// understands it: the live listen port when a facade reports ready, otherwise
// the port the next bring-up is planned to use.
//
// The planned value is read the same way custom/add reads it (see
// frontsFacadePort): a fallback recorded after a lost bind race outranks the
// persisted file, because that file is exactly what the fallback was chosen to
// replace, and the stock port is the answer when nobody has moved anything.
func (b *Broker) customProxyPortState() customProxyPortState {
	if pp := b.customProxyHandle(); pp != nil {
		if ready, port := pp.Status(customProxyProfile.Name); ready && port > 0 {
			return customProxyPortState{Port: port, Running: true}
		}
	}
	if rt := b.engineProxy(customProxyProfile); rt != nil {
		if planned := int(rt.startupPort.Load()); planned > 0 {
			return customProxyPortState{Port: planned}
		}
	}
	return customProxyPortState{Port: configuredEngineProxyPort(customProxyProfile)}
}

// moveCustomFacadePort converges the Custom facade onto port and reports where
// it landed.
//
// Typing the port the facade already listens on is a success, not a rebind: it
// keeps Apply honest after a no-op edit and avoids persisting a number that
// never changed.
//
// With no live facade there is nothing to rebind, and this does not invent a
// second authority for the number. The broker could remember a planned port the
// way the managed engines do, but that plan would then outrank the proxy's own
// persisted port on the next bring-up (IgnorePersistedPort), so a Custom move
// taken while the proxy happened to be down would silently override one taken
// while it was up. Reporting the missing facade is cheaper than that
// asymmetry: in the normal run the proxy is up whenever the broker is, and the
// restart path restores a persisted Custom port on its own.
func (b *Broker) moveCustomFacadePort(port int) (customProxyPortState, error) {
	if port < 1 || port > 65535 {
		return customProxyPortState{}, fmt.Errorf("port must be between 1 and 65535")
	}
	pp := b.customProxyHandle()
	if pp == nil {
		return customProxyPortState{}, fmt.Errorf(
			"the Custom proxy is not running on this node, so there is no facade port to change")
	}
	ready, current := pp.Status(customProxyProfile.Name)
	if !ready || current <= 0 {
		return customProxyPortState{}, fmt.Errorf(
			"the Custom proxy has not reported its port yet; try again once it is listening")
	}
	if port == current {
		return customProxyPortState{Port: current, Running: true}, nil
	}
	if reason := b.customFacadePortClaimed(port); reason != "" {
		return customProxyPortState{}, fmt.Errorf("port %d is already in use on this node: %s", port, reason)
	}

	body, err := json.Marshal(map[string]int{"port": port})
	if err != nil {
		return customProxyPortState{}, err
	}
	// Call bounds itself by proxyCallTimeout, the same wedge detector every
	// other facade rebind uses.
	result, rpcErr, err := pp.Call(context.Background(), customProxyProfile.addressed("set-port"), body)
	if err != nil || rpcErr != nil {
		slog.Warn("Custom facade rebind failed", "from", current, "to", port, "err", err, "rpcErr", rpcErr)
		if rpcErr != nil {
			return customProxyPortState{}, fmt.Errorf("the Custom proxy could not listen on port %d: %s", port, rpcErr.Message)
		}
		return customProxyPortState{}, fmt.Errorf("the Custom proxy did not answer; port %d was not applied", port)
	}
	var bound proxyReadyParams
	if json.Unmarshal(result, &bound) != nil || bound.Port != port {
		slog.Warn("Custom facade did not confirm the rebind", "from", current, "to", port)
		return customProxyPortState{}, fmt.Errorf("the Custom proxy did not confirm port %d", port)
	}
	slog.Info("moved the Custom facade", "from", current, "to", port)
	return customProxyPortState{Port: port, Running: true}, nil
}

// customFacadePortClaimed names why this node cannot put the Custom facade on
// port, or returns "" when nothing else has a claim on it.
//
// The sibling set is built from the engine table on purpose: it lists every
// other engine's proxy and managed-backend ports, including for an engine whose
// proxy is not enabled right now, because that engine's saved configuration
// outlives this process and would meet a facade sitting on its port.
// Running engines are asked live, which is how a relocated or externally
// installed engine stays visible here.
func (b *Broker) customFacadePortClaimed(port int) string {
	if reason, ok := b.siblingEngineProxyPorts(customProxyProfile)[port]; ok {
		return reason
	}
	if b.runningEnginePorts()[port] {
		return "an inference engine is running on that port; a proxy cannot route to an engine it sits on"
	}
	if key, baseURL := b.customEndpointOnPort(port); key != "" {
		return "configured endpoint " + key + " (" + baseURL + ") forwards to that port; " +
			"the Custom proxy cannot route to itself"
	}
	return ""
}

// customEndpointOnPort finds a configured Custom endpoint whose target port is
// port, returning its key and baseUrl for the refusal text. Moving the facade
// onto an endpoint's own address is the self-forward loop custom/add refuses
// when the endpoint comes first, so the set side has to refuse it too; the
// proxy's own alias guard is defense in depth, not permission.
func (b *Broker) customEndpointOnPort(port int) (string, string) {
	b.customPersistMu.Lock()
	entries, err := loadPersistedCustomEndpoints(b.customEndpointsPath)
	b.customPersistMu.Unlock()
	if err != nil {
		return "", ""
	}
	for _, entry := range entries {
		host, target, _, err := customEndpointTarget(entry.BaseURL)
		if err != nil {
			continue
		}
		if target == port && isLoopbackHost(host) {
			return entry.key(), entry.BaseURL
		}
	}
	return "", ""
}
