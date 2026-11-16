// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Engines use different inventory shapes and loaded-model markers. Only rows
// matching the configured path and filter may become model identifiers.
func TestExtractStrings_ReadsConfiguredResultPath(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		spec *ActionResult
		want []string
	}{
		{
			name: "ollama tags shape",
			raw:  `{"models":[{"name":"llama3:8b","model":"llama3:8b"},{"name":"qwen:0.5b"}]}`,
			spec: &ActionResult{Array: "models", Field: "name"},
			want: []string{"llama3:8b", "qwen:0.5b"},
		},
		{
			name: "lmstudio v1/models shape",
			raw:  `{"object":"list","data":[{"id":"phi-3"},{"id":"gemma-2b"}]}`,
			spec: &ActionResult{Array: "data", Field: "id"},
			want: []string{"phi-3", "gemma-2b"},
		},
		{
			name: "lmstudio native models shape",
			raw:  `{"models":[{"key":"phi-3","loaded_instances":[]},{"key":"gemma-2b","loaded_instances":[]}]}`,
			spec: &ActionResult{Array: "models", Field: "key"},
			want: []string{"phi-3", "gemma-2b"},
		},
		{
			name: "missing array yields nothing",
			raw:  `{"other":[]}`,
			spec: &ActionResult{Array: "models", Field: "name"},
			want: nil,
		},
		{
			name: "elements missing the field are skipped",
			raw:  `{"models":[{"name":"a"},{"other":"b"},{"name":""}]}`,
			spec: &ActionResult{Array: "models", Field: "name"},
			want: []string{"a"},
		},
		{
			name: "non-object response yields nothing",
			raw:  `["a","b"]`,
			spec: &ActionResult{Array: "models", Field: "name"},
			want: nil,
		},
		{
			name: "string-in match keeps only matching rows",
			raw:  `{"data":[{"id":"a","state":"loaded"},{"id":"b","state":"not-loaded"},{"id":"c","state":"loaded"}]}`,
			spec: &ActionResult{Array: "data", Field: "id", Match: &ResultMatch{Field: "state", In: []string{"loaded"}}},
			want: []string{"a", "c"},
		},
		{
			name: "match: rows missing the state field are excluded",
			raw:  `{"data":[{"id":"a","state":"loaded"},{"id":"b"}]}`,
			spec: &ActionResult{Array: "data", Field: "id", Match: &ResultMatch{Field: "state", In: []string{"loaded"}}},
			want: []string{"a"},
		},
		{
			name: "nested string-in match excludes missing and malformed paths",
			raw:  `{"data":[{"id":"a","status":{"value":"loaded"}},{"id":"b","status":{"value":"not-loaded"}},{"id":"c","status":{}},{"id":"d","status":"loaded"}]}`,
			spec: &ActionResult{Array: "data", Field: "id", Match: &ResultMatch{Field: "status.value", In: []string{"loaded"}}},
			want: []string{"a"},
		},
		{
			name: "match with no accepted values yields nothing",
			raw:  `{"data":[{"id":"a","state":"loaded"}]}`,
			spec: &ActionResult{Array: "data", Field: "id", Match: &ResultMatch{Field: "state", In: []string{"other"}}},
			want: nil,
		},
		{
			name: "lmstudio v1 nonempty loaded_instances keeps only loaded rows",
			raw:  `{"models":[{"key":"a","loaded_instances":[{"id":"a"}]},{"key":"b","loaded_instances":[]},{"key":"c","loaded_instances":[{"id":"c"}]}]}`,
			spec: &ActionResult{Array: "models", Field: "key", Match: &ResultMatch{Field: "loaded_instances", Nonempty: true}},
			want: []string{"a", "c"},
		},
		{
			name: "nonempty match: missing or non-array field is excluded",
			raw:  `{"models":[{"key":"a","loaded_instances":[{"id":"a"}]},{"key":"b"},{"key":"c","loaded_instances":"bad"}]}`,
			spec: &ActionResult{Array: "models", Field: "key", Match: &ResultMatch{Field: "loaded_instances", Nonempty: true}},
			want: []string{"a"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, extractStrings(json.RawMessage(tc.raw), tc.spec))
		})
	}
}

