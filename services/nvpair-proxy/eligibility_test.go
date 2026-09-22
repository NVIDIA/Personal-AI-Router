// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// Coverage for which of a node's advertised inventories makes it an owner.
//
// Ollama and LM Studio load a requested model on demand, so a catalog entry is
// a promise they can serve it. PAIR runs llama.cpp with --no-models-autoload,
// so a downloaded model it has not been told to load will not be served, and
// eligibility there has to read the loaded set instead. These assert the
// difference directly, because everything else in the suite goes through
// advertiseEngine and so would keep passing if the two collapsed.

import (
	"testing"

	"nvpair-shared/noderec"
)

// nodeAdvertising builds a relay record for one engine with no inventory, ready
// for a test to populate whichever field it means.
func nodeAdvertising(p engineProfile) noderec.DirectoryNode {
	return noderec.DirectoryNode{
		HostUUID: "uuid-eligibility",
		Name:     "host-eligibility",
		IP:       "10.0.0.9",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			p.DiscoveryService: {Port: p.FacadePort},
		},
	}
}

// Exactly one engine reads the loaded set. Pinning the count keeps a new
// engine from silently inheriting llama.cpp's answer, which is the failure
// this field exists to make impossible.
func TestLoadedOnlyEligibilityIsDeclaredNotInherited(t *testing.T) {
	loaded := map[string]bool{}
	for _, p := range profiles {
		if p.ModelEligibility == loadedModels {
			loaded[p.Name] = true
		}
	}
	if len(loaded) != 1 || !loaded["llamacpp"] {
		t.Fatalf("engines reading the loaded set = %v, want just llamacpp", loaded)
	}
}

// A node whose catalog lists a model it has not loaded is an owner for the
// on-demand engines and not for llama.cpp.
func TestCatalogOnlyNodeIsNotALlamaCppOwner(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		n := nodeAdvertising(tc.profile)
		n.ModelsByEngine = map[string][]string{tc.profile.Name: {"on-disk"}}
		n.Models = []string{"on-disk"}

		got, ok := subscribedToNode(tc.profile, n)
		if !ok {
			t.Fatal("node advertising this engine should still project")
		}
		if tc.profile.ModelEligibility == loadedModels {
			if len(got.Models) != 0 {
				t.Fatalf("catalog-only node offered %v as loaded owners", got.Models)
			}
			return
		}
		if len(got.Models) != 1 || got.Models[0] != "on-disk" {
			t.Fatalf("catalog models = %v, want [on-disk]", got.Models)
		}
	})
}

// The converse: a loaded report is what makes llama.cpp route, and it is not
// read as a catalog by the engines that do consult one.
func TestLoadedReportMakesLlamaCppAnOwner(t *testing.T) {
	llamacpp := llamacppCase(t).profile

	n := nodeAdvertising(llamacpp)
	n.LoadedByEngine = map[string][]string{llamacpp.Name: {"resident"}}

	got, ok := subscribedToNode(llamacpp, n)
	if !ok {
		t.Fatal("node advertising llama.cpp should project")
	}
	if len(got.Models) != 1 || got.Models[0] != "resident" {
		t.Fatalf("loaded models = %v, want [resident]", got.Models)
	}
}

// A missing report means nothing is loaded, never "fall back to the catalog".
// Without that, a node that never answered a loaded-models query would look
// like an owner of everything it has on disk.
func TestMissingLoadedReportIsNotACatalogFallback(t *testing.T) {
	llamacpp := llamacppCase(t).profile

	n := nodeAdvertising(llamacpp)
	n.Models = []string{"on-disk"}
	n.ModelsByEngine = map[string][]string{llamacpp.Name: {"on-disk"}}
	// LoadedByEngine deliberately absent.

	got, ok := subscribedToNode(llamacpp, n)
	if !ok {
		t.Fatal("node advertising llama.cpp should project")
	}
	if len(got.Models) != 0 {
		t.Fatalf("a node with no loaded report offered %v", got.Models)
	}
}

// Attribution stays per engine on the loaded side too: a model resident under
// another engine does not make this one an owner.
func TestLoadedAttributionDoesNotLeakAcrossEngines(t *testing.T) {
	llamacpp := llamacppCase(t).profile

	n := nodeAdvertising(llamacpp)
	n.LoadedByEngine = map[string][]string{
		llamacpp.Name: {"mine"},
		"lmstudio":    {"theirs"},
	}

	got, ok := subscribedToNode(llamacpp, n)
	if !ok {
		t.Fatal("node advertising llama.cpp should project")
	}
	if len(got.Models) != 1 || got.Models[0] != "mine" {
		t.Fatalf("loaded models = %v, want [mine] only", got.Models)
	}
}
