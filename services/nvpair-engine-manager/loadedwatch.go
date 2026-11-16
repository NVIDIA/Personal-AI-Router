// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"maps"
	"slices"
	"time"
)

// modelsChangedMethod is the push emitted whenever one of an engine's model
// lists changes: what it serves, what is loaded in memory, or what it has
// downloaded.
const modelsChangedMethod = "engine:models-changed"

// defaultLoadedPollSeconds is the default cadence of the loaded-model watcher,
// shared by NewExecutor and the --loaded-poll-interval flag so the two can't
// drift. 0 (via the flag) disables the watcher.
const defaultLoadedPollSeconds = 5

// modelsChangedParams is the engine:models-changed payload: which engine's
// lists changed (the trigger) plus the full current model result — the same
// shape engine:models returns — so a push-driven consumer swaps its whole
// snapshot instead of applying a delta.
type modelsChangedParams struct {
	Engine string       `json:"engine"`
	Models ModelsResult `json:"models"`
}

// residencyNeutralActions are engine actions that only read state and so can
// never change a model list. pokeLoaded is skipped after them (see
// Manager.runAction) to avoid a wasted out-of-cycle sweep. Anything not listed
// here — load_model/unload_model/run_model/pull_model/delete_model, and chat,
// which can JIT-load a model — is treated as potentially changing one.
var residencyNeutralActions = map[string]bool{
	"list_models":     true,
	"list_downloaded": true,
	"loaded_models":   true,
}

// watchLoaded polls the model lists of every engine and emits
// engine:models-changed whenever one changes — covering explicit load/unload, LM
// Studio JIT auto-load, TTL/idle auto-eviction, and a model pulled or deleted.
// The engines themselves push nothing, so this internal poll is the only signal;
// keeping it here means a push-driven consumer never has to poll. It runs until
// ctx is cancelled and is pokeable (see Executor.pokeLoaded) for sub-tick
// latency after an explicit action. The first sweep only seeds the baseline (no emit) so a consumer's
// initial engine:models fetch isn't immediately shadowed by a redundant push.
func (e *Executor) watchLoaded(ctx context.Context) {
	interval := e.loadedPollInterval
	if interval <= 0 {
		return // watcher disabled
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	var prev modelLists
	seeded := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.loadedPoke:
		}
		changed, next, res := e.sweepModels(ctx, prev)
		prev = next
		if !seeded {
			seeded = true
			continue // first sweep only seeds the baseline
		}
		for _, name := range changed {
			e.notify(modelsChangedMethod, modelsChangedParams{Engine: name, Models: res})
		}
	}
}

// modelLists is the watcher's baseline: the last-good value of each per-engine
// list in a ModelsResult, and the engines whose downloaded list the last sweep
// could not read.
type modelLists struct {
	advertised, loaded, downloaded map[string][]string
	downloadedMissed               map[string]bool
}

// sweepModels performs one poll: it fetches the current model result and
// computes which engines changed relative to prev in any of the lists a consumer
// shows — what is served (ByEngine), loaded (LoadedByEngine) or downloaded
// (DownloadedByEngine). The loaded set alone misses a pull or delete, and for an
// engine that advertises only what it serves (vLLM) the downloaded list changes
// on its own. It returns those engine names, the refreshed baseline to carry
// into the next sweep, and the full result (the push payload). In each list an
// engine absent from this sweep keeps its last-good entry (see changedEngines),
// so a transient miss isn't mistaken for a change. It never mutates prev. Split
// out of watchLoaded so the diff logic is unit-testable without goroutines or
// timers.
func (e *Executor) sweepModels(ctx context.Context, prev modelLists) (changed []string, next modelLists, res ModelsResult) {
	res = e.ModelsResult(ctx)
	seen := map[string]bool{}
	sweep := func(prev, cur map[string][]string) map[string][]string {
		for _, name := range changedEngines(prev, cur) {
			if !seen[name] {
				seen[name] = true
				changed = append(changed, name)
			}
		}
		next := make(map[string][]string, len(prev)+len(cur))
		maps.Copy(next, prev)
		maps.Copy(next, cur)
		return next
	}
	next = modelLists{
		advertised: sweep(prev.advertised, res.ByEngine),
		loaded:     sweep(prev.loaded, res.LoadedByEngine),
		downloaded: sweep(prev.downloaded, res.DownloadedByEngine),
	}
	// A push made while an engine's downloaded list could not be read carries
	// no list for it, so a consumer drops its rows. When the list reads again,
	// push once even if it matches the last-good one, or those rows stay
	// missing until something else changes.
	for name := range prev.downloaded {
		if _, ok := res.DownloadedByEngine[name]; !ok {
			if next.downloadedMissed == nil {
				next.downloadedMissed = map[string]bool{}
			}
			next.downloadedMissed[name] = true
		}
	}
	for name := range prev.downloadedMissed {
		if _, ok := res.DownloadedByEngine[name]; ok && !seen[name] {
			seen[name] = true
			changed = append(changed, name)
		}
	}
	slices.Sort(changed)
	return changed, next, res
}

// pokeLoaded requests an immediate model-list check (non-blocking, coalesced).
// Called after an action that may change a list (load/unload/pull/delete/…) so the
// engine:models-changed push doesn't wait up to a full poll interval.
func (e *Executor) pokeLoaded() {
	select {
	case e.loadedPoke <- struct{}{}:
	default:
	}
}

// changedEngines returns, sorted, the engine names whose list was reported
// this sweep (present in cur) and differs from the baseline prev — either a new
// key or a key whose set changed order-insensitively.
//
// A key present only in prev is deliberately NOT reported. The watcher can't
// tell an engine that genuinely stopped from one whose loaded_models query
// merely blipped this sweep (both drop the key), and a real stop already reaches
// consumers via engine:state-changed. Treating every disappearance as a change
// would emit spurious engine:models-changed churn — a drop then a re-add — on a
// single transient failure, so the watcher keeps such keys as last-good (see
// sweepModels) and waits for a definitive reading.
func changedEngines(prev, cur map[string][]string) []string {
	var changed []string
	for name, cv := range cur {
		if pv, ok := prev[name]; !ok || !sameStringSet(pv, cv) {
			changed = append(changed, name)
		}
	}
	slices.Sort(changed)
	return changed
}

// sameStringSet reports whether a and b contain the same elements, ignoring
// order and treating nil and empty as equal.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, s := range a {
		counts[s]++
	}
	for _, s := range b {
		counts[s]--
		if counts[s] < 0 {
			return false
		}
	}
	return true
}