func TestExtractStringsResult_DistinguishesEmptyFromUnknown(t *testing.T) {
	spec := &ActionResult{Array: "models", Field: "key"}
	got, ok := extractStringsResult(json.RawMessage(`{"models":[]}`), spec)
	require.True(t, ok, "explicit empty inventory (%v, %v)", got, ok)
	require.NotNil(t, got, "explicit empty inventory must serialize as []")
	assert.Empty(t, got, "explicit empty inventory")
	test := func(name, raw string) {
		t.Run(name, func(t *testing.T) {
			got, ok := extractStringsResult(json.RawMessage(raw), spec)
			assert.False(t, ok, "invalid inventory must remain unknown")
			assert.Nil(t, got, "invalid inventory must not return models")
		})
	}
	test("missing models", `{}`)
	test("null models", `{"models":null}`)
	test("object models", `{"models":{}}`)
	test("missing model key", `{"models":[{}]}`)
	test("null model entry", `{"models":[null]}`)
	test("invalid JSON", `not-json`)
}

// TestModels drives Models() against the running fake engine: a running engine
// with a Result-bearing list_models action contributes its models; a stopped
// engine contributes nothing.
func TestModels_ReturnsInventoryOnlyForRunningEngines(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	m.Actions["list_models"] = Action{
		HTTP:   &ActionHTTP{Method: "GET", Path: "/api/tags"},
		Result: &ActionResult{Array: "models", Field: "name"},
	}
	ex := newTestExecutor(t, m)
	ctx := context.Background()
	t.Cleanup(func() { _ = ex.Stop("fake") })

	// Stopped: nothing queryable.
	require.Empty(t, ex.Models(ctx), "Models() on stopped engine")

	require.NoError(t, ex.Start(ctx, "fake"), "start")
	// The fake engine's seeded model.
	require.Equal(t, []string{"llama3.2:1b"}, ex.Models(ctx), "Models() on running engine")

	require.NoError(t, ex.Stop("fake"), "stop")
	require.Empty(t, ex.Models(ctx), "Models() after stop")
}

// TestModelsResult drives ModelsResult() against the running fake engine: the
// flat union and the per-engine attribution are both derived from one sweep,
// and a stopped engine yields neither.
func TestModelsResult_GroupsRunningEngineInventory(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	m.Actions["list_models"] = Action{
		HTTP:   &ActionHTTP{Method: "GET", Path: "/api/tags"},
		Result: &ActionResult{Array: "models", Field: "name"},
	}
	m.Actions["delete_model"] = Action{HTTP: &ActionHTTP{Method: "DELETE", Path: "/api/delete"}}
	ex := newTestExecutor(t, m)
	ctx := context.Background()
	t.Cleanup(func() { _ = ex.Stop("fake") })

	// Stopped: empty union, no attribution map.
	res := ex.ModelsResult(ctx)
	require.Empty(t, res.Models, "ModelsResult().Models on stopped engine")
	require.Empty(t, res.ByEngine, "ModelsResult().ByEngine on stopped engine")

	require.NoError(t, ex.Start(ctx, "fake"), "start")
	res = ex.ModelsResult(ctx)
	// The fake engine's seeded model.
	require.Equal(t, []string{"llama3.2:1b"}, res.Models, "ModelsResult().Models")
	require.Equal(t, map[string][]string{"fake": {"llama3.2:1b"}}, res.ByEngine, "ModelsResult().ByEngine")

	// Deleting the last model is a successful empty inventory, not an unknown
	// response: preserve the engine key so consumers can clear stale state.
	_, err := ex.Action(ctx, "fake", "delete_model", json.RawMessage(`{"name":"llama3.2:1b"}`))
	require.NoError(t, err, "delete last model")
	res = ex.ModelsResult(ctx)
	require.Empty(t, res.Models, "ModelsResult().Models after last delete")
	require.Equal(t, map[string][]string{"fake": {}}, res.ByEngine, "ModelsResult().ByEngine after last delete")
}

