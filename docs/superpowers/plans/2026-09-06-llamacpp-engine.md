# llama.cpp Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a first-class `llamacpp` engine to PAIR that adopts an already-running `llama-server`, exposes OpenAI `/v1` on PAIR port 8084, lists the full catalog, and routes only to nodes that already have the requested model loaded.

**Architecture:** Clone `lmstudio-proxy` into `llamacpp-proxy` (discovery key `lc`, facade 8084). Engine-manager gains `runtime.mode: "adopt"` and a `llamacpp.json` manifest (default probe 8082). Routing uses `LoadedByEngine`, never catalog ids, so PAIR cannot trigger a GGUF swap. This node can still forward to paired Mac/laptop PAIR nodes while ComfyUI holds the local GPU.

**Tech Stack:** Go 1.25 services, Electron/TypeScript desktop, newline-delimited JSON-RPC, existing `nvpair-shared` discovery.

**Spec:** `docs/superpowers/specs/2026-09-05-llamacpp-engine-design.md`

**Constraints:** No live GGUF loads. No PAIR installer. Do not bind 8082. Do not spawn or kill llama-server. Commits use `git commit -s`. Every new file gets the two-line SPDX header.

---

## File map

| Path | Role |
|---|---|
| `services/shared/noderec/noderec.go` | `ServiceLlamaCpp = "lc"`; `EngineLoadedModels` |
| `services/nvpair-engine-manager/models.go` | Dotted `status.value` match |
| `services/nvpair-engine-manager/registry.go` | Validate `mode: "adopt"` |
| `services/nvpair-engine-manager/lifecycle.go` | Adopt-only Start (no spawn) |
| `services/nvpair-engine-manager/setport.go` | Adopt-mode port = probe override, no rebind |
| `services/nvpair-engine-manager/manifests/llamacpp.json` | Engine manifest |
| `services/llamacpp-proxy/` | New OpenAI proxy (clone of lmstudio-proxy) |
| `services/nvpair-ui-broker/` | Spawn, advertise `lc`, relay `llamacpp-proxy:` |
| `services/nvpair-job-scheduler/schedule.go` | `schedulerEngines` += `llamacpp` |
| `services/nvpair-manual-nodes/` | Probe llama.cpp on adopt port |
| `desktop/src/shared/constants/engines.ts` | Engine type `llamacpp` |
| `desktop/src/shared/constants/modular-binaries.ts` | `llamacpp-proxy` binary |
| `desktop/src/electron/service-bridge/modular-supervisor.ts` | `--llamacpp-proxy-path` |
| `desktop/src/electron/service-bridge/modular-state.ts` | Proxy source mapping |
| `desktop/src/ui/constants/engine-capabilities.ts` | No install/load/delete |
| `services/nvpair-tui/ui/proxies.go` | Third proxy row |
| `scripts/inference-dispatcher/config.go` | `--backend llamacpp` |
| `services/versions.json`, `services/build.bat`, `services/build.sh` | Build the 14th binary |

Do **not** copy LM Studio's managed-facade port steal (`lmstudioport.go`). llama.cpp must never take 8082.

---

### Task 1: Discovery key `lc` and loaded-model helper

**Files:**
- Modify: `services/shared/noderec/noderec.go`
- Modify: `services/shared/noderec/noderec_test.go`

- [ ] **Step 1: Write the failing test**

Add to `services/shared/noderec/noderec_test.go` after `TestEngineModels`:

```go
func TestEngineLoadedModels(t *testing.T) {
	n := DirectoryNode{
		Models: []string{"catalog-a", "catalog-b"},
		ModelsByEngine: map[string][]string{
			"llamacpp": {"catalog-a", "catalog-b"},
		},
		LoadedByEngine: map[string][]string{
			"llamacpp": {"catalog-a"},
		},
	}
	got := n.EngineLoadedModels("llamacpp")
	if !reflect.DeepEqual(got, []string{"catalog-a"}) {
		t.Fatalf("EngineLoadedModels(llamacpp) = %v, want [catalog-a]", got)
	}
	if got := n.EngineLoadedModels("ollama"); len(got) != 0 {
		t.Fatalf("EngineLoadedModels(missing) = %v, want empty", got)
	}
	legacy := DirectoryNode{Models: []string{"catalog-a"}}
	if got := legacy.EngineLoadedModels("llamacpp"); len(got) != 0 {
		t.Fatalf("nil LoadedByEngine must not fall back to catalog, got %v", got)
	}
}

func TestServiceLlamaCppKey(t *testing.T) {
	if ServiceLlamaCpp != "lc" {
		t.Fatalf("ServiceLlamaCpp = %q, want lc", ServiceLlamaCpp)
	}
	found := false
	for _, k := range serviceKeyOrder {
		if k == ServiceLlamaCpp {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("ServiceLlamaCpp missing from serviceKeyOrder")
	}
	if ServiceLlamaCpp.Transport() != TransportPlain {
		t.Fatal("lc transport must be TransportPlain (same as ol/lm)")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run from `services/shared`:

```
go test ./noderec -count=1 -run "TestEngineLoadedModels|TestServiceLlamaCppKey"
```

Expected: FAIL (`EngineLoadedModels` undefined and/or `ServiceLlamaCpp` undefined).

- [ ] **Step 3: Write minimal implementation**

In `services/shared/noderec/noderec.go` add after `ServiceLMStudio`:

```go
	ServiceLlamaCpp ServiceKey = "lc"
