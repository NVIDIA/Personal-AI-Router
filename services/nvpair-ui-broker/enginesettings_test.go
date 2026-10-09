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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/engines"
	settings "nvpair-shared/enginesettings"
	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

type settingsHarness struct {
	b                   *Broker
	applies             atomic.Int32
	proxyRebinds        atomic.Int32
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

func newSettingsHarness(t *testing.T) *settingsHarness {
	t.Helper()
	return newSettingsHarnessForEngine(t, "ollama")
}

func newSettingsHarnessForEngine(t *testing.T, engine string) *settingsHarness {
	t.Helper()
	ports := make([]int, 4)
	listeners := []net.Listener{}
	for i := range ports {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
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
	proxy := &proxyProcess{peer: proxyWorker.peer, facadeState: map[string]proxyFacadeState{
		"ollama":   {ready: true, port: ports[1]},
		"lmstudio": {ready: true, port: ports[2]},
		"llamacpp": {ready: true, port: ports[3]},
	}}
	for _, profile := range engineProxyProfiles {
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
			engine, method := engines.SplitAddressedMethod(msg.Method)
			if engine == "" || method != "set-port" {
				_ = proxyCodec.Respond(msg.ID, map[string]bool{"ok": true})
				continue
			}
			var p struct {
				Port int `json:"port"`
			}
			if !assert.NoError(t, json.Unmarshal(msg.Params, &p), "decode proxy port request") {
				return
			}
			h.proxyRebinds.Add(1)
			proxy.readyMu.Lock()
			proxy.facadeState[engine] = proxyFacadeState{ready: true, port: p.Port}
			proxy.readyMu.Unlock()
			_ = proxyCodec.Respond(msg.ID, map[string]int{"port": p.Port})
		}
	}()
	launch := settings.LaunchState{Engine: engine, ServerPort: ports[0], EffectivePort: ports[0], LaunchText: "--fixture-option", Running: true, Editable: true, Format: "pair-arguments-v1"}
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
				h.launchMu.Lock()
				current := launch
				h.launchMu.Unlock()
				_ = codec.Respond(msg.ID, current)
			case "engine:configured-ports":
				_ = codec.Respond(msg.ID, map[string]any{"engines": []any{map[string]any{"engine": "lmstudio", "port": h.otherEnginePort.Load()}}})
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
					if h.entered != nil {
						h.entered <- struct{}{}
						<-h.release
					}
					if h.failBeforeStop.Load() {
						if h.loseProxyOnStop.Load() {
							proxy.readyMu.Lock()
							state := proxy.facadeState[engine]
							state.ready = false
							proxy.facadeState[engine] = state
							proxy.readyMu.Unlock()
						}
						_ = codec.RespondError(msg.ID, -32000, "stop failure")
						return
					}
					var p settings.Configure
					_ = json.Unmarshal(msg.Params, &p)
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
					if h.failResultSave.Load() {
						journal, _ := h.b.engineSettingsPath()
						assert.NoError(t, os.Remove(journal))
						assert.NoError(t, os.Mkdir(journal, 0700))
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
	require.NoError(t, err)
	return settings.Request{Engine: "ollama", ExpectedRevision: s.Revision, RequestID: settingsID(), Settings: s.Settings}
}

func TestSettingsCoordinatorRevisionDedupNoopAndFailure(t *testing.T) {
	h := newSettingsHarness(t)
	p := h.request(t)
	receipt, err := h.b.applyEngineSettings(context.Background(), p, "")
	require.NoError(t, err)
	require.Equal(t, int32(0), h.applies.Load(), "no-op mutated runtime")
	require.Equal(t, "succeeded", receipt.Phase, "no-op mutated runtime")
	p = h.request(t)
	p.Settings.LaunchText += " --parallel 2"
	receipt, err = h.b.applyEngineSettings(context.Background(), p, "")
	require.NoError(t, err)
	require.Equal(t, p.ExpectedRevision+1, receipt.Revision, "receipt (%v)", receipt)
	require.Equal(t, "succeeded", receipt.Phase, "receipt (%v)", receipt)
	require.Equal(t, int32(1), h.applies.Load(), "receipt (%v)", receipt)
	_, err = h.b.applyEngineSettings(context.Background(), p, "")
	require.NoError(t, err, "duplicate restarted")
	require.Equal(t, int32(1), h.applies.Load(), "duplicate restarted")
	reused := p
	reused.Settings.LaunchText += " --different"
	_, err = h.b.applyEngineSettings(context.Background(), reused, "")
	require.Error(t, err, "reused identifier accepted")
	p.RequestID = settingsID()
	_, err = h.b.applyEngineSettings(context.Background(), p, "")
	require.Error(t, err, "stale revision accepted")
	p = h.request(t)
	p.Settings.LaunchText += " --invalid"
	h.fail.Store(true)
	receipt, err = h.b.applyEngineSettings(context.Background(), p, "")
	require.NoError(t, err, "failure receipt (%v)", receipt)
	require.Equal(t, "failed", receipt.Phase, "failure receipt (%v)", receipt)
	s := h.b.engineSettings["ollama"].Snapshot
	require.Equal(t, p.Settings, s.Settings, "untruthful failed snapshot (%v)", s)
	require.False(t, s.Running, "untruthful failed snapshot (%v)", s)
	require.Less(t, s.AppliedRevision, s.Revision, "untruthful failed snapshot (%v)", s)
	require.NotEqual(t, "", s.Error, "untruthful failed snapshot (%v)", s)
	h.fail.Store(false)
	p = h.request(t)
	_, err = h.b.applyEngineSettings(context.Background(), p, "")
	require.NoError(t, err)
	s = h.b.engineSettings["ollama"].Snapshot
	require.True(t, s.Running, "retry lost resume intent (%v)", s)
	require.Equal(t, "succeeded", s.Phase, "retry lost resume intent (%v)", s)
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
			require.NoError(t, err, "expected failed receipt (%v)", receipt)
			require.Equal(t, "failed", receipt.Phase, "expected failed receipt (%v)", receipt)
			registered := h.b.regCache.Snapshot()
			if tc.advertised {
				require.Len(t, registered, 1, "running engine lost registration")
				require.Equal(t, noderec.ServiceOllama, registered[0].Service, "running engine lost registration (%v)", registered)
				require.Equal(t, oldPort, registered[0].Port, "running engine lost registration (%v)", registered)
			} else {
				require.Empty(t, registered, "unavailable service advertised")
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
	require.NoError(t, <-done)
	require.Error(t, <-canceled, "canceled queued mutation accepted")
	require.Equal(t, int32(1), h.applies.Load(), "concurrent request mutated runtime")
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
		require.NoError(t, err, "read during an in-flight apply failed")
	case <-time.After(10 * time.Second):
		require.FailNow(t, "engine settings read blocked behind an in-flight apply")
	}

	close(h.release)
	require.NoError(t, <-done)
}

func TestSettingsResultPersistenceFailureReturnsFailedReceipt(t *testing.T) {
	h := newSettingsHarness(t)
	p := h.request(t)
	p.Settings.LaunchText += " --new"
	h.failResultSave.Store(true)
	receipt, err := h.b.applyEngineSettings(context.Background(), p, "")
	require.NoError(t, err, "result persistence failure claimed success (%v)", receipt)
	require.Equal(t, "failed", receipt.Phase, "result persistence failure claimed success (%v)", receipt)
	require.Equal(t, "failed", h.b.engineSettings["ollama"].Snapshot.Phase, "result persistence failure claimed success (%v)", receipt)
}

func TestSettingsJournalFailureAndInterruptedRecovery(t *testing.T) {
	h := newSettingsHarness(t)
	p := h.request(t)
	p.Settings.LaunchText += " --new"
	path, _ := h.b.engineSettingsPath()
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0700))
	_, err := h.b.applyEngineSettings(context.Background(), p, "")
	require.Error(t, err, "runtime changed despite failed journal write")
	require.Equal(t, int32(0), h.applies.Load(), "runtime changed despite failed journal write")
	require.NoError(t, os.Remove(path))
	r := h.b.engineSettings["ollama"]
	r.Snapshot.Settings = p.Settings
	r.Snapshot.Revision++
	r.Snapshot.Phase = "applying"
	r.Resume = true
	r.Explicit = true
	require.NoError(t, h.b.saveEngineSettingsLocked())
	// Simulate a fresh coordinator after its journal was written but component
	// writes/rebind/start had not completed. The same replay handles later cuts.
	h.b.engineSettingsLoaded = false
	require.True(t, h.b.recoverEngineSettings(), "recovery refused")
	require.Equal(t, int32(1), h.applies.Load(), "accepted operation not recovered")
	require.Equal(t, "succeeded", h.b.engineSettings["ollama"].Snapshot.Phase, "accepted operation not recovered")
	require.True(t, h.b.recoverEngineSettings(), "completed operation replayed")
	require.Equal(t, int32(1), h.applies.Load(), "completed operation replayed")
	_, ok := h.b.explicitEngineSettings("ollama")
	require.True(t, ok, "explicit startup preference lost")
}