// loadedActionManifest is testEngineManifest with both list_models (/api/tags)
// and loaded_models (/api/ps) actions declared, so the executor surfaces
// LoadedByEngine from the fake engine's resident set.
func loadedActionManifest() *Manifest {
	m := testEngineManifest(fakeEngineBin)
	m.Actions["list_models"] = Action{
		HTTP:   &ActionHTTP{Method: "GET", Path: "/api/tags"},
		Result: &ActionResult{Array: "models", Field: "name"},
	}
	m.Actions["loaded_models"] = Action{
		HTTP:   &ActionHTTP{Method: "GET", Path: "/api/ps"},
		Result: &ActionResult{Array: "models", Field: "name"},
	}
	return m
}

// setLoaded drives the fake engine's resident set via its /testctl/loaded
// endpoint, standing in for an explicit load/unload or a TTL eviction.
func setLoaded(t *testing.T, ex *Executor, names []string) {
	t.Helper()
	st, err := ex.Status("fake")
	require.NoError(t, err, "status")
	body, err := json.Marshal(map[string][]string{"names": names})
	require.NoError(t, err)
	url := fmt.Sprintf("http://127.0.0.1:%d/testctl/loaded", st.Port)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	require.NoError(t, err, "set loaded")
	require.NoError(t, resp.Body.Close())
}

// TestModelsResultLoaded covers the loaded surface: a running engine that
// declares loaded_models reports its resident subset in LoadedByEngine, and an
// eviction leaves the engine key present with an empty list ("running, nothing
// loaded") while the installed list is unchanged.
func TestModelsResult_TracksLoadedModelsWithoutChangingInventory(t *testing.T) {
	ex := newTestExecutor(t, loadedActionManifest())
	ctx := context.Background()
	t.Cleanup(func() { _ = ex.Stop("fake") })

	require.NoError(t, ex.Start(ctx, "fake"), "start")
	res := ex.ModelsResult(ctx)
	require.Equal(t, map[string][]string{"fake": {"llama3.2:1b"}}, res.LoadedByEngine, "LoadedByEngine")

	// Evict everything: the key stays with an empty list.
	setLoaded(t, ex, nil)
	res = ex.ModelsResult(ctx)
	require.Equal(t, map[string][]string{"fake": {}}, res.LoadedByEngine, "LoadedByEngine after evict")
	require.Equal(t, []string{"llama3.2:1b"}, res.Models, "Models after evict")
}

// TestModelsResultNoLoadedActionOmitsKey confirms an engine with no loaded_models
// action contributes no LoadedByEngine key (the map stays nil), so the field is
// omitted on the wire for engines that don't support loaded reporting.
func TestModelsResultNoLoadedAction_OmitsKey(t *testing.T) {
	m := testEngineManifest(fakeEngineBin)
	m.Actions["list_models"] = Action{
		HTTP:   &ActionHTTP{Method: "GET", Path: "/api/tags"},
		Result: &ActionResult{Array: "models", Field: "name"},
	}
	// No loaded_models action.
	ex := newTestExecutor(t, m)
	ctx := context.Background()
	t.Cleanup(func() { _ = ex.Stop("fake") })
	require.NoError(t, ex.Start(ctx, "fake"), "start")
	require.Nil(t, ex.ModelsResult(ctx).LoadedByEngine)
}

