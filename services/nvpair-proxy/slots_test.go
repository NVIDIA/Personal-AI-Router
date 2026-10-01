// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"nvpair-shared/engines"
)

func TestNormalizeSlots(t *testing.T) {
	ollama, _ := profileFor("ollama")
	lmstudio, _ := profileFor("lmstudio")
	test := func(name string, profile engineProfile, in *engineSlots, want engineSlots) {
		t.Run(name, func(t *testing.T) {
			if got := profile.normalizeSlots(in); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s normalizeSlots(%+v) = %+v, want %+v", profile.Name, in, got, want)
			}
		})
	}
	test("no slots", ollama, nil, engineSlots{})
	test("a default and per-model counts", lmstudio,
		&engineSlots{Default: 2, Models: map[string]int{"qwen/qwen3-8b": 4}},
		engineSlots{Default: 2, Models: map[string]int{"qwen/qwen3-8b": 4}})
	test("counts below one are dropped", lmstudio,
		&engineSlots{Default: 0, Models: map[string]int{"zero": 0, "negative": -1, "kept": 3}},
		engineSlots{Models: map[string]int{"kept": 3}})
	test("a negative default is dropped", ollama, &engineSlots{Default: -2}, engineSlots{})
	test("no valid model leaves no map", lmstudio,
		&engineSlots{Default: 1, Models: map[string]int{"zero": 0, " ": 2}},
		engineSlots{Default: 1})
	test("Ollama keys take the implied latest tag", ollama,
		&engineSlots{Default: 1, Models: map[string]int{"llama3": 2, "llama3:8b": 3}},
		engineSlots{Default: 1, Models: map[string]int{"llama3:latest": 2, "llama3:8b": 3}})
	test("names that normalize alike keep the smaller count", ollama,
		&engineSlots{Default: 1, Models: map[string]int{"llama3": 4, "llama3:latest": 2}},
		engineSlots{Default: 1, Models: map[string]int{"llama3:latest": 2}})
	test("LM Studio keys are kept exactly", lmstudio,
		&engineSlots{Default: 1, Models: map[string]int{"qwen3-8b": 2, "qwen3-8b:latest": 3}},
		engineSlots{Default: 1, Models: map[string]int{"qwen3-8b": 2, "qwen3-8b:latest": 3}})
}

// sendLocalBackend drives node/set-local-backend through the proxy's real
// dispatch and checks whether the proxy accepted it.
func sendLocalBackend(t *testing.T, p *Proxy, out *bytes.Buffer, engine, params string, wantAccepted bool) {
	t.Helper()
	id := json.RawMessage(`1`)
	out.Reset()
	p.handleMessage(&Message{
		Method: engines.AddressMethod(engine, "node/set-local-backend"),
		Params: json.RawMessage(params),
		ID:     &id,
	})
	var reply struct {
		Result *struct {
			OK bool `json:"ok"`
		} `json:"result"`
		Error *RPCError `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &reply); err != nil {
		t.Fatalf("decode reply %q: %v", out.String(), err)
	}
	accepted := reply.Error == nil && reply.Result != nil && reply.Result.OK
	if accepted != wantAccepted {
		t.Fatalf("node/set-local-backend %s: accepted = %v (error %+v), want %v", params, accepted, reply.Error, wantAccepted)
	}
}

func trackedSlots(f *facade) engineSlots {
	f.slots.mu.Lock()
	defer f.slots.mu.Unlock()
	return f.slots.slots
}

const ollamaBackendWithSlots = `{"engine":"ollama","host":"127.0.0.1","port":11435,"healthy":true,` +
	`"slots":{"default":2,"models":{"llama3":4,"broken":0}}}`

// The facade's tracker keeps the normalized counts, and the stored backend
// keeps none, so there is one copy of them.
func TestSetLocalBackendTracksNormalizedSlots(t *testing.T) {
	profile, _ := profileFor("ollama")
	var out bytes.Buffer
	p := newTestProxy(profile, NewCodec(&out), nil, 0)
	f := p.soleFacade()

	sendLocalBackend(t, p, &out, profile.Name, ollamaBackendWithSlots, true)
	want := engineSlots{Default: 2, Models: map[string]int{"llama3:latest": 4}}
	if got := trackedSlots(f); !reflect.DeepEqual(got, want) {
		t.Fatalf("tracked slots = %+v, want %+v", got, want)
	}
	if got := f.currentLocalBackend(); got.Port != 11435 || !got.Healthy || got.Slots != nil {
		t.Fatalf("stored backend = %+v with slots %+v, want healthy port 11435 and no slots", got, got.Slots)
	}
}

func TestSetLocalBackendReplacesTrackedSlots(t *testing.T) {
	profile, _ := profileFor("ollama")
	tracked := engineSlots{Default: 2, Models: map[string]int{"llama3:latest": 4}}
	test := func(name, params string, accepted bool, want engineSlots) {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			p := newTestProxy(profile, NewCodec(&out), nil, 0)
			f := p.soleFacade()
			sendLocalBackend(t, p, &out, profile.Name, ollamaBackendWithSlots, true)

			sendLocalBackend(t, p, &out, profile.Name, params, accepted)
			if got := trackedSlots(f); !reflect.DeepEqual(got, want) {
				t.Fatalf("tracked slots = %+v, want %+v", got, want)
			}
		})
	}
	test("new slots replace them",
		`{"engine":"ollama","host":"127.0.0.1","port":11435,"healthy":true,"slots":{"default":3}}`,
		true, engineSlots{Default: 3})
	test("a backend without slots clears them",
		`{"engine":"ollama","host":"127.0.0.1","port":11435,"healthy":false}`,
		true, engineSlots{})
	test("a rejected backend leaves them alone",
		`{"engine":"ollama","host":"192.0.2.1","port":11435,"healthy":true,"slots":{"default":9}}`,
		false, tracked)
}
