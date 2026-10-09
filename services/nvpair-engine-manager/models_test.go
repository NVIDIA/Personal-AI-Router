// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExtractStrings(t *testing.T) {
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

func TestExtractStringsResultDistinguishesEmptyFromUnknown(t *testing.T) {
	spec := &ActionResult{Array: "models", Field: "key"}
	got, ok := extractStringsResult(json.RawMessage(`{"models":[]}`), spec)
	require.True(t, ok, "explicit empty inventory (%v, %v)", got, ok)
	assert.Empty(t, got, "explicit empty inventory")
	for _, raw := range []string{
		`{}`,
		`{"models":null}`,
		`{"models":{}}`,
		`{"models":[{}]}`,
		`{"models":[null]}`,
		`not-json`,
	} {
		got, ok := extractStringsResult(json.RawMessage(raw), spec)
		assert.False(t, ok, "invalid inventory (%v, %v, %v)", raw, got, ok)
		assert.Empty(t, got, "invalid inventory (%v)", raw)
	}
}

// TestModels drives Models() against the running fake engine: a running engine
// with a Result-bearing list_models action contributes its models; a stopped
// engine contributes nothing.
func TestModels(t *testing.T) {
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
func TestModelsResult(t *testing.T) {
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
	body, _ := json.Marshal(map[string][]string{"names": names})
	url := fmt.Sprintf("http://127.0.0.1:%d/testctl/loaded", st.Port)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	require.NoError(t, err, "set loaded")
	_ = resp.Body.Close()
}

// TestModelsResultLoaded covers the loaded surface: a running engine that
// declares loaded_models reports its resident subset in LoadedByEngine, and an
// eviction leaves the engine key present with an empty list ("running, nothing
// loaded") while the installed list is unchanged.
func TestModelsResultLoaded(t *testing.T) {
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
func TestModelsResultNoLoadedActionOmitsKey(t *testing.T) {
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

// TestSweepLoadedSeedsThenEmitsOnChange covers the watcher's core diff without
// goroutines or timers: the first sweep only seeds a baseline (the caller,
// watchLoaded, suppresses its emit), a subsequent identical sweep reports no
// change, and a sweep after an eviction reports the engine as changed and
// carries the fresh full model result as the push payload.
func TestSweepLoadedSeedsThenEmitsOnChange(t *testing.T) {
	ex := newTestExecutor(t, loadedActionManifest())
	ctx := context.Background()
	t.Cleanup(func() { _ = ex.Stop("fake") })
	require.NoError(t, ex.Start(ctx, "fake"), "start")

	// Seed sweep: baseline is {fake:[llama3.2:1b]}.
	_, prev, _ := ex.sweepLoaded(ctx, nil)
	require.Equal(t, map[string][]string{"fake": {"llama3.2:1b"}}, prev, "seed baseline")

	// No residency change -> no engine reported changed.
	changed, prev, _ := ex.sweepLoaded(ctx, prev)
	require.Empty(t, changed, "unchanged sweep reported")

	// Evict everything -> fake changes; payload carries the empty loaded set.
	setLoaded(t, ex, nil)
	changed, _, res := ex.sweepLoaded(ctx, prev)
	require.Equal(t, []string{"fake"}, changed, "changed")
	require.Equal(t, map[string][]string{"fake": {}}, res.LoadedByEngine, "pushed LoadedByEngine")
}

// TestSweepLoadedRetainsLastGoodOnTransientMiss covers the anti-churn guard: an
// engine that drops out of a sweep (a transient loaded_models miss, indistinct
// from a stop) is NOT reported as changed and keeps its last-good baseline, so a
// single blip can't emit a spurious drop-then-re-add pair.
func TestSweepLoadedRetainsLastGoodOnTransientMiss(t *testing.T) {
	ex := newTestExecutor(t, loadedActionManifest())
	prev := map[string][]string{"fake": {"llama3.2:1b"}}
	// The engine isn't started, so ModelsResult reports it neither running nor
	// queryable: LoadedByEngine has no "fake" key this sweep.
	changed, next, _ := ex.sweepLoaded(context.Background(), prev)
	require.Empty(t, changed, "a disappeared engine reported")
	require.Equal(t, map[string][]string{"fake": {"llama3.2:1b"}}, next, "baseline after miss")
}

func TestChangedEngines(t *testing.T) {
	tests := []struct {
		name      string
		prev, cur map[string][]string
		want      []string
	}{
		{name: "no change", prev: map[string][]string{"a": {"x"}}, cur: map[string][]string{"a": {"x"}}, want: nil},
		{name: "reorder is not a change", prev: map[string][]string{"a": {"x", "y"}}, cur: map[string][]string{"a": {"y", "x"}}, want: nil},
		{name: "value changed", prev: map[string][]string{"a": {"x"}}, cur: map[string][]string{"a": {"x", "y"}}, want: []string{"a"}},
		{name: "new key reported", prev: map[string][]string{}, cur: map[string][]string{"a": {"x"}}, want: []string{"a"}},
		{name: "disappeared key not reported", prev: map[string][]string{"a": {"x"}, "b": {"y"}}, cur: map[string][]string{"a": {"x"}}, want: nil},
		{name: "present-empty vs nil is not a change", prev: map[string][]string{"a": {}}, cur: map[string][]string{"a": nil}, want: nil},
		{name: "multiple changed sorted", prev: nil, cur: map[string][]string{"b": {"1"}, "a": {"2"}}, want: []string{"a", "b"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, changedEngines(tc.prev, tc.cur), "changedEngines")
		})
	}
}

func TestSameStringSet(t *testing.T) {
	tests := []struct {
		name string
		a, b []string
		want bool
	}{
		{name: "nil equals empty", a: nil, b: []string{}, want: true},
		{name: "same order", a: []string{"x", "y"}, b: []string{"x", "y"}, want: true},
		{name: "different order", a: []string{"x", "y"}, b: []string{"y", "x"}, want: true},
		{name: "different length", a: []string{"x"}, b: []string{"x", "y"}, want: false},
		{name: "different elements", a: []string{"x"}, b: []string{"y"}, want: false},
		{name: "duplicates matter", a: []string{"x", "x"}, b: []string{"x", "y"}, want: false},
		{name: "same multiset with dups", a: []string{"x", "x", "y"}, b: []string{"y", "x", "x"}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, sameStringSet(tc.a, tc.b), "sameStringSet")
		})
	}
}