// TestSweepModels_SeedsThenEmitsOnChange covers the watcher's core diff without
// goroutines or timers: the first sweep only seeds a baseline (the caller,
// watchLoaded, suppresses its emit), a subsequent identical sweep reports no
// change, and a sweep after an eviction reports the engine as changed and
// carries the fresh full model result as the push payload.
func TestSweepModels_SeedsThenEmitsOnChange(t *testing.T) {
	ex := newTestExecutor(t, loadedActionManifest())
	ctx := t.Context()
	t.Cleanup(func() { _ = ex.Stop("fake") })
	require.NoError(t, ex.Start(ctx, "fake"), "start")

	// Seed sweep: the loaded baseline is {fake:[llama3.2:1b]}.
	_, prev, _ := ex.sweepModels(ctx, modelLists{})
	require.Equal(t, map[string][]string{"fake": {"llama3.2:1b"}}, prev.loaded, "seed baseline")

	// No change -> no engine reported changed.
	changed, prev, _ := ex.sweepModels(ctx, prev)
	require.Empty(t, changed, "unchanged sweep reported")

	// Evict everything -> fake changes; payload carries the empty loaded set.
	setLoaded(t, ex, nil)
	changed, _, res := ex.sweepModels(ctx, prev)
	require.Equal(t, []string{"fake"}, changed, "changed")
	require.Equal(t, map[string][]string{"fake": {}}, res.LoadedByEngine, "pushed LoadedByEngine")
}

// A pull or delete changes what an engine has, not what it has loaded. The push
// has to follow it too, or a client that waits for pushes (the terminal
// interface) keeps showing the old list until it asks again.
func TestSweepModels_EmitsWhenAModelIsPulledOrDeleted(t *testing.T) {
	test := func(name string, manifest func(t *testing.T) *Manifest, start bool, list func(ModelsResult) []string) {
		t.Run(name, func(t *testing.T) {
			m := manifest(t)
			cache := m.Actions["list_models"].HFCache.Dir
			ex := newTestExecutor(t, m)
			ctx := t.Context()
			if start {
				t.Cleanup(func() { _ = ex.Stop("fake") })
				require.NoError(t, ex.Start(ctx, "fake"))
			}
			_, prev, _ := ex.sweepModels(ctx, modelLists{})

			writeHFRepo(t, cache, "org/pulled-model", llmFiles(map[string]string{"model.safetensors": "w"}))
			changed, prev, res := ex.sweepModels(ctx, prev)
			assert.Equal(t, []string{"fake"}, changed, "after the pull")
			assert.Contains(t, list(res), "org/pulled-model", "the pushed list")

			require.NoError(t, os.RemoveAll(filepath.Join(cache, "models--org--pulled-model")))
			changed, _, res = ex.sweepModels(ctx, prev)
			assert.Equal(t, []string{"fake"}, changed, "after the delete")
			assert.NotContains(t, list(res), "org/pulled-model", "the pushed list")
		})
	}
	test("a stopped engine that advertises only what it serves, such as vLLM",
		func(t *testing.T) *Manifest { return loadedOnlyManifest(t, fakeEngineBin) }, false,
		func(r ModelsResult) []string { return r.DownloadedByEngine["fake"] })
	test("a running engine that advertises what it has",
		func(t *testing.T) *Manifest {
			m := loadedOnlyManifest(t, fakeEngineBin)
			m.AdvertisedModels = ""
			delete(m.Actions, "loaded_models")
			require.NoError(t, m.Validate())
			return m
		}, true,
		func(r ModelsResult) []string { return r.ByEngine["fake"] })
}

// While vLLM's cache cannot be read, a push caused by another engine carries no
// vLLM list, and a consumer drops those rows. When the cache reads again the
// list matches the last-good one, but it still has to be pushed once.
func TestSweepModels_PushesADownloadedListThatReadsAgainUnchanged(t *testing.T) {
	m := loadedOnlyManifest(t, fakeEngineBin)
	cache := m.Actions["list_models"].HFCache.Dir
	ex := newTestExecutor(t, m)
	ctx := t.Context()
	_, prev, _ := ex.sweepModels(ctx, modelLists{})
	require.Equal(t, []string{"other/cached-model"}, prev.downloaded["fake"], "seed baseline")

	require.NoError(t, os.Chmod(cache, 0))
	t.Cleanup(func() { _ = os.Chmod(cache, 0o755) })
	changed, prev, res := ex.sweepModels(ctx, prev)
	if _, read := res.DownloadedByEngine["fake"]; read {
		t.Skip("this system can still read a directory with no permissions")
	}
	assert.Empty(t, changed, "a cache that cannot be read is not a change")

	require.NoError(t, os.Chmod(cache, 0o755))
	changed, prev, res = ex.sweepModels(ctx, prev)
	assert.Equal(t, []string{"fake"}, changed, "the list read again")
	assert.Equal(t, []string{"other/cached-model"}, res.DownloadedByEngine["fake"], "the pushed list")

	changed, _, _ = ex.sweepModels(ctx, prev)
	assert.Empty(t, changed, "pushed once, not on every sweep after")
}

