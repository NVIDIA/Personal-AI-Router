// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

// TestWriteServicesFrame pins the wire form of the service-map push node-info
// decodes. It is the only way a peer that never received this host's mDNS record
// — anything across a routed or overlay network — learns which services this node
// runs, so a renamed method or a missing newline silently reduces such a peer to
// probing raw engine ports that answer 403.
func TestWriteServicesFrame(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	services := map[noderec.ServiceKey]int{
		noderec.ServiceNodeInfo: 14318,
		noderec.ServiceOllama:   11434,
	}
	if err := writeServicesFrame(&mu, &buf, services); err != nil {
		t.Fatalf("write: %v", err)
	}

	line := buf.String()
	if !strings.HasSuffix(line, "\n") {
		t.Error("frame is not newline-terminated; node-info reads line-delimited frames")
	}
	if strings.Count(line, "\n") != 1 {
		t.Errorf("frame contains %d newlines, want exactly one", strings.Count(line, "\n"))
	}

	var frame struct {
		JSONRPC string                 `json:"jsonrpc"`
		Method  string                 `json:"method"`
		ID      json.RawMessage        `json:"id"`
		Params  noderec.ServicesParams `json:"params"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		t.Fatalf("decode frame %q: %v", line, err)
	}
	if frame.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", frame.JSONRPC)
	}
	if frame.Method != noderec.MethodSetServices {
		t.Errorf("method = %q, want %q", frame.Method, noderec.MethodSetServices)
	}
	// A notification, not a request: node-info's stdout is drained to io.Discard,
	// so an id-bearing frame would strand a reply.
	if len(frame.ID) != 0 {
		t.Errorf("frame carries an id (%s); the push must be a notification", frame.ID)
	}
	if frame.Params.Services[noderec.ServiceOllama] != 11434 {
		t.Errorf("services = %v, want ol=11434", frame.Params.Services)
	}
}

// An empty set is a real value: it is how "every service went away" is expressed,
// and merging on the far side would leave a departed service advertised forever.
func TestWriteServicesFrame_EmptySetIsStillSent(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	if err := writeServicesFrame(&mu, &buf, nil); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !strings.Contains(buf.String(), `"services":{}`) {
		t.Errorf("frame did not carry an empty services object: %s", buf.String())
	}
}

// The map node-info reports and the ports the scanner advertises come from one
// cache, so a peer reading /v1/node-info and a peer reading the mDNS record agree.
func TestLocalServices_MirrorsTheRegistrationCache(t *testing.T) {
	b := &Broker{regCache: relay.NewRegistrationCache()}
	b.regCache.Register(noderec.RegisterParams{Service: noderec.ServiceNodeInfo, Port: 14318})
	b.regCache.Register(noderec.RegisterParams{Service: noderec.ServiceOllama, Port: 11434})
	b.regCache.Register(noderec.RegisterParams{Service: noderec.ServiceEngineControl, Port: 14323})

	got := b.localServices()
	want := map[noderec.ServiceKey]int{
		noderec.ServiceNodeInfo:      14318,
		noderec.ServiceOllama:        11434,
		noderec.ServiceEngineControl: 14323,
	}
	if len(got) != len(want) {
		t.Fatalf("localServices = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("localServices[%s] = %d, want %d", k, got[k], v)
		}
	}

	b.regCache.Unregister(noderec.ServiceOllama)
	if _, present := b.localServices()[noderec.ServiceOllama]; present {
		t.Error("an unregistered service must leave the map a peer reads")
	}
}
