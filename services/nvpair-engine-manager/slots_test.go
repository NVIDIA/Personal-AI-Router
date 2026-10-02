// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// slotCountVar is an environment variable no developer or CI host sets, so a
// test controls whether it is present.
const slotCountVar = "PAIR_TEST_SLOT_COUNT"

// lmStudioLoadedSlots is the bundled LM Studio slots.loaded block. Each call
// returns a fresh copy a test may mutate.
func lmStudioLoadedSlots() *LoadedSlots {
	return &LoadedSlots{
		Array:      "models",
		Key:        "key",
		Instances:  "loaded_instances",
		InstanceID: "id",
		Parallel:   []string{"config", "parallel"},
	}
}

// loadedModelsAction reads LM Studio's native model list.
func loadedModelsAction() Action {
	return Action{HTTP: &ActionHTTP{Method: http.MethodGet, Path: "/api/v1/models"}}
}

func requireSlots(t *testing.T, got, want *EngineSlots) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("slots = %+v, want %+v", got, want)
	}
}

func TestValidateAcceptsSlots(t *testing.T) {
	accept := func(name string, slots Slots) {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			m.Actions["loaded_models"] = loadedModelsAction()
			m.Slots = &slots
			if err := m.Validate(); err != nil {
				t.Fatalf("Validate() = %v, want nil", err)
			}
		})
	}
	accept("default only", Slots{Default: 1})
	accept("environment variable", Slots{Default: 1, Env: "OLLAMA_NUM_PARALLEL"})
	accept("loaded counts", Slots{Default: 1, Loaded: lmStudioLoadedSlots()})
}

func TestValidateRejectsSlots(t *testing.T) {
	reject := func(name string, mutate func(*Manifest), want string) {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			m.Actions["loaded_models"] = loadedModelsAction()
			m.Slots = &Slots{Default: 1, Loaded: lmStudioLoadedSlots()}
			mutate(&m)
			err := m.Validate()
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, want)
			}
		})
	}
	const needsDefault = "slots.default must be at least 1"
	const needsAction = "slots.loaded requires an http loaded_models action"
	const needsPaths = "slots.loaded requires array, key, instances, instance_id and a parallel path"
	reject("zero default", func(m *Manifest) { m.Slots.Default = 0 }, needsDefault)
	reject("negative default", func(m *Manifest) { m.Slots.Default = -1 }, needsDefault)
	reject("env with a dash", func(m *Manifest) { m.Slots.Env = "NUM-PARALLEL" }, "slots.env")
	reject("env starting with a digit", func(m *Manifest) { m.Slots.Env = "1PARALLEL" }, "slots.env")
	reject("loaded without loaded_models", func(m *Manifest) { delete(m.Actions, "loaded_models") }, needsAction)
	reject("loaded from a cmd action", func(m *Manifest) {
		m.Actions["loaded_models"] = Action{Cmd: []string{"lms", "ps"}}
	}, needsAction)
	reject("loaded without an array", func(m *Manifest) { m.Slots.Loaded.Array = "" }, needsPaths)
	reject("loaded without a key", func(m *Manifest) { m.Slots.Loaded.Key = "" }, needsPaths)
	reject("loaded without instances", func(m *Manifest) { m.Slots.Loaded.Instances = "" }, needsPaths)
	reject("loaded with a blank instance id", func(m *Manifest) { m.Slots.Loaded.InstanceID = " " }, needsPaths)
	reject("loaded without a parallel path", func(m *Manifest) { m.Slots.Loaded.Parallel = nil }, needsPaths)
	reject("loaded with an empty path segment", func(m *Manifest) {
		m.Slots.Loaded.Parallel = []string{"config", ""}
	}, needsPaths)
}

func TestBundledManifestSlots(t *testing.T) {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatalf("load bundled manifests: %v", err)
	}
	test := func(engine string, want Slots) {
		t.Run(engine, func(t *testing.T) {
			m, ok := reg.Get(engine)
			if !ok {
				t.Fatalf("no bundled %s manifest", engine)
			}
			if m.Slots == nil || !reflect.DeepEqual(*m.Slots, want) {
				t.Fatalf("%s slots = %+v, want %+v", engine, m.Slots, want)
			}
		})
	}
	test("ollama", Slots{Default: 1, Env: "OLLAMA_NUM_PARALLEL"})
	test("lmstudio", Slots{Default: 1, Loaded: lmStudioLoadedSlots()})
}