// TestSweepModels_RetainsLastGoodOnTransientMiss covers the anti-churn guard: an
// engine that drops out of a sweep (a transient loaded_models miss, indistinct
// from a stop) is NOT reported as changed and keeps its last-good baseline, so a
// single blip can't emit a spurious drop-then-re-add pair.
func TestSweepModels_RetainsLastGoodOnTransientMiss(t *testing.T) {
	ex := newTestExecutor(t, loadedActionManifest())
	prev := modelLists{
		advertised: map[string][]string{"fake": {"llama3.2:1b"}},
		loaded:     map[string][]string{"fake": {"llama3.2:1b"}},
		downloaded: map[string][]string{},
	}
	// The engine isn't started, so ModelsResult reports it neither running nor
	// queryable: no list has a "fake" key this sweep.
	changed, next, _ := ex.sweepModels(t.Context(), prev)
	require.Empty(t, changed, "a disappeared engine reported")
	require.Equal(t, prev, next, "baseline after miss")
}

func TestChangedEngines_IdentifiesInventoryDifferences(t *testing.T) {
	test := func(name string, prev, cur map[string][]string, want []string) {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, want, changedEngines(prev, cur))
		})
	}
	test("no change", map[string][]string{"a": {"x"}}, map[string][]string{"a": {"x"}}, nil)
	test("reorder is not a change", map[string][]string{"a": {"x", "y"}}, map[string][]string{"a": {"y", "x"}}, nil)
	test("value changed", map[string][]string{"a": {"x"}}, map[string][]string{"a": {"x", "y"}}, []string{"a"})
	test("new key reported", map[string][]string{}, map[string][]string{"a": {"x"}}, []string{"a"})
	test("disappeared key not reported", map[string][]string{"a": {"x"}, "b": {"y"}}, map[string][]string{"a": {"x"}}, nil)
	test("present-empty vs nil is not a change", map[string][]string{"a": {}}, map[string][]string{"a": nil}, nil)
	test("multiple changed sorted", nil, map[string][]string{"b": {"1"}, "a": {"2"}}, []string{"a", "b"})
}

func TestSameStringSet_IgnoresOrdering(t *testing.T) {
	test := func(name string, a, b []string, want bool) {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, want, sameStringSet(a, b))
		})
	}
	test("nil equals empty", nil, []string{}, true)
	test("same order", []string{"x", "y"}, []string{"x", "y"}, true)
	test("different order", []string{"x", "y"}, []string{"y", "x"}, true)
	test("different length", []string{"x"}, []string{"x", "y"}, false)
	test("different elements", []string{"x"}, []string{"y"}, false)
	test("duplicates matter", []string{"x", "x"}, []string{"x", "y"}, false)
	test("same multiset with dups", []string{"x", "x", "y"}, []string{"y", "x", "x"}, true)
}

// loadedOnlyManifest is a vLLM-like engine: it advertises only what it is
// serving (loaded_models on the OpenAI-shape /v1/models), and lists what it has
// downloaded from a Hugging Face cache, which needs no running server.
func loadedOnlyManifest(t *testing.T, bin string) *Manifest {
	t.Helper()
	cache := t.TempDir()
	writeHFRepo(t, cache, "other/cached-model", llmFiles(map[string]string{"model.safetensors": "w"}))
	m := testEngineManifest(bin)
	m.AdvertisedModels = advertisedModelsLoaded
	m.Actions["list_models"] = Action{
		HFCache: &ActionHFCache{Dir: cache},
		Result:  &ActionResult{Array: "data", Field: "id"},
	}
	m.Actions["loaded_models"] = Action{
		HTTP:   &ActionHTTP{Method: "GET", Path: "/v1/models"},
		Result: &ActionResult{Array: "data", Field: "id", InventoryField: "root"},
	}
	require.NoError(t, m.Validate())
	return m
}

