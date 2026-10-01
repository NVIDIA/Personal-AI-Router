// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"maps"
	"os"
	"strconv"
	"strings"
)

// EngineSlots is how many requests a running engine processes at once for one
// model. Models holds the count of each model that reports its own, and
// Default covers every other model.
type EngineSlots struct {
	Default int            `json:"default"`
	Models  map[string]int `json:"models,omitempty"`
}

// slotsLocked reports the running engine's slots, or nil when the engine is
// stopped or its manifest declares none. Caller holds st.mu.
func (st *engineState) slotsLocked() *EngineSlots {
	spec := st.manifest.Slots
	if spec == nil || !st.running {
		return nil
	}
	slots := &EngineSlots{Default: spec.Default}
	if st.slotDefault > 0 {
		slots.Default = st.slotDefault
	}
	if len(st.slotModels) > 0 {
		slots.Models = maps.Clone(st.slotModels)
	}
	return slots
}

// clearSlotsLocked forgets the counts of a run that ended. Caller holds st.mu.
func (st *engineState) clearSlotsLocked() {
	st.slotDefault = 0
	st.slotModels = nil
}

// launchSlots returns the count an NVPAIR-launched process reads from
// spec.Env, or 0 when the launch sets none and the manifest default applies.
// The launch's own environment wins over the inherited one, as it does for
// the child. Surrounding spaces and quotes are ignored, as Ollama ignores
// them when it reads its variables, and a value that is not a positive
// integer leaves the engine on its own default.
func launchSlots(spec *Slots, env map[string]string) int {
	if spec == nil || spec.Env == "" {
		return 0
	}
	value, ok := launchEnvValue(env, spec.Env)
	if !ok {
		value, ok = os.LookupEnv(spec.Env)
	}
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(strings.Trim(strings.TrimSpace(value), `"'`))
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// launchEnvValue looks a variable up in a launch environment, matching names
// the way the host OS does.
func launchEnvValue(env map[string]string, name string) (string, bool) {
	for key, value := range env {
		if environmentKey(key) == environmentKey(name) {
			return value, true
		}
	}
	return "", false
}

// loadedSlots reads per-model counts from a loaded_models response. Each
// loaded instance maps its id to its count. A model key that no instance id
// matches maps to the smallest count among the model's instances, because a
// request naming the key may land on any of them. Counts below 1 are ignored,
// and so are rows of the wrong shape. ok is false only when the response
// lacks the declared array, so the caller can keep its last good counts.
func loadedSlots(raw json.RawMessage, spec *LoadedSlots) (map[string]int, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	var models []map[string]json.RawMessage
	if err := json.Unmarshal(obj[spec.Array], &models); err != nil || models == nil {
		return nil, false
	}
	counts := map[string]int{}
	smallestByKey := map[string]int{}
	for _, model := range models {
		var key string
		_ = json.Unmarshal(model[spec.Key], &key)
		var instances []map[string]json.RawMessage
		_ = json.Unmarshal(model[spec.Instances], &instances)
		for _, instance := range instances {
			n, ok := intAt(instance, spec.Parallel)
			if !ok || n < 1 {
				continue
			}
			var id string
			if json.Unmarshal(instance[spec.InstanceID], &id) == nil && id != "" {
				if cur, seen := counts[id]; !seen || n < cur {
					counts[id] = n
				}
			}
			if key != "" {
				if cur, seen := smallestByKey[key]; !seen || n < cur {
					smallestByKey[key] = n
				}
			}
		}
	}
	for key, n := range smallestByKey {
		if _, ok := counts[key]; !ok {
			counts[key] = n
		}
	}
	return counts, true
}

// intAt decodes the integer at a field path inside obj.
func intAt(obj map[string]json.RawMessage, path []string) (int, bool) {
	if len(path) == 0 {
		return 0, false
	}
	for _, name := range path[:len(path)-1] {
		var next map[string]json.RawMessage
		if err := json.Unmarshal(obj[name], &next); err != nil {
			return 0, false
		}
		obj = next
	}
	var n int
	if err := json.Unmarshal(obj[path[len(path)-1]], &n); err != nil {
		return 0, false
	}
	return n, true
}

// runGeneration returns the engine's run generation, so a reader can tell
// whether the run it queried is still the current one.
func (e *Executor) runGeneration(engine string) int64 {
	st, err := e.state(engine)
	if err != nil {
		return 0
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.gen
}

// recordLoadedSlots replaces a running engine's per-model counts with the ones
// a loaded_models response reports. A response that does not parse keeps the
// last good counts. One that arrives after the engine stopped, or after
// engine-manager started it again, is dropped.
func (e *Executor) recordLoadedSlots(engine string, gen int64, raw json.RawMessage) {
	st, err := e.state(engine)
	if err != nil || st.manifest.Slots == nil || st.manifest.Slots.Loaded == nil {
		return
	}
	models, ok := loadedSlots(raw, st.manifest.Slots.Loaded)
	if !ok {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.running || st.gen != gen {
		return
	}
	st.slotModels = models
}
