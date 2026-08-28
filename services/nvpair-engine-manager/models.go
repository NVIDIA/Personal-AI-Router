// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"
)

// modelsTimeout bounds the whole engine:models sweep so one hung engine can't
// stall the model list. list_models is a cheap loopback GET, so this is short
// (unlike the 30-minute engine:action ceiling that also has to cover pulls).
const modelsTimeout = 5 * time.Second

// ModelsResult is the model payload the LAN HTTP surface (/v1/models) and the
// engine:models RPC return: the flat, de-duplicated union of every running
// engine's models plus the per-engine attribution keyed by engine name (e.g.
// "ollama", "lmstudio"). Models is retained unchanged for consumers that only
// need the node-level set; ByEngine is the additive attribution a per-engine
// consumer needs (a node's engine card, and the proxies' own-engine model-owner
// ranking). ByEngine includes every running engine whose inventory was
// successfully queried, so an empty list means "running, no models available"
// while a missing key means "not running / not queryable".
//
// LoadedByEngine names the models currently resident in memory, per engine —
// normally a subset of that engine's ByEngine list. It is populated for every
// running engine that declares a loaded_models action AND was successfully
// queried, so an engine key with an empty list means "running, nothing loaded"
// while a MISSING key means "not running / not queryable / no loaded endpoint" —
// a distinction a consumer needs to reflect real state. The subset
// relationship is best-effort: list_models and loaded_models are queried
// independently, so in the rare sweep where an engine's list_models fails but
// loaded_models succeeds, a loaded name can appear here without a matching
// ByEngine/Models entry until the next successful sweep. Its own omitempty drops
// the whole map when no engine reports loaded state.
type ModelsResult struct {
	Models         []string            `json:"models"`
	ByEngine       map[string][]string `json:"modelsByEngine,omitempty"`
	LoadedByEngine map[string][]string `json:"loadedByEngine,omitempty"`
}

// Models returns the union of model names served by every installed, running
// engine that declares a list_models action with a result-extraction spec —
// the flat node-level list. It is a thin wrapper over {@link ModelsResult} kept
// for callers that only need the union (and the existing test surface).
func (e *Executor) Models(ctx context.Context) []string {
	return e.ModelsResult(ctx).Models
}

// ModelsResult queries each installed, running engine's loopback list_models
// endpoint (reusing the manifest action machinery), normalizes the
// engine-specific shapes, and returns both the flat de-duplicated union and the
// per-engine attribution — from a single sweep, so a caller needing both
// (the /v1/models HTTP surface, the engine:models RPC) never sweeps twice.
// Stopped engines contribute nothing — their model list isn't queryable while
// the server is down. Best-effort: a per-engine failure is logged and skipped
// so the engines that did answer still surface their models.
//
// The per-engine queries run CONCURRENTLY. A host running several engines (e.g.
// Ollama + LM Studio) must not have the later engines squeezed out of a single
// shared deadline: sequential queries under one budget meant a slow first engine
// left little for the second, and the peer's discovery daemon fetches this over
// HTTP with its own (shorter) timeout, so a squeezed second engine silently
// dropped its models for peers. Concurrency bounds the sweep by the slowest
// single engine, not the sum.
func (e *Executor) ModelsResult(ctx context.Context) ModelsResult {
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()

	engineNames := e.reg.Names()
	// Results indexed by engine position so the flattened output stays
	// deterministic (registration order) despite concurrent completion.
	perEngine := make([][]string, len(engineNames))
	listOK := make([]bool, len(engineNames))
	loaded := make([][]string, len(engineNames))
	loadedOK := make([]bool, len(engineNames))
	var wg sync.WaitGroup
	for i, name := range engineNames {
		mf, ok := e.reg.Get(name)
		if !ok {
			continue
		}
		var listSpec, loadedSpec *ActionResult
		if act, ok := mf.Actions["list_models"]; ok {
			listSpec = act.Result
		}
		if act, ok := mf.Actions["loaded_models"]; ok {
			loadedSpec = act.Result
		}
		if listSpec == nil && loadedSpec == nil {
			continue
		}
		wg.Add(1)
		go func(i int, name string, listSpec, loadedSpec *ActionResult) {
			defer wg.Done()
			st, err := e.Status(name)
			if err != nil || !st.Running {
				return
			}
			if listSpec != nil {
				if raw, err := e.Action(ctx, name, "list_models", nil); err != nil {
					slog.Debug("engine:models list_models failed", "engine", name, "err", err)
				} else if models, ok := extractStringsResult(raw, listSpec); ok {
					perEngine[i] = models
					listOK[i] = true
				} else {
					slog.Debug("engine:models list_models returned an invalid inventory", "engine", name)
				}
			}
			if loadedSpec != nil {
				if raw, err := e.Action(ctx, name, "loaded_models", nil); err != nil {
					slog.Debug("engine:models loaded_models failed", "engine", name, "err", err)
				} else if models, ok := extractStringsResult(raw, loadedSpec); ok {
					// A successful query — even an empty result — records the key
					// so a consumer can tell "running, nothing loaded" apart from
					// "unknown". An action or shape error leaves loadedOK[i] false.
					loaded[i] = models
					loadedOK[i] = true
				} else {
					slog.Debug("engine:models loaded_models returned an invalid inventory", "engine", name)
				}
			}
		}(i, name, listSpec, loadedSpec)
	}
	wg.Wait()

	res := ModelsResult{Models: []string{}}
	seen := map[string]bool{}
	for i, models := range perEngine {
		if listOK[i] {
			if res.ByEngine == nil {
				res.ByEngine = make(map[string][]string)
			}
			// Normalize nil -> [] so the wire distinguishes a successful empty
			// inventory from a missing/not-queryable engine.
			if models == nil {
				models = []string{}
			}
			res.ByEngine[engineNames[i]] = models
			for _, m := range models {
				if m != "" && !seen[m] {
					seen[m] = true
					res.Models = append(res.Models, m)
				}
			}
		}
		if loadedOK[i] {
			if res.LoadedByEngine == nil {
				res.LoadedByEngine = make(map[string][]string)
			}
			// Normalize nil -> [] so the wire carries an empty array ("queried,
			// nothing resident"), never null.
			ld := loaded[i]
			if ld == nil {
				ld = []string{}
			}
			res.LoadedByEngine[engineNames[i]] = ld
		}
	}
	return res
}