func TestOverrideManifestKeepsBundledSlots(t *testing.T) {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatalf("load bundled manifests: %v", err)
	}
	dir := t.TempDir()
	override := []byte(`{"engine":"ollama","display_name":"Ollama (custom)"}`)
	if err := os.WriteFile(filepath.Join(dir, "ollama.json"), override, 0o600); err != nil {
		t.Fatalf("write override: %v", err)
	}
	if err := reg.LoadOverrideDir(dir); err != nil {
		t.Fatalf("load override: %v", err)
	}
	m, ok := reg.Get("ollama")
	if !ok || m.DisplayName != "Ollama (custom)" {
		t.Fatalf("override was not applied: %+v", m)
	}
	if m.Slots == nil || *m.Slots != (Slots{Default: 1, Env: "OLLAMA_NUM_PARALLEL"}) {
		t.Fatalf("override lost the bundled slots: %+v", m.Slots)
	}
}

func TestLaunchSlots(t *testing.T) {
	spec := &Slots{Default: 1, Env: slotCountVar}
	test := func(name string, launch map[string]string, inherited string, want int) {
		t.Run(name, func(t *testing.T) {
			if inherited != "" {
				t.Setenv(slotCountVar, inherited)
			}
			if got := launchSlots(spec, launch); got != want {
				t.Fatalf("launchSlots() = %d, want %d", got, want)
			}
		})
	}
	test("launch environment", map[string]string{slotCountVar: "3"}, "", 3)
	test("launch environment wins over the inherited one", map[string]string{slotCountVar: "3"}, "5", 3)
	test("inherited environment", nil, "5", 5)
	test("neither sets it", nil, "", 0)
	test("an invalid launch value hides the inherited one", map[string]string{slotCountVar: "many"}, "5", 0)
	test("an empty launch value hides the inherited one", map[string]string{slotCountVar: ""}, "5", 0)
	test("zero", map[string]string{slotCountVar: "0"}, "", 0)
	test("negative", map[string]string{slotCountVar: "-2"}, "", 0)
	test("an invalid inherited value", nil, "lots", 0)
	test("surrounding spaces and quotes", map[string]string{slotCountVar: ` "4" `}, "", 4)

	t.Run("no slots block", func(t *testing.T) {
		if got := launchSlots(nil, map[string]string{slotCountVar: "3"}); got != 0 {
			t.Fatalf("launchSlots(nil) = %d, want 0", got)
		}
	})
	t.Run("no environment variable declared", func(t *testing.T) {
		if got := launchSlots(&Slots{Default: 1}, map[string]string{slotCountVar: "3"}); got != 0 {
			t.Fatalf("launchSlots() = %d, want 0", got)
		}
	})
}

func TestLaunchSlotsMatchesNamesLikeTheHost(t *testing.T) {
	got := launchSlots(&Slots{Default: 1, Env: slotCountVar}, map[string]string{strings.ToLower(slotCountVar): "3"})
	want := 0
	if runtime.GOOS == "windows" {
		want = 3
	}
	if got != want {
		t.Fatalf("launchSlots() with a lower-case name on %s = %d, want %d", runtime.GOOS, got, want)
	}
}