```

Add `ServiceLlamaCpp` to `serviceKeyOrder` immediately after `ServiceLMStudio`.

Add after `EngineModels`:

```go
// EngineLoadedModels returns models currently resident in memory for one
// engine. It never falls back to Models or ModelsByEngine: a missing
// LoadedByEngine report means nothing is loaded, so a router cannot treat
// catalog ids as eligible.
func (n DirectoryNode) EngineLoadedModels(engine string) []string {
	if n.LoadedByEngine == nil {
		return nil
	}
	return n.LoadedByEngine[engine]
}
```

- [ ] **Step 4: Run the tests and make sure they pass**

```
go test ./noderec -count=1 -run "TestEngineLoadedModels|TestServiceLlamaCppKey|TestEngineModels"
```

Expected: PASS.

- [ ] **Step 5: Commit**

```
git add services/shared/noderec/noderec.go services/shared/noderec/noderec_test.go
git commit -s -m "feat(noderec): add lc service key and EngineLoadedModels"
```

---

### Task 2: Dotted ResultMatch for `status.value`

**Files:**
- Modify: `services/nvpair-engine-manager/models.go`
- Modify: `services/nvpair-engine-manager/models_test.go`

llama.cpp `/v1/models` rows look like `{"id":"...","status":{"value":"loaded"}}`. Existing `ResultMatch.In` only reads a top-level string field.

- [ ] **Step 1: Write the failing test**

Append a case to the `extractStrings` table in `models_test.go`:

```go
		{
			name: "dotted status.value keeps only loaded llama.cpp rows",
			raw:  `{"data":[{"id":"a","status":{"value":"loaded"}},{"id":"b","status":{"value":"unloaded"}},{"id":"c"},{"id":"d","status":{"value":"loaded"}}]}`,
			spec: &ActionResult{Array: "data", Field: "id", Match: &ResultMatch{Field: "status.value", In: []string{"loaded"}}},
			want: []string{"a", "d"},
		},
		{
			name: "dotted match: missing status is unloaded",
			raw:  `{"data":[{"id":"a","status":{"value":"loaded"}},{"id":"b"}]}`,
			spec: &ActionResult{Array: "data", Field: "id", Match: &ResultMatch{Field: "status.value", In: []string{"loaded"}}},
			want: []string{"a"},
		},
```

- [ ] **Step 2: Run test to verify it fails**

```
cd services/nvpair-engine-manager
go test -count=1 -run TestExtractStrings
```

Expected: FAIL on the dotted `status.value` cases (field lookup misses nested object).

- [ ] **Step 3: Write minimal implementation**

Replace `matchRow` in `models.go` so `Field` may be a dotted path. Keep nonempty and `In` behavior:

```go
func lookupField(el map[string]json.RawMessage, field string) (json.RawMessage, bool) {
	obj := el
	parts := strings.Split(field, ".")
	for i, part := range parts {
		fv, ok := obj[part]
		if !ok {
			return nil, false
		}
		if i == len(parts)-1 {
			return fv, true
		}
		next := map[string]json.RawMessage{}
		if err := json.Unmarshal(fv, &next); err != nil {
			return nil, false
		}
		obj = next
	}
	return nil, false
}

func matchRow(el map[string]json.RawMessage, m *ResultMatch) bool {
	fv, ok := lookupField(el, m.Field)
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
```

Add `"strings"` to `models.go` imports if missing.

- [ ] **Step 4: Run the tests and make sure they pass**

```
go test -count=1 -run TestExtractStrings
```

Expected: PASS, including existing LM Studio nonempty cases.

- [ ] **Step 5: Commit**

```
git add services/nvpair-engine-manager/models.go services/nvpair-engine-manager/models_test.go
git commit -s -m "feat(engine-manager): match nested JSON fields like status.value"
```

---

### Task 3: Adopt-only runtime mode

**Files:**
- Modify: `services/nvpair-engine-manager/registry.go` (`modeOrDefault` validation)
- Modify: `services/nvpair-engine-manager/lifecycle.go` (`doStart`)
- Modify: `services/nvpair-engine-manager/registry_test.go`
- Create: `services/nvpair-engine-manager/adopt_test.go`

- [ ] **Step 1: Write the failing tests**

In `registry_test.go` add a validation case: `runtime.mode: "adopt"` with `ready` set and empty `bin`/`start` must load; `adopt` with a `bin` must fail; `adopt` without `ready` must fail.

In `adopt_test.go`:

```go
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

func TestAdoptModeStartsWithoutSpawning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "m1", "status": map[string]string{"value": "unloaded"}}},
		})
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(u.Port())

	m := adoptManifest(port)
	ex := newTestExecutor(t, m)
	if err := ex.Start(context.Background(), "llamacpp"); err != nil {
		t.Fatalf("adopt start: %v", err)
	}
	st, err := ex.Status("llamacpp")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || !st.Healthy || st.Port != port {
		t.Fatalf("status = %+v", st)
	}
	state, _ := ex.state("llamacpp")
	state.mu.Lock()
	proc := state.proc
	state.mu.Unlock()
	if proc != nil {
		t.Fatal("adopt mode spawned a process")
	}
}

