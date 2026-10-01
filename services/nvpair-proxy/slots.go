// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "sync"

// engineSlots is how many requests the local engine processes at once for one
// model. The broker relays it from engine-manager on node/set-local-backend.
// Models holds per-model counts, and Default covers every other model.
type engineSlots struct {
	Default int            `json:"default"`
	Models  map[string]int `json:"models,omitempty"`
}

// normalizeSlots keeps only counts of at least 1, and keys the per-model counts
// by the normalized model name routing compares. When two names normalize to
// one key, the smaller count wins. A missing default stays zero.
func (p engineProfile) normalizeSlots(s *engineSlots) engineSlots {
	var out engineSlots
	if s == nil {
		return out
	}
	if s.Default >= 1 {
		out.Default = s.Default
	}
	for model, n := range s.Models {
		key := p.normalizeModel(model)
		if key == "" || n < 1 {
			continue
		}
		if out.Models == nil {
			out.Models = make(map[string]int)
		}
		if cur, seen := out.Models[key]; !seen || n < cur {
			out.Models[key] = n
		}
	}
	return out
}

// slotTracker holds the local engine's slot counts for one facade.
type slotTracker struct {
	mu    sync.Mutex
	slots engineSlots
}

// setSlots replaces the counts. The zero value means the engine reported none.
func (t *slotTracker) setSlots(s engineSlots) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.slots = s
}