func TestLoadedSlots(t *testing.T) {
	test := func(name, raw string, want map[string]int) {
		t.Run(name, func(t *testing.T) {
			got, ok := loadedSlots(json.RawMessage(raw), lmStudioLoadedSlots())
			if !ok {
				t.Fatalf("loadedSlots() rejected %s", raw)
			}
			if !maps.Equal(got, want) {
				t.Fatalf("loadedSlots() = %v, want %v", got, want)
			}
		})
	}
	test("one instance",
		`{"models":[{"key":"qwen/qwen3-8b","loaded_instances":[{"id":"qwen/qwen3-8b","config":{"parallel":4}}]}]}`,
		map[string]int{"qwen/qwen3-8b": 4})
	test("the key follows the instance that shares its id",
		`{"models":[{"key":"m","loaded_instances":[{"id":"m:2","config":{"parallel":2}},{"id":"m","config":{"parallel":6}}]}]}`,
		map[string]int{"m": 6, "m:2": 2})
	test("a key no instance shares takes the smallest count",
		`{"models":[{"key":"m","loaded_instances":[{"id":"m:a","config":{"parallel":4}},{"id":"m:b","config":{"parallel":2}}]}]}`,
		map[string]int{"m:a": 4, "m:b": 2, "m": 2})
	test("several models",
		`{"models":[{"key":"a","loaded_instances":[{"id":"a","config":{"parallel":1}}]},{"key":"b","loaded_instances":[{"id":"b","config":{"parallel":3}}]}]}`,
		map[string]int{"a": 1, "b": 3})
	test("a missing count is ignored",
		`{"models":[{"key":"embed","loaded_instances":[{"id":"embed","config":{"context_length":2048}}]}]}`,
		map[string]int{})
	test("counts below 1 are ignored",
		`{"models":[{"key":"m","loaded_instances":[{"id":"m","config":{"parallel":0}},{"id":"m:2","config":{"parallel":-1}}]}]}`,
		map[string]int{})
	test("counts of the wrong type are ignored",
		`{"models":[{"key":"m","loaded_instances":[{"id":"a","config":{"parallel":"4"}},{"id":"b","config":{"parallel":2.5}},{"id":"c","config":{"parallel":null}},{"id":"d","config":4}]}]}`,
		map[string]int{})
	test("models with no loaded instances add nothing",
		`{"models":[{"key":"idle","loaded_instances":[]},{"key":"bare"}]}`,
		map[string]int{})
	test("an empty model list",
		`{"models":[]}`,
		map[string]int{})
	test("a malformed row is skipped",
		`{"models":[{"key":5,"loaded_instances":"x"},{"key":"m","loaded_instances":[{"id":"m","config":{"parallel":3}}]}]}`,
		map[string]int{"m": 3})
}

func TestLoadedSlotsRejectsAResponseWithoutTheArray(t *testing.T) {
	reject := func(name, raw string) {
		t.Run(name, func(t *testing.T) {
			if got, ok := loadedSlots(json.RawMessage(raw), lmStudioLoadedSlots()); ok {
				t.Fatalf("loadedSlots(%s) = %v, want a rejection", raw, got)
			}
		})
	}
	reject("not JSON", `{`)
	reject("missing array", `{}`)
	reject("null array", `{"models":null}`)
	reject("array of the wrong type", `{"models":{}}`)
}

// slotEngine returns the fake engine's manifest declaring slots, with extra
// launch environment.
func slotEngine(slots Slots, env map[string]string) *Manifest {
	m := testEngineManifest(fakeEngineBin)
	m.Slots = &slots
	p := m.Platforms[hostKey()]
	maps.Copy(p.Runtime.Env, env)
	m.Platforms[hostKey()] = p
	return m
}

