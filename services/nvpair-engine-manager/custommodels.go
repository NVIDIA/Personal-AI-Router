// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"sync"
)

// Custom's model inventory never comes from the loopback sweep: Custom
// endpoints are user-configured external URLs with no manifest entry and no
// local process to query. The broker owns that configuration and pushes the
// current set over the local stdio link; this file holds the receiving state
// and the merge that puts it into every ModelsResult. Key presence is the
// published semantic — ByEngine["custom"] exists exactly while a non-empty
// set is configured, matching the documented "missing key = not running /
// not queryable" distinction. The push carries model ids only: the endpoint
// credential never travels this hop, and nothing here needs it.

// customSetModelsMethod is the broker→engine-manager notification carrying
// this node's custom model set.
const customSetModelsMethod = "custom:set-models"

// customSetModelsPayload is that notification's envelope.
type customSetModelsPayload struct {
	Models []string `json:"models"`
}

// customModels is the last set the broker pushed, normalized on write (empty
// names dropped, de-duplicated, copied) so the merge and the change gate
// upstream both read a stable value.
type customModels struct {
	mu    sync.Mutex
	names []string
}

// SetCustomModels replaces the published custom inventory. An empty (or
// fully-invalid) payload clears it, which removes ByEngine["custom"] from
// subsequent results rather than publishing an empty list.
func (e *Executor) SetCustomModels(models []string) {
	var stored []string
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		stored = append(stored, m)
	}
	e.custom.mu.Lock()
	e.custom.names = stored
	e.custom.mu.Unlock()
}

// CustomModels returns a copy of the published set (nil when unset).
func (e *Executor) CustomModels() []string {
	e.custom.mu.Lock()
	defer e.custom.mu.Unlock()
	if len(e.custom.names) == 0 {
		return nil
	}
	return append([]string(nil), e.custom.names...)
}
