// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// The {service: port} set reported on /v1/node-info. It is what lets a peer that
// reached this node by a typed address — one on a Tailscale tailnet, where no
// mDNS record ever arrives — learn that this is a PAIR node and where each of its
// services listens.

package main

import (
	"encoding/json"
	"testing"

	"nvpair-shared/applog"
	"nvpair-shared/noderec"
)

func servicesPush(t *testing.T, services map[noderec.ServiceKey]int) applog.StdinMessage {
	t.Helper()
	params, err := json.Marshal(noderec.ServicesParams{Services: services})
	if err != nil {
		t.Fatalf("marshal services params: %v", err)
	}
	return applog.StdinMessage{Method: noderec.MethodSetServices, Params: params}
}

func TestServiceMap_AbsentUntilPushed(t *testing.T) {
	services := &serviceMap{}
	if got := services.get(); got != nil {
		t.Fatalf("service map before any push = %v, want nil", got)
	}

	var out map[string]any
	if err := json.Unmarshal(buildResponse(nil, nil, 0, statsSnapshot{}, "host", nil, services.get()), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, present := out["services"]; present {
		// A node that has not been told its own ports must not claim it has none:
		// a peer would read that as "not a PAIR node" and fall back to probing
		// raw engine ports that answer 403.
		t.Fatal("services must be absent from the response until the parent pushes the set")
	}
}

func TestServiceMap_ReportedAfterPush(t *testing.T) {
	services := &serviceMap{}
	handleSetServices(servicesPush(t, map[noderec.ServiceKey]int{
		noderec.ServiceNodeInfo:      14318,
		noderec.ServiceOllama:        11434,
		noderec.ServiceEngineManager: 14322,
		noderec.ServiceEngineControl: 14323,
	}), services)

	var typed NodeInfoResponse
	if err := json.Unmarshal(buildResponse(nil, nil, 0, statsSnapshot{}, "host", nil, services.get()), &typed); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := typed.Services[noderec.ServiceOllama]; got != 11434 {
		t.Fatalf("ol port = %d, want 11434", got)
	}
	if got := typed.Services[noderec.ServiceEngineControl]; got != 14323 {
		t.Fatalf("ec port = %d, want 14323", got)
	}
	if len(typed.Services) != 4 {
		t.Fatalf("services = %v, want 4 entries", typed.Services)
	}
}

// The set is replaced, never merged: a service that stopped is expressed by its
// key being absent, exactly as an unregister is on the discovery record. Merging
// would leave a departed service advertised to peers forever.
func TestServiceMap_PushReplacesRatherThanMerges(t *testing.T) {
	services := &serviceMap{}
	handleSetServices(servicesPush(t, map[noderec.ServiceKey]int{
		noderec.ServiceOllama:   11434,
		noderec.ServiceLMStudio: 1234,
	}), services)
	handleSetServices(servicesPush(t, map[noderec.ServiceKey]int{
		noderec.ServiceOllama: 11434,
	}), services)

	got := services.get()
	if _, present := got[noderec.ServiceLMStudio]; present {
		t.Fatalf("services = %v, want the departed lm key gone", got)
	}
}

func TestServiceMap_DropsUnusableEntriesAndMalformedPushes(t *testing.T) {
	services := &serviceMap{}
	handleSetServices(servicesPush(t, map[noderec.ServiceKey]int{
		noderec.ServiceOllama: 11434,
		"":                    9000, // no key
		noderec.ServiceErrors: 0,    // no port
	}), services)
	if got := services.get(); len(got) != 1 || got[noderec.ServiceOllama] != 11434 {
		t.Fatalf("services = %v, want only the ol entry", got)
	}

	// A malformed payload is dropped rather than latching: the broker re-pushes
	// on every change, so the next one corrects us.
	handleSetServices(applog.StdinMessage{
		Method: noderec.MethodSetServices,
		Params: json.RawMessage(`{"services":"not a map"}`),
	}, services)
	if got := services.get(); len(got) != 1 {
		t.Fatalf("services after a malformed push = %v, want the previous set kept", got)
	}
}

func TestServiceMap_IgnoresOtherMethods(t *testing.T) {
	services := &serviceMap{}
	msg := servicesPush(t, map[noderec.ServiceKey]int{noderec.ServiceOllama: 11434})
	msg.Method = noderec.MethodSetClusterIdentity
	handleSetServices(msg, services)
	if got := services.get(); got != nil {
		t.Fatalf("services = %v, want nil for an unrelated method", got)
	}
}