func TestAdoptModeDoesNotSpawnWhenDown(t *testing.T) {
	m := adoptManifest(1) // nothing listens on :1
	ex := newTestExecutor(t, m)
	err := ex.Start(context.Background(), "llamacpp")
	if err == nil {
		t.Fatal("expected start error when probe fails")
	}
	st, _ := ex.Status("llamacpp")
	if st.Running {
		t.Fatal("must not mark running")
	}
}
```

`newTestExecutor(t, m *Manifest)` already exists in `executor_test.go` (same package `main`). `adoptManifest(port int) *Manifest` must return `Engine: "llamacpp"`, `DisplayName: "llama.cpp"`, platforms for `runtime.GOOS/runtime.GOARCH` with `Runtime.Mode: "adopt"`, `Runtime.Port: port`, `Runtime.Ready.HTTP: "http://127.0.0.1:{port}/v1/models"`.

- [ ] **Step 2: Run test to verify it fails**

```
go test -count=1 -run "TestAdoptMode"
```

Expected: FAIL (mode `adopt` rejected by validate, or Start tries to spawn).

- [ ] **Step 3: Write minimal implementation**

`registry.go` `Platform.validate`:

```go
	case "adopt":
		if strings.TrimSpace(p.Runtime.Bin) != "" {
			return fmt.Errorf("platform %q: runtime.bin is forbidden in adopt mode", key)
		}
		if len(p.Runtime.Start) != 0 {
			return fmt.Errorf("platform %q: runtime.start is forbidden in adopt mode", key)
		}
		if p.Runtime.Ready == nil {
			return fmt.Errorf("platform %q: runtime.ready is required in adopt mode", key)
		}
```

`lifecycle.go` `doStart`, after the `presence.Identified` adopt-success block and before `bringUp`:

```go
	if rt.modeOrDefault() == "adopt" {
		return fmt.Errorf("cannot start engine %q: nothing is serving on port %d (PAIR will not launch llama-server)", engine, port)
	}
```

Do not call `bringUp` for adopt mode.

- [ ] **Step 4: Run the tests and make sure they pass**

```
go test -count=1 -run "TestAdoptMode|TestManifestValidate"
go test -count=1
```

Expected: new tests PASS; existing executor tests still PASS.

- [ ] **Step 5: Commit**

```
git add services/nvpair-engine-manager/registry.go services/nvpair-engine-manager/lifecycle.go services/nvpair-engine-manager/registry_test.go services/nvpair-engine-manager/adopt_test.go
git commit -s -m "feat(engine-manager): add adopt-only runtime mode"
```

---

### Task 4: Adopt-mode set-port probes only

**Files:**
- Modify: `services/nvpair-engine-manager/setport.go`
- Modify: `services/nvpair-engine-manager/setport_test.go`

Spec: changing the adopt port persists and changes which loopback port PAIR probes. It must not stop llama-server or spawn on the new port.

- [ ] **Step 1: Write the failing test**

In `setport_test.go` (create if missing; same package):

```go
func TestSetPortAdoptModeDoesNotStopListener(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	oldPort, _ := strconv.Atoi(u.Port())

	ex := newTestExecutor(t, adoptManifest(oldPort))
	if err := ex.Start(context.Background(), "llamacpp"); err != nil {
		t.Fatal(err)
	}
	before := hits
	_, err := ex.SetPort(context.Background(), "llamacpp", oldPort+1)
	if err != nil {
		t.Fatalf("set-port adopt: %v", err)
	}
	if !srvOpen(srv) {
		t.Fatal("set-port stopped the foreign listener")
	}
	st, _ := ex.Status("llamacpp")
	if st.Port != oldPort+1 {
		t.Fatalf("port = %d, want %d", st.Port, oldPort+1)
	}
	if hits < before {
		t.Fatal("listener should still be reachable on the old port")
	}
}
```

`srvOpen` can be a 1-line `http.Get(srv.URL + "/v1/models")` that expects no connection refused.

- [ ] **Step 2: Run test to verify it fails**

```
go test -count=1 -run TestSetPortAdoptModeDoesNotStopListener
```

Expected: FAIL because current `SetPort` refuses adopted engines or stops them.

- [ ] **Step 3: Write minimal implementation**

At the top of `SetPort`, after loading `st` and `wasRunning`/`adopted`:

```go
	if st.plat.Runtime.modeOrDefault() == "adopt" {
		if err := e.persistPort(engine, port); err != nil {
			return EngineStatus{}, err
		}
		st.mu.Lock()
		st.port = port
		if st.plat != nil {
			st.plat.Runtime.Port = port
		}
		st.mu.Unlock()
		if wasRunning {
			_ = e.doStart(ctx, st, engine, startOpts{})
		}
		return e.snapshot(engine, st), nil
	}