// An engine that serves one model per process can list many models on disk but
// answers for only the one it is running. The proxy routes to any node that
// advertises a model, so advertising the on-disk list would send requests to an
// engine that cannot answer them. advertised_models: "loaded" advertises the
// served model and reports the on-disk list apart, for model managers.
func TestModelsResult_AdvertisesOnlyTheServedModelOfALoadedOnlyEngine(t *testing.T) {
	ex := newTestExecutor(t, loadedOnlyManifest(t, fakeEngineBin))
	ctx := t.Context()
	t.Cleanup(func() { _ = ex.Stop("fake") })
	require.NoError(t, ex.Start(ctx, "fake"))

	res := ex.ModelsResult(ctx)
	served := []string{"llama3.2:1b"} // what the fake engine is running
	assert.Equal(t, map[string][]string{"fake": served}, res.ByEngine, "advertised per engine")
	assert.Equal(t, served, res.Models, "advertised union")
	assert.Equal(t, map[string][]string{"fake": served}, res.LoadedByEngine, "loaded")
	assert.Equal(t, map[string][]string{"fake": {"other/cached-model"}}, res.DownloadedByEngine, "downloaded")
}

// Ejecting a vLLM model stops the engine. Its downloaded models must still be
// reported, so a model manager on this node or a peer keeps the rows it needs to
// Load or Delete them, while nothing is advertised for routing.
func TestModelsResult_ReportsTheDownloadedModelsOfAStoppedLoadedOnlyEngine(t *testing.T) {
	ex := newTestExecutor(t, loadedOnlyManifest(t, fakeEngineBin))
	ctx := t.Context()

	res := ex.ModelsResult(ctx)
	assert.Equal(t, map[string][]string{"fake": {"other/cached-model"}}, res.DownloadedByEngine, "downloaded")
	assert.Empty(t, res.ByEngine, "advertised per engine")
	assert.Empty(t, res.Models, "advertised union")
	assert.Equal(t, map[string][]string{"fake": {}}, res.LoadedByEngine, "nothing loaded, and known to be")
}

// A model the engine cannot read is unknown, not an empty cache: an engine that
// is not installed reports no downloaded key at all.
func TestModelsResult_OmitsTheDownloadedModelsOfAnEngineThatIsNotInstalled(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-installed")
	ex := newTestExecutor(t, loadedOnlyManifest(t, missing))

	res := ex.ModelsResult(t.Context())
	assert.Nil(t, res.DownloadedByEngine)
}

