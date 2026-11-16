// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
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
//
// DownloadedByEngine is the model inventory of an engine that advertises only
// what it is serving (advertised_models "loaded", such as vLLM): ByEngine holds
// the names a request may be routed to, and this holds every model the engine
// can load, so a model manager keeps its rows for models that are on disk but
// not being served. It is read whenever the engine is installed, running or
// not, because such an engine's list_models needs no running server. Its
// LoadedByEngine entry is present and empty while it is stopped, and names
// models in this list's terms (loaded_models'
// result.inventory_field), so a model served under an alias still marks its
// cached row as loaded. An empty list means the inventory was read and is empty;
// a missing key means it was not read (not installed, or the read failed). An
// engine whose advertised list is its inventory has no entry here.
type ModelsResult struct {
	Models             []string            `json:"models"`
	ByEngine           map[string][]string `json:"modelsByEngine,omitempty"`
	LoadedByEngine     map[string][]string `json:"loadedByEngine,omitempty"`
	DownloadedByEngine map[string][]string `json:"downloadedByEngine,omitempty"`
}

// Models returns the union of model names served by every installed, running
// engine that declares a list_models action with a result-extraction spec —
// the flat node-level list. It is a thin wrapper over {@link ModelsResult} kept
// for callers that only need the union (and the existing test surface).
func (e *Executor) Models(ctx context.Context) []string {
	return e.ModelsResult(ctx).Models
}

// engineModels is one engine's answer within a ModelsResult sweep. Each list
// has an ok flag because an empty list read successfully and a list that could
// not be read mean different things on the wire.
type engineModels struct {
	advertised, loaded, downloaded       []string
	advertisedOK, loadedOK, downloadedOK bool
	statusErr                            error
}

// ModelsResult queries each installed, running engine's loopback list_models
// endpoint (reusing the manifest action machinery), normalizes the
// engine-specific shapes, and returns both the flat de-duplicated union and the
// per-engine attribution — from a single sweep, so a caller needing both
// (the /v1/models HTTP surface, the engine:models RPC) never sweeps twice.
// Stopped engines contribute nothing — their model list isn't queryable while
// the server is down — except the DownloadedByEngine inventory of an engine
// that advertises its loaded set, which is read from disk. Best-effort: a
// per-engine failure is logged and skipped so the engines that did answer
// still surface their models.
//
// Queries run concurrently so a slow engine does not consume the time available
// to the others. The five-second limit covers status checks and model-list
// requests for every engine.
func (e *Executor) ModelsResult(ctx context.Context) ModelsResult {
	res, _ := e.modelsResult(ctx)
	return res
}

// modelsResult also reports a canceled sweep to HTTP callers, which must not
// replace a peer's last-good inventory with a successful partial response.
func (e *Executor) modelsResult(ctx context.Context) (ModelsResult, error) {
	ctx, cancel := context.WithTimeout(ctx, modelsTimeout)
	defer cancel()

	engineNames := e.reg.Names()
	// Results indexed by engine position so the flattened output stays
	// deterministic (registration order) despite concurrent completion.
	perEngine := make([]engineModels, len(engineNames))
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
		wg.Go(func() {
			// Unsupported engines are omitted, as in get-installed.
			if _, err := e.state(name); err != nil {
				return
			}
			st, err := e.statusWithContext(ctx, name, 0, true)
			if err != nil {
				perEngine[i].statusErr = err
				return
			}
			// An engine that can only answer for what it is running advertises its
			// loaded set. Its list_models is a view of what could be loaded, which
			// is not what a peer or the proxy may route to, so it is reported apart.
			if mf.AdvertisedModels == advertisedModelsLoaded && loadedSpec != nil {
				perEngine[i] = e.loadedOnlyEngineModels(ctx, name, st, listSpec, loadedSpec)
				return
			}
			if !st.Running {
				return
			}
			var r engineModels
			if listSpec != nil {
				r.advertised, r.advertisedOK = e.queryModelList(ctx, name, "list_models", listSpec)
			}
			if loadedSpec != nil {
				// A successful query — even an empty result — records the key so a
				// consumer can tell "running, nothing loaded" apart from "unknown".
				r.loaded, r.loadedOK = e.queryModelList(ctx, name, "loaded_models", loadedSpec)
			}
			perEngine[i] = r
		})
	}
	wg.Wait()

	res := ModelsResult{Models: []string{}}
	seen := map[string]bool{}
	for i, r := range perEngine {
		name := engineNames[i]
		if r.advertisedOK {
			res.ByEngine = withModels(res.ByEngine, name, r.advertised)
			for _, m := range r.advertised {
				if m != "" && !seen[m] {
					seen[m] = true
					res.Models = append(res.Models, m)
				}
			}
		}
		if r.loadedOK {
			res.LoadedByEngine = withModels(res.LoadedByEngine, name, r.loaded)
		}
		if r.downloadedOK {
			res.DownloadedByEngine = withModels(res.DownloadedByEngine, name, r.downloaded)
		}
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	for _, result := range perEngine {
		if result.statusErr != nil {
			return res, result.statusErr
		}
	}
	return res, nil
}

