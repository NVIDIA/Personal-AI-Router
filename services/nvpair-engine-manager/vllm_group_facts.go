// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"slices"
	"sync"
	"time"
)

const vllmGroupFactsPath = "/v1/vllm/group-facts"
const vllmGroupFactsMaxBytes = 2 << 20

type vllmGroupControlRoute struct{ Local, Peer string }

// Use the socket selected by the existing pinned-mTLS client, including reused
// connections. Discovery candidates or configured bridge prefixes alone are not
// evidence that a management route actually carried this authenticated reply.
func vllmObservedControlRoute(base, local, peer string) (*vllmGroupControlRoute, error) {
	target, err := url.Parse(base)
	if err != nil || target.Scheme != "https" || target.Host != peer {
		return nil, errors.New("participant socket differs from the pinned control target")
	}
	values := make([]string, 0, 2)
	for _, endpoint := range []string{local, peer} {
		host, _, err := net.SplitHostPort(endpoint)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || ip.To4() == nil || !ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() {
			return nil, errors.New("authenticated private IPv4 management route is unavailable")
		}
		values = append(values, ip.String())
	}
	return &vllmGroupControlRoute{Local: values[0], Peer: values[1]}, nil
}

func bindVLLMGroupControlRoute(local, peer *vllmGroupFacts) error {
	if peer.controlRoute == nil {
		return errors.New("authenticated participant control route is unavailable")
	}
	for _, side := range []struct {
		facts   *vllmGroupFacts
		address string
	}{{local, peer.controlRoute.Local}, {peer, peer.controlRoute.Peer}} {
		var matched []vllmGroupAddress
		for _, address := range side.facts.Addresses {
			if address.IP == side.address {
				matched = append(matched, address)
			}
		}
		if len(matched) != 1 {
			return errors.New("authenticated control address has no unique observed local interface")
		}
		side.facts.Addresses = matched
	}
	return nil
}

// Node UUIDs are mapped through the existing directory and current certificate
// pins, never a renderer's supplied principal, certificate or endpoint.
func (m *Manager) vllmGroupIdentities(s vllmGroupSelection) ([]string, []string, error) {
	if err := validateVLLMGroupSelection(s); err != nil {
		return nil, nil, err
	}
	if m.mesh == nil || m.peers == nil || m.cableLocal == nil {
		return nil, nil, errors.New("current paired identity is unavailable")
	}
	m.mesh.Refresh()
	if !m.mesh.Clustered() {
		return nil, nil, errors.New("current paired membership is unavailable")
	}
	principals := make([]string, len(s.NodeIDs))
	pins := make([]string, len(s.NodeIDs))
	for rank, node := range s.NodeIDs {
		if node == m.cableLocal.nodeID {
			principals[rank] = m.mesh.NodeUUID()
		} else {
			peer, ok := m.peers.lookup(node)
			if !ok || peer.clusterUUID == "" {
				return nil, nil, errors.New("selected member is not a current paired engine-control peer")
			}
			principals[rank] = peer.clusterUUID
		}
		pin, ok := m.mesh.PinSHA256(principals[rank])
		if !ok || !onboardingSHA.MatchString(pin) {
			return nil, nil, errors.New("selected member has no current certificate pin")
		}
		pins[rank] = pin
	}
	return principals, pins, nil
}

// Read-only fixed inspection on the existing ec mTLS mux. It reads the selected
// model/runtime and native facts; it does not create reviews, ranks or services.
func (p *vllmGroupPeer) serveFacts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if p == nil || p.m == nil || p.m.mesh == nil || p.m.exec == nil {
		http.Error(w, "paired inspection unavailable", http.StatusServiceUnavailable)
		return
	}
	p.m.mesh.Refresh()
	caller, ok := p.m.mesh.VerifyClientPin(r)
	if !ok {
		http.Error(w, "paired coordinator required", http.StatusForbidden)
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer controller.SetReadDeadline(time.Time{})
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4097))
	var selection vllmGroupSelection
	if err != nil || len(raw) > 4096 || strictDiagnosticJSON(raw, &selection) != nil || validateVLLMGroupSelection(selection) != nil {
		http.Error(w, "invalid group selection", http.StatusBadRequest)
		return
	}
	principals, pins, err := p.m.vllmGroupIdentities(selection)
	if err != nil || caller != principals[0] || !slices.Contains(selection.NodeIDs, p.m.cableLocal.nodeID) {
		http.Error(w, "only the selected paired coordinator may inspect this member", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), vllmGroupFactsBudget(selection.Model))
	defer cancel()
	facts, err := p.m.exec.inspectVLLMGroupFacts(ctx, selection.Model, len(selection.NodeIDs))
	if err != nil {
		http.Error(w, "selected managed model/runtime/resource facts are unavailable: "+err.Error(), http.StatusConflict)
		return
	}
	after, afterPins, err := p.m.vllmGroupIdentities(selection)
	currentCaller, pinned := p.m.mesh.VerifyClientPin(r)
	if err != nil || !pinned || caller != currentCaller || !slices.Equal(principals, after) || !slices.Equal(pins, afterPins) {
		http.Error(w, "paired identity changed", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(facts)
}

func vllmGroupFactsBudget(model string) time.Duration {
	if isQwen38ProfileModel(model) {
		return 10 * time.Minute
	}
	return 90 * time.Second
}

func vllmGroupFactsRequestBudget(model string) time.Duration {
	if isQwen38ProfileModel(model) {
		return vllmGroupFactsBudget(model) + time.Minute
	}
	return remoteActionTimeout
}

func (c *remoteClient) vllmGroupFacts(ctx context.Context, s vllmGroupSelection) (vllmGroupFacts, error) {
	var f vllmGroupFacts
	if err := validateVLLMGroupSelection(s); err != nil {
		return f, err
	}
	// Model-byte verification can exceed the ordinary 30-second header budget.
	// Reuse the existing pinned long-operation pool; outer inspection deadlines
	// remain in force, and no alternate endpoint or TLS policy is introduced.
	inspection := *c
	if c.readyHTTP != nil {
		inspection.http = c.readyHTTP
	}
	var routeMu sync.Mutex
	var local, peer string
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if info.Conn == nil {
			return
		}
		routeMu.Lock()
		local, peer = info.Conn.LocalAddr().String(), info.Conn.RemoteAddr().String()
		routeMu.Unlock()
	}})
	var raw json.RawMessage
	var err error
	if isQwen38ProfileModel(s.Model) {
		raw, err = inspection.postJSONWithTimeout(ctx, vllmGroupFactsPath, "vllm", s, vllmGroupFactsRequestBudget(s.Model))
	} else {
		raw, err = inspection.postJSON(ctx, vllmGroupFactsPath, "vllm", s)
	}
	if err != nil {
		return f, err
	}
	if len(raw) > vllmGroupFactsMaxBytes || strictDiagnosticJSON(raw, &f) != nil {
		return f, errors.New("invalid bounded paired group facts")
	}
	routeMu.Lock()
	f.controlRoute, err = vllmObservedControlRoute(c.base, local, peer)
	routeMu.Unlock()
	if err != nil {
		return f, err
	}
	return f, nil
}