// With vLLM's --served-model-name, requests use the alias, but the model list
// names the cached repository. The alias is advertised for routing and the
// loaded marker uses the repository, so the cached row shows as loaded.
func TestModelsResult_MarksTheCachedModelLoadedWhenServedUnderAnAlias(t *testing.T) {
	m := loadedOnlyManifest(t, fakeEngineBin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	plat := m.Platforms[key]
	plat.Runtime.Env = map[string]string{"OLLAMA_HOST": "127.0.0.1:{port}", "FAKE_SERVED_MODEL_NAME": "qwen-api"}
	m.Platforms[key] = plat
	ex := newTestExecutor(t, m)
	ctx := t.Context()
	t.Cleanup(func() { _ = ex.Stop("fake") })
	require.NoError(t, ex.Start(ctx, "fake"))

	res := ex.ModelsResult(ctx)
	assert.Equal(t, map[string][]string{"fake": {"qwen-api"}}, res.ByEngine, "advertised per engine")
	assert.Equal(t, []string{"qwen-api"}, res.Models, "advertised union")
	assert.Equal(t, map[string][]string{"fake": {"llama3.2:1b"}}, res.LoadedByEngine, "loaded")
}

// An engine whose advertised list is its inventory reports nothing apart.
func TestModelsResult_OmitsDownloadedForAnEngineThatAdvertisesItsList(t *testing.T) {
	ex := newTestExecutor(t, loadedActionManifest())
	ctx := t.Context()
	t.Cleanup(func() { _ = ex.Stop("fake") })
	require.NoError(t, ex.Start(ctx, "fake"))

	assert.Nil(t, ex.ModelsResult(ctx).DownloadedByEngine)
}

func TestInventoryField_IsOnlyForTheLoadedModelsOfALoadedOnlyEngine(t *testing.T) {
	manifestWith := func(advertised, action, field string) *Manifest {
		m := loadedActionManifest()
		m.AdvertisedModels = advertised
		act := m.Actions[action]
		result := *act.Result
		result.InventoryField = field
		act.Result = &result
		m.Actions[action] = act
		return m
	}
	accept := func(name, advertised, action, field string) {
		t.Run(name, func(t *testing.T) {
			assert.NoError(t, manifestWith(advertised, action, field).Validate())
		})
	}
	reject := func(name, advertised, action, field string) {
		t.Run(name, func(t *testing.T) {
			assert.Error(t, manifestWith(advertised, action, field).Validate())
		})
	}
	accept("loaded_models of a loaded-only engine", advertisedModelsLoaded, "loaded_models", "root")
	reject("engine that advertises its list", advertisedModelsList, "loaded_models", "root")
	reject("action other than loaded_models", advertisedModelsLoaded, "list_models", "root")
	reject("object path instead of a field name", advertisedModelsLoaded, "loaded_models", "a.b")
}

func TestAdvertisedModels_AcceptsOnlyKnownValuesItCanServe(t *testing.T) {
	accept := func(name, value string) {
		t.Run(name, func(t *testing.T) {
			m := loadedActionManifest()
			m.AdvertisedModels = value

			assert.NoError(t, m.Validate())
		})
	}
	reject := func(name string, m *Manifest, value string) {
		t.Run(name, func(t *testing.T) {
			m.AdvertisedModels = value

			assert.Error(t, m.Validate())
		})
	}
	accept("unset", "")
	accept("the list", advertisedModelsList)
	accept("loaded, with a loaded_models action", advertisedModelsLoaded)
	reject("loaded without a loaded_models action", testEngineManifest(fakeEngineBin), advertisedModelsLoaded)
	reject("an unknown value", testEngineManifest(fakeEngineBin), "everything")
}

// Canceling inventory should release its caller promptly. A later status read
// must still be able to identify the listener after that cancellation.
func TestModelsResult_CanceledCallerReturnsBeforeProbeCompletes(t *testing.T) {
	test := func(name, advertised string) {
		t.Run(name, func(t *testing.T) {
			fixture := newBlockedReadinessFixture(t)
			ex := fixture.executor
			enableModelInventory(t, ex)
			m, ok := ex.reg.Get("fake")
			require.True(t, ok)
			m.AdvertisedModels = advertised
			m.Actions["loaded_models"] = m.Actions["list_models"]
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Receive cancellation while the fixture still withholds the engine reply.
			finished := make(chan error, 1)
			go func() {
				_, err := ex.modelsResult(ctx)
				finished <- err
			}()
			awaitProbeStart(t, fixture.probeStarted)
			cancel()
			require.ErrorIs(t, awaitResult(t, finished), context.Canceled)
			// The last caller leaving must stop the HTTP request before its reply is
			// released, rather than merely letting the caller stop waiting.
			awaitResult(t, fixture.probeCanceled)
			fixture.releaseProbe()
			status, err := ex.Status("fake")
			require.NoError(t, err)
			require.True(t, status.Running)
		})
	}
	test("engine advertises its inventory", advertisedModelsList)
	test("engine advertises its loaded models", advertisedModelsLoaded)
}

// enableModelInventory adds the result specification needed for modelsResult
// to include this engine in an inventory sweep.
func enableModelInventory(t *testing.T, ex *Executor) {
	t.Helper()
	m, ok := ex.reg.Get("fake")
	require.True(t, ok)
	m.Actions["list_models"] = Action{
		HTTP:   &ActionHTTP{Method: http.MethodGet, Path: "/api/tags"},
		Result: &ActionResult{Array: "models", Field: "name"},
	}
}
