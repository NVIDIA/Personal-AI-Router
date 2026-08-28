// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
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
			got := extractStrings(json.RawMessage(tc.raw), tc.spec)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("extractStrings = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExtractStringsResultDistinguishesEmptyFromUnknown(t *testing.T) {
	spec := &ActionResult{Array: "models", Field: "key"}
	if got, ok := extractStringsResult(json.RawMessage(`{"models":[]}`), spec); !ok || got == nil || len(got) != 0 {
		t.Fatalf("explicit empty inventory = (%v, %v), want non-nil empty, true", got, ok)
	}
	for _, raw := range []string{
		`{}`,
		`{"models":null}`,
		`{"models":{}}`,
		`{"models":[{}]}`,
		`{"models":[null]}`,
		`not-json`,
	} {
		if got, ok := extractStringsResult(json.RawMessage(raw), spec); ok || got != nil {
			t.Errorf("invalid inventory %q = (%v, %v), want nil, false", raw, got, ok)
		}
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
	if got := ex.Models(ctx); len(got) != 0 {
		t.Fatalf("Models() on stopped engine = %v, want empty", got)
	}

	if err := ex.Start(ctx, "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}
	got := ex.Models(ctx)
	want := []string{"llama3.2:1b"} // fake engine's seeded model
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Models() on running engine = %v, want %v", got, want)
	}

	if err := ex.Stop("fake"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := ex.Models(ctx); len(got) != 0 {
		t.Fatalf("Models() after stop = %v, want empty", got)
	}
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
	if len(res.Models) != 0 {
		t.Fatalf("ModelsResult().Models on stopped engine = %v, want empty", res.Models)
	}
	if len(res.ByEngine) != 0 {
		t.Fatalf("ModelsResult().ByEngine on stopped engine = %v, want empty", res.ByEngine)
	}

	if err := ex.Start(ctx, "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}
	res = ex.ModelsResult(ctx)
	wantModels := []string{"llama3.2:1b"} // fake engine's seeded model
	if !reflect.DeepEqual(res.Models, wantModels) {
		t.Fatalf("ModelsResult().Models = %v, want %v", res.Models, wantModels)
	}
	wantByEngine := map[string][]string{"fake": {"llama3.2:1b"}}
	if !reflect.DeepEqual(res.ByEngine, wantByEngine) {
		t.Fatalf("ModelsResult().ByEngine = %v, want %v", res.ByEngine, wantByEngine)
	}

	// Deleting the last model is a successful empty inventory, not an unknown
	// response: preserve the engine key so consumers can clear stale state.
	if _, err := ex.Action(ctx, "fake", "delete_model", json.RawMessage(`{"name":"llama3.2:1b"}`)); err != nil {
		t.Fatalf("delete last model: %v", err)
	}
	res = ex.ModelsResult(ctx)
	if len(res.Models) != 0 {
		t.Fatalf("ModelsResult().Models after last delete = %v, want empty", res.Models)
	}
	if want := map[string][]string{"fake": {}}; !reflect.DeepEqual(res.ByEngine, want) {
		t.Fatalf("ModelsResult().ByEngine after last delete = %v, want %v", res.ByEngine, want)
	}
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
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	body, _ := json.Marshal(map[string][]string{"names": names})
	url := fmt.Sprintf("http://127.0.0.1:%d/testctl/loaded", st.Port)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("set loaded: %v", err)
	}
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

	if err := ex.Start(ctx, "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}
	res := ex.ModelsResult(ctx)
	if want := map[string][]string{"fake": {"llama3.2:1b"}}; !reflect.DeepEqual(res.LoadedByEngine, want) {
		t.Fatalf("LoadedByEngine = %v, want %v", res.LoadedByEngine, want)
	}

	// Evict everything: the key stays with an empty list.
	setLoaded(t, ex, nil)
	res = ex.ModelsResult(ctx)
	if want := map[string][]string{"fake": {}}; !reflect.DeepEqual(res.LoadedByEngine, want) {
		t.Fatalf("LoadedByEngine after evict = %v, want %v", res.LoadedByEngine, want)
	}
	if want := []string{"llama3.2:1b"}; !reflect.DeepEqual(res.Models, want) {
		t.Fatalf("Models after evict = %v, want %v (residency must not change the installed list)", res.Models, want)
	}
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
	if err := ex.Start(ctx, "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if res := ex.ModelsResult(ctx); res.LoadedByEngine != nil {
		t.Fatalf("LoadedByEngine = %v, want nil (no loaded_models action)", res.LoadedByEngine)
	}
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
	if err := ex.Start(ctx, "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Seed sweep: baseline is {fake:[llama3.2:1b]}.
	_, prev, _ := ex.sweepLoaded(ctx, nil)
	if want := map[string][]string{"fake": {"llama3.2:1b"}}; !reflect.DeepEqual(prev, want) {
		t.Fatalf("seed baseline = %v, want %v", prev, want)
	}

	// No residency change -> no engine reported changed.
	changed, prev, _ := ex.sweepLoaded(ctx, prev)
	if len(changed) != 0 {
		t.Fatalf("unchanged sweep reported %v, want none", changed)
	}

	// Evict everything -> fake changes; payload carries the empty loaded set.
	setLoaded(t, ex, nil)
	changed, _, res := ex.sweepLoaded(ctx, prev)
	if !reflect.DeepEqual(changed, []string{"fake"}) {
		t.Fatalf("changed = %v, want [fake]", changed)
	}
	if want := map[string][]string{"fake": {}}; !reflect.DeepEqual(res.LoadedByEngine, want) {
		t.Fatalf("pushed LoadedByEngine = %v, want %v", res.LoadedByEngine, want)
	}
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
	if len(changed) != 0 {
		t.Fatalf("a disappeared engine reported %v changed, want none", changed)
	}
	if want := map[string][]string{"fake": {"llama3.2:1b"}}; !reflect.DeepEqual(next, want) {
		t.Fatalf("baseline after miss = %v, want last-good %v", next, want)
	}
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
			if got := changedEngines(tc.prev, tc.cur); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("changedEngines = %v, want %v", got, tc.want)
			}
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
			if got := sameStringSet(tc.a, tc.b); got != tc.want {
				t.Fatalf("sameStringSet(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
