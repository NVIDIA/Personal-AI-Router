// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nvpair-shared/engines"
	settings "nvpair-shared/enginesettings"
	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

type settingsHarness struct {
	b                   *Broker
	applies             atomic.Int32
	fail                atomic.Bool
	failBeforeStop      atomic.Bool
	loseProxyOnStop     atomic.Bool
	failResultSave      atomic.Bool
	otherEnginePort     atomic.Int32
	previewPreserveCORS atomic.Bool
	entered, release    chan struct{}
	// launchMu guards the fixture's launch state, which a suspended
	// engine:configure-launch mutates while the reader loop keeps serving.
	launchMu sync.Mutex
}

// newSettingsHarness stands up a broker with a fixture engine manager and one
// fixture proxy process hosting a ready facade for every engine in the table,
// as production does. The fixture engine manager serves a single launch state
// for whichever engine is asked about.
func newSettingsHarness(t *testing.T) *settingsHarness {
	t.Helper()
	// One ephemeral port for the fixture engine's server, then one facade port
	// per engine.
	ports := make([]int, 1+len(engineProxyProfiles))
	listeners := []net.Listener{}
	for i := range ports {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, ln)
		ports[i] = ln.Addr().(*net.TCPAddr).Port
	}
	for _, ln := range listeners {
		_ = ln.Close()
	}
	h := &settingsHarness{b: &Broker{codec: NewCodec(&bytes.Buffer{}), nodeID: "target", clusterDir: filepath.Join(t.TempDir(), "cluster")}}
	worker, codec := newTestRPCWorkerPipe(t)
	h.b.setEngineMgr(worker)
	proxyWorker, proxyCodec := newTestRPCWorkerPipe(t)
	proxy := &proxyProcess{peer: proxyWorker.peer, facadeState: make(map[string]proxyFacadeState, len(engineProxyProfiles))}
	for i, profile := range engineProxyProfiles {
		proxy.facadeState[profile.Name] = proxyFacadeState{ready: true, port: ports[1+i]}
		h.b.setEngineProxyHandle(profile, proxy)
	}
	go func() {
		for {
			msg, err := proxyCodec.Read()
			if err != nil {
				return
			}
			if !msg.IsRequest() {
				continue
			}
			// Only a facade-addressed set-port changes fixture state; every
			// other request (node/set-local-backend and the like) is acknowledged.
			engine, bare := engines.SplitAddressedMethod(msg.Method)
			if engine == "" || bare != "set-port" {
				_ = proxyCodec.Respond(msg.ID, map[string]bool{"ok": true})
				continue
			}
			var p struct {
				Port int `json:"port"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			proxy.readyMu.Lock()
			proxy.facadeState[engine] = proxyFacadeState{ready: true, port: p.Port}
			proxy.readyMu.Unlock()
			_ = proxyCodec.Respond(msg.ID, map[string]int{"port": p.Port})
		}
	}()
	launch := settings.LaunchState{ServerPort: ports[0], EffectivePort: ports[0], LaunchText: "--fixture-option", Running: true, Editable: true, Format: "pair-arguments-v1"}
	go func() {
		for {
			msg, err := codec.Read()
			if err != nil {
				return
			}
			if !msg.IsRequest() {
				continue
			}
			switch msg.Method {
			case "engine:get-launch":
				var p settings.Request
				_ = json.Unmarshal(msg.Params, &p)
				h.launchMu.Lock()
				current := launch
				h.launchMu.Unlock()
				current.Engine = p.Engine
				_ = codec.Respond(msg.ID, current)
			case "engine:configured-ports":
				// Every engine reports the fixture's "other engine" port; the
				// broker skips the entry for the engine being configured, so
				// whichever engine a test targets, its siblings hold this port.
				configured := make([]any, 0, len(engineProxyProfiles))
				for _, profile := range engineProxyProfiles {
					configured = append(configured, map[string]any{"engine": profile.Name, "port": h.otherEnginePort.Load()})
				}
				_ = codec.Respond(msg.ID, map[string]any{"engines": configured})
			case "engine:preview-launch":
				var p settings.Request
				_ = json.Unmarshal(msg.Params, &p)
				h.previewPreserveCORS.Store(p.PreserveCORS)
				if p.Format == "pair-launch-v1" {
					p.Settings.LaunchText = strings.TrimPrefix(p.Settings.LaunchText, "managed serve ")
				}
				h.launchMu.Lock()
				restart := launch.Running && (p.Settings.ServerPort != launch.ServerPort || p.Settings.LaunchText != launch.LaunchText)
				h.launchMu.Unlock()
				_ = codec.Respond(msg.ID, settings.Preview{Settings: p.Settings, Restart: restart})
			case "engine:configure-launch":
				// Serve this off the reader loop: a suspended apply must not
				// stop the fixture from answering an unrelated read, which is
				// exactly what the broker's lock split makes possible.
				go func(msg *Message) {
					h.applies.Add(1)
					var p settings.Configure
					_ = json.Unmarshal(msg.Params, &p)
					if h.entered != nil {
						h.entered <- struct{}{}
						<-h.release
					}
					if h.failBeforeStop.Load() {
						if h.loseProxyOnStop.Load() {
							proxy.readyMu.Lock()
							state := proxy.facadeState[p.Engine]
							state.ready = false
							proxy.facadeState[p.Engine] = state
							proxy.readyMu.Unlock()
						}
						_ = codec.RespondError(msg.ID, -32000, "stop failure")
						return
					}
					h.launchMu.Lock()
					launch.ServerPort = p.Settings.ServerPort
					launch.LaunchText = p.Settings.LaunchText
					launch.Running = false
					h.launchMu.Unlock()
					if err := h.b.rebindSettingsProxy(p.Engine, p.Settings.ProxyPort); err != nil {
						_ = codec.RespondError(msg.ID, -32000, err.Error())
						return
					}
					if h.fail.Load() {
						_ = codec.RespondError(msg.ID, -32000, "startup failure")
						return
					}
					h.launchMu.Lock()
					launch.EffectivePort = p.Settings.ServerPort
					launch.Running = p.Resume
					current := launch
					h.launchMu.Unlock()
					current.Engine = p.Engine
					if h.failResultSave.Load() {
						journal, _ := h.b.engineSettingsPath()
						if err := os.Remove(journal); err != nil {
							t.Error(err)
						}
						if err := os.Mkdir(journal, 0700); err != nil {
							t.Error(err)
						}
					}
					_ = codec.Respond(msg.ID, current)
				}(msg)
			default:
				_ = codec.RespondError(msg.ID, -32601, "unsupported fixture method")
			}
		}
	}()
	return h
}

func (h *settingsHarness) request(t *testing.T) settings.Request {
	t.Helper()
	s, err := h.b.getEngineSettings(context.Background(), settings.Request{Engine: "ollama"}, "")
	if err != nil {
		t.Fatal(err)
	}
	return settings.Request{Engine: "ollama", ExpectedRevision: s.Revision, RequestID: settingsID(), Settings: s.Settings}
}

func TestSettingsCoordinatorRevisionDedupNoopAndFailure(t *testing.T) {
	h := newSettingsHarness(t)
	p := h.request(t)
	receipt, err := h.b.applyEngineSettings(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	if h.applies.Load() != 0 || receipt.Phase != "succeeded" {
		t.Fatal("no-op mutated runtime")
	}
	p = h.request(t)
	p.Settings.LaunchText += " --parallel 2"
	receipt, err = h.b.applyEngineSettings(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Revision != p.ExpectedRevision+1 || receipt.Phase != "succeeded" || h.applies.Load() != 1 {
		t.Fatalf("receipt=%+v applies=%d", receipt, h.applies.Load())
	}
	if _, err = h.b.applyEngineSettings(context.Background(), p, ""); err != nil || h.applies.Load() != 1 {
		t.Fatal("duplicate restarted")
	}
	reused := p
	reused.Settings.LaunchText += " --different"
	if _, err = h.b.applyEngineSettings(context.Background(), reused, ""); err == nil {
		t.Fatal("reused identifier accepted")
	}
	p.RequestID = settingsID()
	if _, err = h.b.applyEngineSettings(context.Background(), p, ""); err == nil {
		t.Fatal("stale revision accepted")
	}
	p = h.request(t)
	p.Settings.LaunchText += " --invalid"
	h.fail.Store(true)
	receipt, err = h.b.applyEngineSettings(context.Background(), p, "")
	if err != nil || receipt.Phase != "failed" {
		t.Fatalf("failure receipt %+v %v", receipt, err)
	}
	s := h.b.engineSettings["ollama"].Snapshot
	if s.Settings != p.Settings || s.Running || s.AppliedRevision >= s.Revision || s.Error == "" {
		t.Fatalf("untruthful failed snapshot: %+v", s)
	}
	h.fail.Store(false)
	p = h.request(t)
	if _, err = h.b.applyEngineSettings(context.Background(), p, ""); err != nil {
		t.Fatal(err)
	}
	if s := h.b.engineSettings["ollama"].Snapshot; !s.Running || s.Phase != "succeeded" {
		t.Fatalf("retry lost resume intent: %+v", s)
	}
}

func TestSettingsFailedApplyRestoresOnlyRunningReadyService(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stopFails  bool
		proxyReady bool
		advertised bool
	}{
		{"stop failed with engine still running", true, true, true},
		{"engine stopped before startup failed", false, true, false},
		{"proxy unavailable", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newSettingsHarness(t)
			p := h.request(t)
			oldPort := p.Settings.ProxyPort
			h.b.regCache = relay.NewRegistrationCache()
			h.b.registerService(noderec.RegisterParams{Service: noderec.ServiceOllama, Port: oldPort})
			h.failBeforeStop.Store(tc.stopFails)
			h.fail.Store(true)
			h.loseProxyOnStop.Store(!tc.proxyReady)
			p.Settings.LaunchText += " --new-option"
			receipt, err := h.b.applyEngineSettings(context.Background(), p, "")
			if err != nil || receipt.Phase != "failed" {
				t.Fatalf("expected failed receipt, got %+v, %v", receipt, err)
			}
			registered := h.b.regCache.Snapshot()
			if tc.advertised {
				if len(registered) != 1 || registered[0].Service != noderec.ServiceOllama || registered[0].Port != oldPort {
					t.Fatalf("running engine lost registration: %+v", registered)
				}
			} else if len(registered) != 0 {
				t.Fatalf("unavailable service advertised: %+v", registered)
			}
		})
	}
}

func TestSettingsCoordinatorSerializesAndCancelsQueuedRequests(t *testing.T) {
	h := newSettingsHarness(t)
	p := h.request(t)
	p.Settings.LaunchText += " --first"
	h.entered = make(chan struct{}, 1)
	h.release = make(chan struct{})
	done := make(chan error, 1)
	go func() { _, err := h.b.applyEngineSettings(context.Background(), p, ""); done <- err }()
	<-h.entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	other := p
	other.RequestID = settingsID()
	canceled := make(chan error, 1)
	go func() { _, err := h.b.applyEngineSettings(ctx, other, ""); canceled <- err }()
	close(h.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-canceled; err == nil {
		t.Fatal("canceled queued mutation accepted")
	}
	if h.applies.Load() != 1 {
		t.Fatal("concurrent request mutated runtime")
	}
}

// An apply stops and restarts an engine, which can run for minutes. Holding the
// journal lock across that froze the other engine's editor and the refresh
// poller for the whole restart.
func TestSettingsApplyDoesNotBlockUnrelatedReads(t *testing.T) {
	h := newSettingsHarness(t)
	p := h.request(t)
	p.Settings.LaunchText += " --busy"
	h.entered = make(chan struct{}, 1)
	h.release = make(chan struct{})
	done := make(chan error, 1)
	go func() { _, err := h.b.applyEngineSettings(context.Background(), p, ""); done <- err }()
	<-h.entered

	read := make(chan error, 1)
	go func() {
		_, err := h.b.getEngineSettings(context.Background(), settings.Request{Engine: "lmstudio"}, "")
		read <- err
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Fatalf("read during an in-flight apply failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("engine settings read blocked behind an in-flight apply")
	}

	close(h.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSettingsResultPersistenceFailureReturnsFailedReceipt(t *testing.T) {
	h := newSettingsHarness(t)
	p := h.request(t)
	p.Settings.LaunchText += " --new"
	h.failResultSave.Store(true)
	receipt, err := h.b.applyEngineSettings(context.Background(), p, "")
	if err != nil || receipt.Phase != "failed" || h.b.engineSettings["ollama"].Snapshot.Phase != "failed" {
		t.Fatalf("result persistence failure claimed success: %+v %v", receipt, err)
	}
}

func TestSettingsJournalFailureAndInterruptedRecovery(t *testing.T) {
	h := newSettingsHarness(t)
	p := h.request(t)
	p.Settings.LaunchText += " --new"
	path, _ := h.b.engineSettingsPath()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := h.b.applyEngineSettings(context.Background(), p, ""); err == nil || h.applies.Load() != 0 {
		t.Fatal("runtime changed despite failed journal write")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	r := h.b.engineSettings["ollama"]
	r.Snapshot.Settings = p.Settings
	r.Snapshot.Revision++
	r.Snapshot.Phase = "applying"
	r.Resume = true
	r.Explicit = true
	if err := h.b.saveEngineSettingsLocked(); err != nil {
		t.Fatal(err)
	}
	// Simulate a fresh coordinator after its journal was written but component
	// writes/rebind/start had not completed. The same replay handles later cuts.
	h.b.engineSettingsLoaded = false
	if !h.b.recoverEngineSettings() {
		t.Fatal("recovery refused")
	}
	if h.applies.Load() != 1 || h.b.engineSettings["ollama"].Snapshot.Phase != "succeeded" {
		t.Fatal("accepted operation not recovered")
	}
	if !h.b.recoverEngineSettings() || h.applies.Load() != 1 {
		t.Fatal("completed operation replayed")
	}
	if _, ok := h.b.explicitEngineSettings("ollama"); !ok {
		t.Fatal("explicit startup preference lost")
	}
}

func TestSettingsPortValidationIncludesStoppedEnginesAndAliases(t *testing.T) {
	h := newSettingsHarness(t)
	p := h.request(t)
	p.Settings.ProxyPort = p.Settings.ServerPort
	preview, err := h.b.previewEngineSettings(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Errors) == 0 {
		t.Fatal("equal ports accepted")
	}
	p = h.request(t)
	p.Settings.ServerPort = engineControlPort
	preview, err = h.b.previewEngineSettings(context.Background(), p, "")
	if err != nil || len(preview.Errors) == 0 {
		t.Fatalf("reserved control port accepted: %v %v", preview, err)
	}
	p = h.request(t)
	h.otherEnginePort.Store(int32(p.Settings.ServerPort))
	preview, err = h.b.previewEngineSettings(context.Background(), p, "")
	if err != nil || len(preview.Errors) == 0 {
		t.Fatalf("stopped engine reservation ignored: %v %v", preview, err)
	}
	h.otherEnginePort.Store(0)
	h.b.ollamaHostAliasMu.Lock()
	h.b.ollamaHostAlias.Port = p.Settings.ProxyPort
	h.b.ollamaHostAliasMu.Unlock()
	preview, err = h.b.previewEngineSettings(context.Background(), p, "")
	if err != nil || len(preview.Errors) == 0 {
		t.Fatalf("proxy alias reservation ignored: %v %v", preview, err)
	}
}

// Every engine's proxy keeps a saved-port store under its own file name; a valid
// choice in any of them survives as an explicit setting and disarms automatic
// takeover for that engine alone.
func TestSettingsMigratesLegacyProxyChoiceBeforeManagedDefaults(t *testing.T) {
	h := newSettingsHarness(t)
	path, _ := h.b.engineSettingsPath()
	saved := make(map[string]int, len(engineProxyProfiles))
	for i, profile := range engineProxyProfiles {
		saved[profile.Name] = 26080 + i
		data, err := json.Marshal(map[string]int{"port": saved[profile.Name]})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), profile.PortFile), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	h.b.migrateLegacyEngineSettings()
	for _, profile := range engineProxyProfiles {
		config, ok := h.b.explicitEngineSettings(profile.Name)
		if !ok || config.ProxyPort != saved[profile.Name] {
			t.Fatalf("%s legacy choice lost: %+v %v", profile.Name, config, ok)
		}
		rt := h.b.engineProxy(profile)
		if !h.b.prepareExplicitEngineSettings(profile.Name) || int(rt.startupPort.Load()) != saved[profile.Name] || rt.managedFacade.Load() || !rt.explicitSettings.Load() {
			t.Fatalf("%s: automatic startup overrode saved proxy choice", profile.Name)
		}
	}
}

// LM Studio's standalone proxy once wrote 1235 as its own default; that value
// carries no choice and must not become an explicit setting. The rule is LM
// Studio's alone.
func TestSettingsMigrationSkipsLMStudioObsoleteDefault(t *testing.T) {
	h := newSettingsHarness(t)
	path, _ := h.b.engineSettingsPath()
	data, err := json.Marshal(map[string]int{"port": managedLMStudioBackendStart})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), lmstudioProxyProfile.PortFile), data, 0600); err != nil {
		t.Fatal(err)
	}
	h.b.migrateLegacyEngineSettings()
	if _, explicit := h.b.explicitEngineSettings(lmstudioProxyProfile.Name); explicit {
		t.Fatal("LM Studio's obsolete 1235 proxy default became an explicit setting")
	}
}

// lastResponse returns the final id-bearing response the broker wrote to its
// client codec, skipping the engine:settings-changed notifications a settings
// operation publishes on the way.
func lastResponse(t *testing.T, output *bytes.Buffer) Message {
	t.Helper()
	var last *Message
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if line == "" {
			continue
		}
		var msg Message
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			t.Fatalf("unparseable frame %q: %v", line, err)
		}
		if msg.IsResponse() {
			last = &msg
		}
	}
	if last == nil {
		t.Fatalf("no response written: %q", output.String())
	}
	return *last
}

// Every engine's <engine>-proxy:set-port is the same intercepted settings
// operation: refused without an engine manager to vouch for port ownership,
// refused for a port another configured engine holds, and otherwise moving only
// the addressed facade while recording the port as an explicit choice.
func TestSettingsPortRPCServesEveryEngineProxy(t *testing.T) {
	for _, profile := range engineProxyProfiles {
		t.Run(profile.Name, func(t *testing.T) {
			method := profile.ComponentName() + ":set-port"
			id := json.RawMessage(`1`)
			var output bytes.Buffer
			bare := &Broker{codec: NewCodec(readWriter{Reader: bytes.NewReader(nil), Writer: &output}), clusterDir: filepath.Join(t.TempDir(), "cluster")}
			bare.handleSettingsPortRPC(&Message{ID: &id, Method: method, Params: settingsJSON(map[string]int{"port": 8082})}, profile.Name)
			if response := lastResponse(t, &output); response.Error == nil {
				t.Fatalf("port moved without an engine manager to vouch for ownership: %s", output.String())
			}

			h := newSettingsHarness(t)
			h.b.codec = NewCodec(readWriter{Reader: bytes.NewReader(nil), Writer: &output})
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := ln.Addr().(*net.TCPAddr).Port
			_ = ln.Close()
			request := &Message{ID: &id, Method: method, Params: settingsJSON(map[string]int{"port": port})}

			output.Reset()
			h.otherEnginePort.Store(int32(port))
			h.b.handleSettingsPortRPC(request, profile.Name)
			if response := lastResponse(t, &output); response.Error == nil {
				t.Fatalf("took a port configured for another engine: %s", output.String())
			}

			h.otherEnginePort.Store(0)
			output.Reset()
			p := h.b.settingsProxy(profile.Name)
			if p == nil {
				t.Fatal("settings proxy unavailable")
			}
			before := make(map[string]int, len(engineProxyProfiles))
			for _, other := range engineProxyProfiles {
				_, before[other.Name] = p.Status(other.Name)
			}
			h.b.handleSettingsPortRPC(request, profile.Name)
			response := lastResponse(t, &output)
			if response.Error != nil {
				t.Fatalf("port move refused: %s", response.Error.Message)
			}
			var moved struct {
				Port int `json:"port"`
			}
			if json.Unmarshal(response.Result, &moved) != nil || moved.Port != port {
				t.Fatalf("response %s, want port %d", response.Result, port)
			}
			for _, other := range engineProxyProfiles {
				want := before[other.Name]
				if other.Name == profile.Name {
					want = port
				}
				if ready, got := p.Status(other.Name); !ready || got != want {
					t.Fatalf("%s ready=%v port=%d, want %d", other.Name, ready, got, want)
				}
			}
			if s := h.b.engineSettings[profile.Name].Snapshot; s.Settings.ProxyPort != port || s.Phase != "succeeded" {
				t.Fatalf("journal did not record the move: %+v", s)
			}
			if rt := h.b.engineProxy(profile); !rt.explicitSettings.Load() || rt.managedFacade.Load() {
				t.Fatal("a port set from the terminal was not recorded as an explicit choice")
			}
		})
	}
}
