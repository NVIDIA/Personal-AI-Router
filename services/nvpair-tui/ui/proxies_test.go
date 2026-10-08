// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"testing"

	"nvpair-shared/engines"
	"nvpair-tui/rpc"
)

func TestBuildProxyEnginesUsesSelectedEngines(t *testing.T) {
	test := func(name string, selected []engines.Engine) {
		t.Run(name, func(t *testing.T) {
			got := buildProxyEngines(selected)
			if len(got) != len(selected) {
				t.Fatalf("proxy tabs = %d, want %d", len(got), len(selected))
			}
			for i, engine := range selected {
				if got[i].label != engine.DisplayName || got[i].prefix != engine.ComponentName() {
					t.Errorf("proxy tab %d = (%q, %q), want (%q, %q)",
						i, got[i].label, got[i].prefix, engine.DisplayName, engine.ComponentName())
				}
			}
		})
	}

	test("all shared engines", engines.All())
	llamacpp, ok := engines.ByName("llamacpp")
	if !ok {
		t.Fatal("shared engine table has no llamacpp")
	}
	test("explicit llama.cpp", []engines.Engine{llamacpp})
}

func TestLlamaCPPNotificationsUseSelectedFacade(t *testing.T) {
	llamacpp, ok := engines.ByName("llamacpp")
	if !ok {
		t.Fatal("shared engine table has no llamacpp")
	}
	view := newProxiesView(nil, []engines.Engine{llamacpp})

	cmd := view.handleNotification(&rpc.Message{
		Method: "llamacpp-proxy:ready",
		Params: []byte(`{"port":8080}`),
	})
	if cmd != nil {
		t.Fatal("ready notification unexpectedly returned a command")
	}
	if !view.engines[0].ready || view.engines[0].port != 8080 {
		t.Fatalf("llama.cpp status = ready:%v port:%d, want ready on 8080",
			view.engines[0].ready, view.engines[0].port)
	}

	if cmd := view.handleNotification(&rpc.Message{Method: "llamacpp-proxy:node/discovered"}); cmd == nil {
		t.Fatal("llama.cpp node notification did not schedule a nodes refresh")
	}
}

func TestDefaultViewsOmitProxiesWhenNoneSelected(t *testing.T) {
	for _, view := range defaultViews(nil, nil) {
		if view.Title() == "Proxies" {
			t.Fatal("Proxies view present with no selected proxy engines")
		}
	}
}