func TestManagedLaunchReportsSlotsWhileRunning(t *testing.T) {
	m := slotEngine(Slots{Default: 1, Env: slotCountVar}, map[string]string{slotCountVar: "3"})
	ex := newTestExecutor(t, m)
	t.Cleanup(func() { _ = ex.Stop("fake") })

	before, err := ex.Status("fake")
	if err != nil {
		t.Fatalf("status before start: %v", err)
	}
	requireSlots(t, before.Slots, nil)

	if err := ex.Start(context.Background(), "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}
	running, err := ex.Status("fake")
	if err != nil {
		t.Fatalf("status while running: %v", err)
	}
	requireSlots(t, running.Slots, &EngineSlots{Default: 3})

	if err := ex.Stop("fake"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	stopped, err := ex.Status("fake")
	if err != nil {
		t.Fatalf("status after stop: %v", err)
	}
	requireSlots(t, stopped.Slots, nil)
}

func TestEngineWithoutSlotsReportsNone(t *testing.T) {
	ex := newTestExecutor(t, testEngineManifest(fakeEngineBin))
	t.Cleanup(func() { _ = ex.Stop("fake") })
	if err := ex.Start(context.Background(), "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}
	st, err := ex.Status("fake")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Running {
		t.Fatalf("engine is not running: %+v", st)
	}
	requireSlots(t, st.Slots, nil)
}

// An engine NVPAIR adopts runs with an environment NVPAIR cannot see, so its
// count is the manifest default even when NVPAIR's own environment sets the
// variable.
func TestAdoptedEngineReportsTheManifestDefault(t *testing.T) {
	t.Setenv(slotCountVar, "7")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ex := newTestExecutor(t, adoptableManifest(t, srv, Slots{Default: 2, Env: slotCountVar}))
	if err := ex.Start(context.Background(), "adopt"); err != nil {
		t.Fatalf("start should adopt the running service: %v", err)
	}
	st, err := ex.Status("adopt")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.Running {
		t.Fatalf("service was not adopted: %+v", st)
	}
	requireSlots(t, st.Slots, &EngineSlots{Default: 2})
}

// adoptableManifest describes an engine whose binary does not exist, so the
// only way it can run is by adopting the service already on srv's port.
func adoptableManifest(t *testing.T, srv *httptest.Server, slots Slots) *Manifest {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse server port: %v", err)
	}
	return &Manifest{
		Engine: "adopt", DisplayName: "Adopt", ManifestVersion: 1,
		Platforms: map[string]Platform{
			hostKey(): {
				Detect: []string{filepath.Join(t.TempDir(), "missing"+exeExt())},
				Runtime: Runtime{
					Bin:   filepath.Join(t.TempDir(), "does-not-exist"+exeExt()),
					Port:  port,
					Ready: &Probe{HTTP: "http://127.0.0.1:{port}/", Status: 200, TimeoutS: 5},
				},
			},
		},
		Actions: map[string]Action{"loaded_models": loadedModelsAction()},
		Slots:   &slots,
	}
}

// A managed run's launch count must not describe a service NVPAIR adopts on
// the same port after that run ends.
func TestEndedRunsSlotsDoNotCarryOver(t *testing.T) {
	test := func(name string, end func(t *testing.T, ex *Executor, port int)) {
		t.Run(name, func(t *testing.T) {
			port, err := freePort()
			if err != nil {
				t.Fatalf("free port: %v", err)
			}
			m := slotEngine(Slots{Default: 1, Env: slotCountVar}, map[string]string{slotCountVar: "3"})
			p := m.Platforms[hostKey()]
			p.Runtime.Port = port
			m.Platforms[hostKey()] = p
			ex := newTestExecutor(t, m)
			if err := ex.Start(context.Background(), "fake"); err != nil {
				t.Fatalf("start: %v", err)
			}
			end(t, ex, port)

			ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err != nil {
				t.Fatalf("external listener: %v", err)
			}
			external := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})}
			go func() { _ = external.Serve(ln) }()
			t.Cleanup(func() { _ = external.Close() })

			if err := ex.Start(context.Background(), "fake"); err != nil {
				t.Fatalf("start should adopt the external service: %v", err)
			}
			st, err := ex.Status("fake")
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			if !st.Running {
				t.Fatalf("external service was not adopted: %+v", st)
			}
			requireSlots(t, st.Slots, &EngineSlots{Default: 1})
		})
	}
	test("after a stop", func(t *testing.T, ex *Executor, _ int) {
		if err := ex.Stop("fake"); err != nil {
			t.Fatalf("stop: %v", err)
		}
	})
	test("after a crash", func(t *testing.T, ex *Executor, port int) {
		resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(port) + "/exit")
		if err == nil {
			_ = resp.Body.Close()
		}
		state, err := ex.state("fake")
		if err != nil {
			t.Fatalf("engine state: %v", err)
		}
		waitFor(t, 5*time.Second, func() bool {
			state.mu.Lock()
			defer state.mu.Unlock()
			return !state.running
		})
	})
}

// A service that went away and came back must not inherit the counts the
// previous one reported.
func TestUnadoptionClearsLoadedSlots(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	ex := newTestExecutor(t, adoptableManifest(t, srv, Slots{Default: 1, Loaded: lmStudioLoadedSlots()}))
	if err := ex.Start(context.Background(), "adopt"); err != nil {
		t.Fatalf("start should adopt the running service: %v", err)
	}
	ex.recordLoadedSlots("adopt", ex.runGeneration("adopt"),
		json.RawMessage(`{"models":[{"key":"m","loaded_instances":[{"id":"m","config":{"parallel":4}}]}]}`))
	adopted, err := ex.Status("adopt")
	if err != nil {
		t.Fatalf("status while adopted: %v", err)
	}
	requireSlots(t, adopted.Slots, &EngineSlots{Default: 1, Models: map[string]int{"m": 4}})

	srv.Close()
	gone, err := ex.Status("adopt")
	if err != nil {
		t.Fatalf("status after the service stopped: %v", err)
	}
	if gone.Running {
		t.Fatalf("service that stopped is still running: %+v", gone)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("relisten on %s: %v", addr, err)
	}
	returned := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = returned.Serve(ln) }()
	t.Cleanup(func() { _ = returned.Close() })
	back, err := ex.Status("adopt")
	if err != nil {
		t.Fatalf("status after the service returned: %v", err)
	}
	if !back.Running {
		t.Fatalf("returned service was not adopted: %+v", back)
	}
	requireSlots(t, back.Slots, &EngineSlots{Default: 1})
}