// loadedOnlyEngineModels answers for an engine that advertises its loaded set.
// Its inventory comes from list_models whenever it is installed, since that
// list is read from disk and needs no running server, and while it is stopped
// it reports nothing loaded. While it runs, one
// loaded_models response gives both the names it answers to (result.field,
// such as vLLM's served name) and, when result.inventory_field is set, the same
// models in the inventory's terms (vLLM's --model).
func (e *Executor) loadedOnlyEngineModels(ctx context.Context, name string, st EngineStatus, listSpec, loadedSpec *ActionResult) engineModels {
	var r engineModels
	if !st.Installed {
		return r
	}
	if listSpec != nil {
		r.downloaded, r.downloadedOK = e.queryModelList(ctx, name, "list_models", listSpec)
	}
	if !st.Running {
		// A stopped engine of this kind has nothing loaded, which is known, not
		// unknown: its rows show as not loaded rather than unreported.
		r.loadedOK = true
		return r
	}
	raw, err := e.Action(ctx, name, "loaded_models", nil)
	if err != nil {
		slog.Debug("engine:models loaded_models failed", "engine", name, "err", err)
		return r
	}
	r.advertised, r.advertisedOK = extractStringsResult(raw, loadedSpec)
	r.loaded, r.loadedOK = r.advertised, r.advertisedOK
	if loadedSpec.InventoryField != "" {
		inventorySpec := *loadedSpec
		inventorySpec.Field = loadedSpec.InventoryField
		r.loaded, r.loadedOK = extractStringsResult(raw, &inventorySpec)
	}
	if !r.advertisedOK || !r.loadedOK {
		slog.Debug("engine:models loaded_models returned an invalid inventory", "engine", name)
	}
	return r
}

// queryModelList runs one model-list action and extracts its names. ok is
// false when the action failed or its response was not a valid list.
func (e *Executor) queryModelList(ctx context.Context, name, action string, spec *ActionResult) ([]string, bool) {
	raw, err := e.Action(ctx, name, action, nil)
	if err != nil {
		slog.Debug("engine:models action failed", "engine", name, "action", action, "err", err)
		return nil, false
	}
	models, ok := extractStringsResult(raw, spec)
	if !ok {
		slog.Debug("engine:models action returned an invalid inventory", "engine", name, "action", action)
	}
	return models, ok
}

// withModels records one engine's list in a per-engine map, creating the map on
// first use. A nil list is stored as [] so the wire carries an empty array
// ("read, nothing there"), never null.
func withModels(m map[string][]string, engine string, models []string) map[string][]string {
	if m == nil {
		m = make(map[string][]string)
	}
	if models == nil {
		models = []string{}
	}
	m[engine] = models
	return m
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
// Match.Field is a validated dot-separated object path. With Match.In set, the
// resolved value must decode as a JSON string equal to one of In. With
// Match.Nonempty set, it must decode as a JSON array with length > 0 (LM Studio
// /api/v1/models loaded_instances). A missing or wrong-typed path fails the
// match, so a row we cannot classify is excluded rather than counted as loaded.
func matchRow(el map[string]json.RawMessage, m *ResultMatch) bool {
	fv, ok := resolveObjectPath(el, m.Field)
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
	return slices.Contains(m.In, s)
}

func resolveObjectPath(obj map[string]json.RawMessage, path string) (json.RawMessage, bool) {
	parts := strings.Split(path, ".")
	value, ok := obj[parts[0]]
	for _, part := range parts[1:] {
		if !ok {
			return nil, false
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(value, &nested); err != nil {
			return nil, false
		}
		value, ok = nested[part]
	}
	return value, ok
}