```

`doStart` in adopt mode only re-probes; it must not call `doStop` first. Skip the existing “refuse adopted process-mode” branch when mode is `adopt`.

`snapshot(engine, st)` is the existing SetPort return helper in `setport.go`.

- [ ] **Step 4: Run the tests and make sure they pass**

```
go test -count=1 -run "TestSetPort"
```

Expected: PASS.

- [ ] **Step 5: Commit**

```
git add services/nvpair-engine-manager/setport.go services/nvpair-engine-manager/setport_test.go
git commit -s -m "feat(engine-manager): adopt-mode set-port only changes the probe"
```

---

### Task 5: `llamacpp.json` manifest

**Files:**
- Create: `services/nvpair-engine-manager/manifests/llamacpp.json`
- Modify: `services/nvpair-engine-manager/registry_test.go` (bundled manifest load already walks `manifests/`)

- [ ] **Step 1: Write the failing test**

If bundled-manifest tests already load every JSON in `manifests/`, adding the file is enough. Add an explicit test:

```go
func TestLlamaCppManifestIsAdoptOnly(t *testing.T) {
	raw, err := os.ReadFile("manifests/llamacpp.json")
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Engine != "llamacpp" || m.DisplayName != "llama.cpp" {
		t.Fatalf("identity = %s %s", m.Engine, m.DisplayName)
	}
	if _, ok := m.Actions["pull_model"]; ok {
		t.Fatal("pull_model must not exist")
	}
	if _, ok := m.Actions["load_model"]; ok {
		t.Fatal("load_model must not exist")
	}
	for key, p := range m.Platforms {
		if p.Runtime.modeOrDefault() != "adopt" {
			t.Fatalf("%s mode = %q", key, p.Runtime.Mode)
		}
		if p.Runtime.Port != 8082 {
			t.Fatalf("%s port = %d", key, p.Runtime.Port)
		}
		if p.Runtime.Ready == nil || !strings.Contains(p.Runtime.Ready.HTTP, "/v1/models") {
			t.Fatalf("%s ready probe must be /v1/models", key)
		}
		loaded := m.Actions["loaded_models"]
		if loaded.Result == nil || loaded.Result.Match == nil || loaded.Result.Match.Field != "status.value" {
			t.Fatal("loaded_models must match status.value")
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```
go test -count=1 -run TestLlamaCppManifestIsAdoptOnly
```

Expected: FAIL (file missing).

- [ ] **Step 3: Write the manifest**

Create `services/nvpair-engine-manager/manifests/llamacpp.json`:

```json
{
  "engine": "llamacpp",
  "display_name": "llama.cpp",
  "manifest_version": 1,
  "runtime": {
    "mode": "adopt",
    "bind": "127.0.0.1",
    "port": 8082,
    "ready": { "http": "http://127.0.0.1:{port}/v1/models", "status": 200, "timeout_s": 5 },
    "health": { "http": "http://127.0.0.1:{port}/v1/models", "status": 200, "interval_s": 5 }
  },
  "platforms": {
    "windows/amd64": {
      "detect": [
        "%LOCALAPPDATA%\\Microsoft\\WinGet\\Packages\\ggml.llamacpp_Microsoft.Winget.Source_8wekyb3d8bbwe\\llama-server.exe",
        "C:\\Users\\P-DLE\\Desktop\\AI Playground\\vendor\\llama.cpp-official\\bin\\llama-server.exe"
      ]
    },
    "windows/arm64": {
      "detect": [
        "%LOCALAPPDATA%\\Microsoft\\WinGet\\Packages\\ggml.llamacpp_Microsoft.Winget.Source_8wekyb3d8bbwe\\llama-server.exe"
      ]
    },
    "darwin/arm64": { "detect": ["/opt/homebrew/bin/llama-server", "/usr/local/bin/llama-server"] },
    "darwin/amd64": { "detect": ["/usr/local/bin/llama-server"] },
    "linux/amd64": { "detect": ["/usr/local/bin/llama-server", "/usr/bin/llama-server"] },
    "linux/arm64": { "detect": ["/usr/local/bin/llama-server", "/usr/bin/llama-server"] }
  },
  "actions": {
    "list_models": {
      "description": "List every model id advertised by llama-server.",
      "http": { "method": "GET", "path": "/v1/models" },
      "result": { "array": "data", "field": "id" }
    },
    "loaded_models": {
      "description": "List model ids currently loaded in memory.",
      "http": { "method": "GET", "path": "/v1/models" },
      "result": {
        "array": "data",
        "field": "id",
        "match": { "field": "status.value", "in": ["loaded"] }
      }
    }
  }
}
```

Each platform still needs a `runtime` object after merge. Confirm `registry.go` deep-merges top-level `runtime` onto platforms (Ollama does this). If a platform with only `detect` fails validate, copy the top-level runtime into each platform block.

- [ ] **Step 4: Run the tests and make sure they pass**

```
go test -count=1 -run "TestLlamaCppManifest|TestLoadBundled"
go test -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```
git add services/nvpair-engine-manager/manifests/llamacpp.json services/nvpair-engine-manager/registry_test.go
git commit -s -m "feat(engine-manager): add adopt-only llamacpp manifest"
```

---

### Task 6: `llamacpp-proxy` (clone + loaded-only eligibility)

**Files:**
- Create: `services/llamacpp-proxy/` (copy of `services/lmstudio-proxy/`)
- Modify after copy: `go.mod`, `portstore.go`, `discovery.go`, `proxy.go` (`subscribedToNode`), `README.md`
- Create: `services/llamacpp-proxy/loaded_eligibility_test.go`

- [ ] **Step 1: Copy the module**

From `services/`:

```
Copy-Item -Recurse lmstudio-proxy llamacpp-proxy
```

In `llamacpp-proxy/go.mod` set `module llamacpp-proxy`.

In `portstore.go`:

```go
const proxyPortFile = "llamacpp-proxy-port.json"
const defaultProxyPort = 8084
```

Remove `legacyDefaultProxyPort` special-case for 1235; `chooseStartupPort` should honor persisted 8084 and ignore nothing except invalid values.

Replace every `noderec.ServiceLMStudio` with `noderec.ServiceLlamaCpp`.
Replace engine string `"lmstudio"` with `"llamacpp"` in workload tags and `EngineModels` calls.
Replace JSON-RPC log names `lmstudio-proxy` with `llamacpp-proxy`.

- [ ] **Step 2: Write the failing eligibility test**

`loaded_eligibility_test.go`:

```go
func TestSubscribedToNodeUsesLoadedNotCatalog(t *testing.T) {
	n := noderec.DirectoryNode{
		Name:     "box",
		HostUUID: "uuid-1",
		IP:       "127.0.0.1",
		Services: map[noderec.ServiceKey]noderec.ServiceStatus{
			noderec.ServiceLlamaCpp: {Port: 8084},
		},
		ModelsByEngine: map[string][]string{
			"llamacpp": {"ISTA-DASLab-Qwen3.8-27B-GSQ-RCO-GGUF_IQ3_XXS-mtp", "other"},
		},
		LoadedByEngine: map[string][]string{
			"llamacpp": {"ISTA-DASLab-Qwen3.8-27B-GSQ-RCO-GGUF_IQ3_XXS-mtp"},
		},
	}
	node, ok := subscribedToNode(n)
	if !ok {
		t.Fatal("expected lc node")
	}
	if !nodeAdvertisesModel(node, "ISTA-DASLab-Qwen3.8-27B-GSQ-RCO-GGUF_IQ3_XXS-mtp") {
		t.Fatal("loaded id must be eligible")
	}
	if nodeAdvertisesModel(node, "other") {
		t.Fatal("catalog-only id must not be eligible")
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

```
cd services/llamacpp-proxy
go test -count=1 -run TestSubscribedToNodeUsesLoadedNotCatalog
```

Expected: FAIL because `subscribedToNode` still copies `EngineModels` (catalog).

- [ ] **Step 4: Fix `subscribedToNode`**

In `proxy.go` `subscribedToNode`:

```go
	svc, ok := n.Services[noderec.ServiceLlamaCpp]
	// ...
		Models: append([]string(nil), n.EngineLoadedModels("llamacpp")...),
```

- [ ] **Step 5: Add a proxy test that catalog-only chat is 502 with zero upstream hits**

Follow `proxy_test.go` patterns: httptest backend that increments `hits` on `POST /v1/chat/completions`. Seed a node whose `Models` is empty (nothing loaded) but whose backend `/v1/models` would list ids. POST chat with `{"model":"other","messages":[]}`. Expect HTTP 502 and `hits == 0`.

If existing tests assume `EngineModels`, update those in the clone to use loaded lists.

- [ ] **Step 6: Run all proxy tests**

```
go test ./... -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```
git add services/llamacpp-proxy
git commit -s -m "feat: add llamacpp-proxy with loaded-only routing"
```

---

### Task 7: Broker spawn, advertise, relay

**Files:**
- Create: `services/nvpair-ui-broker/llamacppproxy.go` (mirror `lmstudioproxy.go`, no facade steal)
- Modify: `services/nvpair-ui-broker/advertiser.go` (add `runAutoAdvertiseLlamaCpp`)
- Modify: `services/nvpair-ui-broker/broker.go` (path flag, spawn, shutdown, `llamacpp-proxy:` relay)
- Modify: `services/nvpair-ui-broker/advertiser_test.go`
- Create: `services/nvpair-ui-broker/llamacpp_advertise_test.go`

Do **not** port `lmstudioport.go` managed facade.

- [ ] **Step 1: Write the failing advertise test**

```go
func TestReconcileAdvertiseLlamaCppRegistersProxyPort(t *testing.T) {
	// Fake engine-manager status: llamacpp running on 8082.
	// Fake proxy listen port 8084.
	// Expect registerService(lc, 8084) and set-local-backend port 8082.
}
```

Mirror `advertiser_test.go` LM Studio cases. Also test: engine down → unregister `lc`; equal proxy/engine ports → do not register.

- [ ] **Step 2: Run test to verify it fails**

```
cd services/nvpair-ui-broker
go test -count=1 -run TestReconcileAdvertiseLlamaCpp
```

Expected: FAIL (no `runAutoAdvertiseLlamaCpp`).

- [ ] **Step 3: Implement**

`llamacppproxy.go`: copy `getLMStudioProxy` / `spawnLMStudioProxy` / relay helpers, rename to LlamaCpp, default startup port **8084**, worker name `llamacpp-proxy`, engine id `llamacpp`.

`advertiser.go`: add `defaultLlamaCppPort = 8082` and `runAutoAdvertiseLlamaCpp` cloned from LM Studio, using `noderec.ServiceLlamaCpp` and `b.getLlamaCppProxy()`. Health check: `GET http://127.0.0.1:{enginePort}/v1/models`.

`broker.go`:
- Add `llamaCppProxyPath string` and `--llamacpp-proxy-path` (same parsing as `--lmstudio-proxy-path`).
- Spawn when path is set (optional, warn and continue if missing).
- Relay `llamacpp-proxy:` the same way as `lmstudio-proxy:`.
- On `schedule:priority` for engine `llamacpp`, `node/set-priority` to the llama.cpp proxy.
- Shutdown: stop llama.cpp proxy with LM Studio (before engine-manager is fine).

Do not bridge manual llama.cpp nodes in this task. Task 9 adds `llamacpp_up` and the broker `node/add-manual` call together.

- [ ] **Step 4: Run broker tests**

```
go test -count=1 -run "LlamaCpp|LMStudio|Advertise"
go test -count=1
```

Expected: PASS. Existing LM Studio tests unchanged.

- [ ] **Step 5: Commit**

```
git add services/nvpair-ui-broker
git commit -s -m "feat(broker): supervise llamacpp-proxy and advertise lc"
```

---

### Task 8: Job scheduler

**Files:**
- Modify: `services/nvpair-job-scheduler/schedule.go` (`schedulerEngines`)
- Modify: `services/nvpair-job-scheduler/schedule_test.go` (loops already use `schedulerEngines`; add one explicit `llamacpp` upsert if a test hard-codes two engines)

- [ ] **Step 1: Write the failing test**

```go
func TestSchedulerEnginesIncludesLlamaCpp(t *testing.T) {
	found := false
	for _, e := range schedulerEngines {
		if e == "llamacpp" {
			found = true
		}
	}
	if !found {
		t.Fatal("schedulerEngines missing llamacpp")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```
cd services/nvpair-job-scheduler
go test -count=1 -run TestSchedulerEnginesIncludesLlamaCpp
```

Expected: FAIL.

- [ ] **Step 3: Implement**

```go
var schedulerEngines = []string{"ollama", "lmstudio", "llamacpp"}
```

- [ ] **Step 4: Run tests**

```
go test -count=1
```

Expected: PASS.

- [ ] **Step 5: Commit**

```
git add services/nvpair-job-scheduler/schedule.go services/nvpair-job-scheduler/schedule_test.go
git commit -s -m "feat(scheduler): rank llamacpp alongside ollama and lmstudio"
```

---

### Task 9: Manual-node llama.cpp probe

**Files:**
- Modify: `services/nvpair-manual-nodes/manager.go`
- Modify: `services/nvpair-manual-nodes/manager_test.go`
- Modify: `services/nvpair-ui-broker/` manual-node bridge (add-manual into llamacpp-proxy)

- [ ] **Step 1: Write the failing test**

Extend the existing probe test (or add `TestProbeLlamaCpp`): a httptest server on a chosen port serving `GET /v1/models` with one loaded id must set `llamacpp_up=true`, `llamacpp_port=<that port>`, and `llamacpp_models` containing only loaded ids.

Default probe port is 8082. Tests should pass an explicit port to the probe helper rather than binding 8082.

- [ ] **Step 2: Run test to verify it fails**

```
cd services/nvpair-manual-nodes
go test -count=1 -run TestProbeLlamaCpp
```

Expected: FAIL (no llamacpp fields).

- [ ] **Step 3: Implement**

Add to the discovered-node payload:

```go
LlamaCppUp     bool     `json:"llamacpp_up"`
LlamaCppPort   int      `json:"llamacpp_port,omitempty"`
LlamaCppModels []string `json:"llamacpp_models,omitempty"`
```

Probe `GET http://{addr}:{port}/v1/models` (port from node override, else 8082). Parse `data[].id` and `data[].status.value`. Loaded ids go to `llamacpp_models` used for routing; keep a separate catalog slice only if the existing Ollama/LM Studio pattern stores full lists on the event. Match LM Studio: liveness and model list from the same `/v1/models` call.

Broker: when `llamacpp_up`, call `node/add-manual` on `getLlamaCppProxy()` with host/port from the event.

- [ ] **Step 4: Run tests**

```
go test -count=1
cd ../nvpair-ui-broker
go test -count=1 -run Manual
```

Expected: PASS.

- [ ] **Step 5: Commit**

```
git add services/nvpair-manual-nodes services/nvpair-ui-broker
git commit -s -m "feat(manual-nodes): probe llama.cpp and bridge into llamacpp-proxy"
```

---

### Task 10: Desktop engine type, capabilities, supervisor

**Files:**
- Modify: `desktop/src/shared/constants/engines.ts`
- Modify: `desktop/src/ui/constants/engine-capabilities.ts`
- Modify: `desktop/src/shared/constants/modular-binaries.ts`
- Modify: `desktop/src/electron/service-bridge/modular-supervisor.ts`
- Modify: `desktop/src/electron/service-bridge/modular-state.ts`
- Create: `desktop/tests/modular/llamacpp-engine.test.ts`

- [ ] **Step 1: Write the failing test**

`desktop/tests/modular/llamacpp-engine.test.ts`:

```ts
import { describe, expect, it } from 'vitest'
import { EngineDisplayNames, EnabledEngineTypes, EngineTypes } from '@/shared/constants/engines'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import { MODULAR_RUNTIME_BINARIES } from '@/shared/constants/modular-binaries'

describe('llamacpp engine', () => {
    it('is a enabled engine type', () => {
        expect(EngineTypes).toContain('llamacpp')
        expect(EnabledEngineTypes).toContain('llamacpp')
        expect(EngineDisplayNames.llamacpp).toBe('llama.cpp')
    })
    it('cannot install, load, eject, or delete', () => {
        const caps = EngineCapabilities.llamacpp
        expect(caps.hasInstall).toEqual([])
        expect(caps.hasEject).toBe(false)
        expect(caps.hasDeleteModel).toBe(false)
        expect(caps.hasEnginePort).toBe(true)
        expect(caps.engineHub).toBeUndefined()
    })
    it('ships llamacpp-proxy as a broker-owned binary', () => {
        const bin = MODULAR_RUNTIME_BINARIES.find(b => b.processName === 'llamacpp-proxy')
        expect(bin?.baseName).toBe('llamacpp-proxy')
        expect(bin?.launchOwner).toBe('broker')
        expect(bin?.optional).toBe(true)
    })
})
```

- [ ] **Step 2: Run test to verify it fails**

```
cd desktop
npx vitest run tests/modular/llamacpp-engine.test.ts
```

Expected: FAIL (`llamacpp` not in EngineTypes).

- [ ] **Step 3: Implement**

`engines.ts`:

```ts
export const EngineTypes = ['ollama', 'lm-studio', 'llamacpp'] as const
export const EnabledEngineTypes: EngineType[] = ['ollama', 'lm-studio', 'llamacpp'] as const
export const EngineDisplayNames: Record<EngineType, string> = {
    ollama: 'Ollama',
    'lm-studio': 'LM Studio',
    llamacpp: 'llama.cpp'
} as const
export const EngineDefaultLinks: Record<EngineType, { docsUrl: string; installUrl: string }> = {
    ollama: { docsUrl: 'https://docs.ollama.com/', installUrl: 'https://ollama.com/download' },
    'lm-studio': { docsUrl: 'https://lmstudio.ai/docs', installUrl: 'https://lmstudio.ai/' },
    llamacpp: { docsUrl: 'https://github.com/ggml-org/llama.cpp', installUrl: 'https://github.com/ggml-org/llama.cpp' }
}
```

`engine-capabilities.ts` add:

```ts
    llamacpp: {
        hasExpiry: false,
        hasEject: false,
        hasInstall: [],
        hasEnginePort: true,
        hasInstallPath: false,
        hasProxyWebUI: false,
        hasPreferredNode: false,
        hasCrashAlert: false,
        hasModelSearchOnlyWhenRunning: true,
        modelOpsWhenStopped: false,
        hasDeleteModel: false
    }
```

`modular-binaries.ts`: add `'llamacpp-proxy'` to `ModularProcessName` and a broker-owned optional entry (`baseName: 'llamacpp-proxy'`, `needsFirewallAccess: true`).

`modular-supervisor.ts`:
- `engineManagerId`: `llamacpp` stays `llamacpp`
- `proxyEngineFromManagerId`: `id === 'llamacpp' → 'llamacpp'`
- `proxyRelayPrefix`: `llamacpp` → `'llamacpp-proxy'`
- `brokerStartupArgs`: `passPath('--llamacpp-proxy-path', 'llamacpp-proxy')`

`modular-state.ts`: extend `ProxyNodeSource` with `'llamacpp-proxy'` and map it to engine `'llamacpp'`. Replace remaining `engine === 'ollama' ? 'ollama-proxy' : 'lmstudio-proxy'` ternaries with a three-way helper:

```ts
function proxySourceForEngine(engine: EngineType): ProxyNodeSource {
    if (engine === 'ollama') return 'ollama-proxy'
    if (engine === 'lm-studio') return 'lmstudio-proxy'
    return 'llamacpp-proxy'
}
```

Endpoint copy: wherever LM Studio shows `http://127.0.0.1:${proxyPort}/v1`, llama.cpp uses the same pattern (proxy default 8084). Do not hardcode 8082 as the app endpoint.

- [ ] **Step 4: Run tests and typecheck**

```
npx vitest run tests/modular/llamacpp-engine.test.ts
npm run typecheck
```

Expected: PASS. Fix every `EngineType` exhaustiveness error the compiler reports (switch statements, records). That is required, not optional.

- [ ] **Step 5: Commit**

```
git add desktop/src desktop/tests/modular/llamacpp-engine.test.ts
git commit -s -m "feat(desktop): enable llamacpp engine and llamacpp-proxy binary"
```

---

### Task 11: TUI and inference dispatcher

**Files:**
- Modify: `services/nvpair-tui/ui/proxies.go`
- Modify: `services/nvpair-tui/ui/health.go`
- Modify: `scripts/inference-dispatcher/config.go`
- Modify: `scripts/inference-dispatcher/dispatcher_test.go`
- Modify: `desktop/src/shared/types/inference-dispatcher.ts`

- [ ] **Step 1: Write failing tests**

TUI: if `proxies.go` has a table of engines, add a test that the table includes `{label: "llama.cpp", prefix: "llamacpp-proxy"}`.

Dispatcher:

```go
func TestBackendLlamaCpp(t *testing.T) {
	cfg, err := parseConfig([]string{"--backend", "llamacpp", "--prompt", "hi"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backend != "llamacpp" {
		t.Fatalf("backend = %q", cfg.Backend)
	}
	if defaultPort(cfg) != 8084 {
		t.Fatalf("port = %d, want 8084", defaultPort(cfg))
	}
}
```

Use the real function names from `config.go` (`defaultLMStudioPort` analogue).

- [ ] **Step 2: Run tests to verify they fail**

```
cd services/nvpair-tui
go test ./ui -count=1 -run LlamaCpp
cd ../../scripts/inference-dispatcher
go test -count=1 -run TestBackendLlamaCpp
```

Expected: FAIL.

- [ ] **Step 3: Implement**

`proxies.go`: add `{label: "llama.cpp", prefix: "llamacpp-proxy", table: newTable(nil)}` next to LM Studio. Handle `llamacpp-proxy:` in the method switch the same as `lmstudio-proxy:`.

`health.go`: append `"llamacpp-proxy"` to the worker list.

`config.go`: allow `--backend llamacpp`; default port 8084; OpenAI chat path like LM Studio (`/v1/chat/completions`). Reuse LM Studio list/parse helpers when `backend == "llamacpp"` (same `/v1/models`).

`inference-dispatcher.ts`: `DispatcherBackend = 'ollama' | 'lmstudio' | 'llamacpp'`.

- [ ] **Step 4: Run tests**

```
go test ./... -count=1
```

from each module. Expected: PASS.

- [ ] **Step 5: Commit**

```
git add services/nvpair-tui scripts/inference-dispatcher desktop/src/shared/types/inference-dispatcher.ts
git commit -s -m "feat: expose llamacpp in TUI and inference dispatcher"
```

---

### Task 12: Versions, build scripts, docs, contracts

**Files:**
- Modify: `services/versions.json`
- Modify: `services/build.bat`
- Modify: `services/build.sh`
- Modify: `services/bom.md`
- Modify: `docs/overview.mdx`, `docs/architecture.mdx`, `docs/engine-lifecycle.mdx`, `docs/getting-started.mdx`
- Modify: `desktop/docs/architecture.md`, `desktop/docs/services-backend.md`
- Run: `npm run service-contracts:write` from `desktop/` after broker RPC surface changes

Version bumps (VERSIONING.md, MINOR = additive IPC/HTTP):

| Component | From | To | Why |
|---|---|---|---|
| `llamacpp-proxy` | (new) | `0.1.0` | new binary |
| `nvpair-engine-manager` | `0.17.4` | `0.18.0` | adopt mode + manifest |
| `nvpair-ui-broker` | `0.40.2` | `0.41.0` | new worker + `lc` advertise |
| `nvpair-job-scheduler` | `0.4.1` | `0.5.0` | new engine |
| `nvpair-manual-nodes` | `0.11.1` | `0.12.0` | llamacpp probe fields |
| `nvpair-tui` | `0.7.2` | `0.8.0` | third proxy |
| `product` / `installer` | `0.91.7` | `0.92.0` | user-visible engine |

Do not bump `desktop/package.json` unless cutting an Electron release.

- [ ] **Step 1: Add `llamacpp-proxy` to `versions.json` and both build scripts**

`build.bat` after the lmstudio-proxy `for /f` line:

```
for /f "delims=" %%V in ('jq -r --arg k "llamacpp-proxy" ".components[$k]" "%VERSIONS_FILE%"') do set "V_LCPROXY=%%V"
```

Add a `go build` for `llamacpp-proxy` next to `lmstudio-proxy`, ldflag `-X main.Version=%V_LCPROXY%`. Mirror in `build.sh` (`V_LCPROXY`, echo, build). Change comments that say “thirteen” binaries to “fourteen”.

- [ ] **Step 2: Docs**

In overview/architecture/engine-lifecycle/getting-started: engine list becomes Ollama, LM Studio, or llama.cpp. State: PAIR adopts llama-server; it does not install or load GGUFs; app endpoint is `http://127.0.0.1:8084/v1`; cluster peers need this fork; routing requires the model loaded on the serving node.

- [ ] **Step 3: Service contracts**

```
cd desktop
npm run service-contracts:write
npm run service-contracts:check
```

Expected: check PASS.

- [ ] **Step 4: Full verification (no live GGUF)**

```
cd services/nvpair-engine-manager ; go test -count=1
cd ../llamacpp-proxy ; go test -count=1
cd ../nvpair-ui-broker ; go test -count=1
cd ../nvpair-job-scheduler ; go test -count=1
cd ../nvpair-manual-nodes ; go test -count=1
cd ../nvpair-tui ; go test -count=1
cd ../shared ; go test ./... -count=1
cd ../../desktop ; npm run typecheck ; npm run test:unit
```

Expected: all PASS. Do not start llama-server. Do not run the PAIR installer.

- [ ] **Step 5: Commit**

```
git add services/versions.json services/build.bat services/build.sh services/bom.md docs desktop/docs desktop/docs/services-api.md
git commit -s -m "chore: version, build, and document the llamacpp engine"
```

---

## Spec coverage (self-review)

| Spec requirement | Task |
|---|---|
| Adopt-only, no spawn/kill/install | 3, 5 |
| Default adopt port 8082, per-node probe override | 4, 5 |
| PAIR facade 8084, never bind adopt port | 6, 7 |
| Discovery `lc` | 1, 7 |
| Wire id `llamacpp`, display `llama.cpp` | 5, 10 |
| Full catalog list | 5 `list_models`, 6 GET `/v1/models` |
| Loaded-only routing; catalog-only 502; zero upstream hits | 1, 2, 6 |
| Missing `status` = unloaded | 2 |
| Local unloaded + peer loaded = forward | 6, 7 |
| Manual nodes | 9 |
| Scheduler | 8 |
| Desktop/TUI/dispatcher | 10, 11 |
| No ComfyUI detection, no GGUF load | 5 (no load action), 6 (no forward) |
| Tests use fake `/v1`, not 8082/27B | all test steps |
| Build 14th binary + versions | 12 |

No placeholders. Types: engine id is `llamacpp` everywhere except desktop display `llama.cpp` and LM Studio’s existing `lm-studio` hyphen type.