// extractStrings is the value-only wrapper used by focused extractor tests.
// Callers that must distinguish a valid empty inventory from an invalid
// response use extractStringsResult.
func extractStrings(raw json.RawMessage, spec *ActionResult) []string {
	out, ok := extractStringsResult(raw, spec)
	if !ok || len(out) == 0 {
		return nil
	}
	return out
}

// extractStringsResult pulls spec.Field from each element of the required
// top-level spec.Array. A present empty array is a successful, authoritative
// empty inventory. A missing, null, or wrong-typed array is unknown and returns
// ok=false so callers do not label it authoritative empty. Malformed rows
// inside a valid array are skipped, preserving additive response compatibility. For an
// unfiltered inventory, however, a non-empty array with no usable names is
// malformed rather than authoritative empty.
func extractStringsResult(raw json.RawMessage, spec *ActionResult) ([]string, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	arrRaw, ok := obj[spec.Array]
	if !ok || bytes.Equal(bytes.TrimSpace(arrRaw), []byte("null")) {
		return nil, false
	}
	var arr []map[string]json.RawMessage
	if err := json.Unmarshal(arrRaw, &arr); err != nil {
		return nil, false
	}
	out := make([]string, 0)
	for _, el := range arr {
		if spec.Match != nil && !matchRow(el, spec.Match) {
			continue
		}
		fv, ok := el[spec.Field]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(fv, &s); err == nil && s != "" {
			out = append(out, s)
		}
	}
	if spec.Match == nil && len(arr) > 0 && len(out) == 0 {
		return nil, false
	}
	return out, true
}

// matchRow reports whether an element passes an ActionResult row filter.
// With Match.In set, Match.Field must decode as a JSON string equal to one of
// In. With Match.Nonempty set, Match.Field must decode as a JSON array with
// length > 0 (LM Studio /api/v1/models loaded_instances). A missing or
// wrong-typed field fails the match, so a row we cannot classify is excluded
// rather than counted as loaded.
func matchRow(el map[string]json.RawMessage, m *ResultMatch) bool {
	fv, ok := el[m.Field]
	if !ok {
		return false
	}
	if m.Nonempty {
		var arr []json.RawMessage
		if err := json.Unmarshal(fv, &arr); err != nil {
			return false
		}
		return len(arr) > 0
	}
	var s string
	if err := json.Unmarshal(fv, &s); err != nil {
		return false
	}
	for _, want := range m.In {
		if s == want {
			return true
		}
	}
	return false
}
