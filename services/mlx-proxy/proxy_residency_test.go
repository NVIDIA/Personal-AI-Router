// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

// mlx-proxy's one behavioural departure from the proxy it was cloned from:
// mlx_lm.server holds a single model, so an owner that already has the
// requested model resident is preferred over one that merely has it on disk --
// but a disk owner still routes when nobody is resident, which is what stops a
// cold cluster from refusing its own first request.
func TestPreferResidentOwners(t *testing.T) {
	const model = "mlx-community/Qwen3-VL-8B-Instruct-4bit"
	other := "mlx-community/Qwen3-VL-2B-Instruct-bf16"

	disk := Node{ID: "disk", Models: []string{model, other}, Loaded: []string{other}}
	resident := Node{ID: "resident", Models: []string{model, other}, Loaded: []string{model}}
	stranger := Node{ID: "stranger", Models: []string{other}, Loaded: []string{other}}

	for _, tc := range []struct {
		name        string
		node        Node
		advertises  bool
		holds       bool
	}{
		{"resident owner advertises and holds", resident, true, true},
		{"disk owner advertises but does not hold", disk, true, false},
		{"non-owner does neither", stranger, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nodeAdvertisesModel(tc.node, model); got != tc.advertises {
				t.Errorf("nodeAdvertisesModel = %v, want %v", got, tc.advertises)
			}
			if got := nodeHoldsModel(tc.node, model); got != tc.holds {
				t.Errorf("nodeHoldsModel = %v, want %v", got, tc.holds)
			}
		})
	}

	// A node that reports no residency at all (an older peer, or one whose
	// engine-manager could not be reached) must stay eligible, not vanish.
	unknown := Node{ID: "unknown", Models: []string{model}}
	if !nodeAdvertisesModel(unknown, model) {
		t.Error("a node with unknown residency must remain an eligible owner")
	}
	if nodeHoldsModel(unknown, model) {
		t.Error("unknown residency must not be reported as resident")
	}
}