func TestSettingsPortValidationIncludesStoppedEnginesAndAliases(t *testing.T) {
	h := newSettingsHarness(t)
	p := h.request(t)
	p.Settings.ProxyPort = p.Settings.ServerPort
	preview, err := h.b.previewEngineSettings(context.Background(), p, "")
	require.NoError(t, err)
	require.NotEmpty(t, preview.Errors, "equal ports accepted")
	p = h.request(t)
	p.Settings.ServerPort = engineControlPort
	preview, err = h.b.previewEngineSettings(context.Background(), p, "")
	require.NoError(t, err, "reserved control port accepted (%v)", preview)
	require.NotEmpty(t, preview.Errors, "reserved control port accepted (%v)", preview)
	p = h.request(t)
	h.otherEnginePort.Store(int32(p.Settings.ServerPort))
	preview, err = h.b.previewEngineSettings(context.Background(), p, "")
	require.NoError(t, err, "stopped engine reservation ignored (%v)", preview)
	require.NotEmpty(t, preview.Errors, "stopped engine reservation ignored (%v)", preview)
	h.otherEnginePort.Store(0)
	h.b.ollamaHostAliasMu.Lock()
	h.b.ollamaHostAlias.Port = p.Settings.ProxyPort
	h.b.ollamaHostAliasMu.Unlock()
	preview, err = h.b.previewEngineSettings(context.Background(), p, "")
	require.NoError(t, err, "proxy alias reservation ignored (%v)", preview)
	require.NotEmpty(t, preview.Errors, "proxy alias reservation ignored (%v)", preview)
}

func TestSettingsMigratesLegacyProxyChoiceBeforeManagedDefaults(t *testing.T) {
	h := newSettingsHarness(t)
	path, _ := h.b.engineSettingsPath()
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "proxy-port.json"), []byte(`{"port":26080}`), 0600))
	h.b.migrateLegacyEngineSettings()
	config, ok := h.b.explicitEngineSettings("ollama")
	require.True(t, ok, "legacy choice lost (%v)", config)
	require.Equal(t, 26080, config.ProxyPort, "legacy choice lost (%v)", config)
	require.True(t, h.b.prepareExplicitEngineSettings("ollama"), "automatic startup overrode saved proxy choice")
	require.Equal(t, int32(26080), h.b.ollamaState().startupPort.Load(), "automatic startup overrode saved proxy choice")
	require.False(t, h.b.ollamaState().managedFacade.Load(), "automatic startup overrode saved proxy choice")
}