func TestRecordLoadedSlots(t *testing.T) {
	const counts = `{"models":[{"key":"m","loaded_instances":[{"id":"m","config":{"parallel":4}}]}]}`
	const recounted = `{"models":[{"key":"m","loaded_instances":[{"id":"m","config":{"parallel":8}}]}]}`
	// running returns an executor whose engine runs as generation 2 and has
	// already recorded counts.
	running := func(t *testing.T) (*Executor, *engineState) {
		t.Helper()
		m := testEngineManifest(fakeEngineBin)
		m.Actions["loaded_models"] = loadedModelsAction()
		m.Slots = &Slots{Default: 1, Loaded: lmStudioLoadedSlots()}
		ex := newTestExecutor(t, m)
		st, err := ex.state("fake")
		if err != nil {
			t.Fatalf("engine state: %v", err)
		}
		st.mu.Lock()
		st.running = true
		st.gen = 2
		st.mu.Unlock()
		ex.recordLoadedSlots("fake", 2, json.RawMessage(counts))
		return ex, st
	}
	recorded := func(st *engineState) map[string]int {
		st.mu.Lock()
		defer st.mu.Unlock()
		return maps.Clone(st.slotModels)
	}
	test := func(name string, act func(*Executor, *engineState), want map[string]int) {
		t.Run(name, func(t *testing.T) {
			ex, st := running(t)
			act(ex, st)
			if got := recorded(st); !maps.Equal(got, want) {
				t.Fatalf("recorded counts = %v, want %v", got, want)
			}
		})
	}
	test("a new response replaces the counts", func(ex *Executor, _ *engineState) {
		ex.recordLoadedSlots("fake", 2, json.RawMessage(recounted))
	}, map[string]int{"m": 8})
	test("a malformed response keeps the last good counts", func(ex *Executor, _ *engineState) {
		ex.recordLoadedSlots("fake", 2, json.RawMessage(`{`))
	}, map[string]int{"m": 4})
	test("a response from an earlier run is dropped", func(ex *Executor, _ *engineState) {
		ex.recordLoadedSlots("fake", 1, json.RawMessage(recounted))
	}, map[string]int{"m": 4})
	test("a response after the engine stopped is dropped", func(ex *Executor, st *engineState) {
		st.mu.Lock()
		st.running = false
		st.mu.Unlock()
		ex.recordLoadedSlots("fake", 2, json.RawMessage(recounted))
	}, map[string]int{"m": 4})
}

// lmStudioShapedEngine is the fake engine with LM Studio's model list and
// slots block, so ModelsResult reads loaded instances and their counts.
func lmStudioShapedEngine(loadedResult *ActionResult) *Manifest {
	m := slotEngine(Slots{Default: 1, Loaded: lmStudioLoadedSlots()}, nil)
	action := loadedModelsAction()
	action.Result = loadedResult
	m.Actions["loaded_models"] = action
	return m
}

func TestModelsResultRecordsLoadedSlots(t *testing.T) {
	loadedList := &ActionResult{Array: "models", Field: "key", Match: &ResultMatch{Field: "loaded_instances", Nonempty: true}}
	ex := newTestExecutor(t, lmStudioShapedEngine(loadedList))
	t.Cleanup(func() { _ = ex.Stop("fake") })
	if err := ex.Start(context.Background(), "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}
	res := ex.ModelsResult(context.Background())
	if got := res.LoadedByEngine["fake"]; !slices.Equal(got, []string{"llama3.2:1b"}) {
		t.Fatalf("loaded models = %v, want [llama3.2:1b]", got)
	}
	st, err := ex.Status("fake")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	requireSlots(t, st.Slots, &EngineSlots{Default: 1, Models: map[string]int{"llama3.2:1b": 4}})
}

func TestModelsResultReadsSlotsWithoutALoadedList(t *testing.T) {
	ex := newTestExecutor(t, lmStudioShapedEngine(nil))
	t.Cleanup(func() { _ = ex.Stop("fake") })
	if err := ex.Start(context.Background(), "fake"); err != nil {
		t.Fatalf("start: %v", err)
	}
	res := ex.ModelsResult(context.Background())
	if _, ok := res.LoadedByEngine["fake"]; ok {
		t.Fatalf("an engine with no loaded_models result reported a loaded list: %v", res.LoadedByEngine)
	}
	st, err := ex.Status("fake")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	requireSlots(t, st.Slots, &EngineSlots{Default: 1, Models: map[string]int{"llama3.2:1b": 4}})
}
