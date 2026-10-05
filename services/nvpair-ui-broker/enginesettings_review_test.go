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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	settings "nvpair-shared/enginesettings"
)

func TestSettingsRebindAddressesOnlyRequestedFacade(t *testing.T) {
	for _, profile := range engineProxyProfiles {
		t.Run(profile.Name, func(t *testing.T) {
			h := newSettingsHarness(t)
			p := h.b.getProxy()
			before := make(map[string]int, len(engineProxyProfiles))
			for _, candidate := range engineProxyProfiles {
				_, before[candidate.Name] = p.Status(candidate.Name)
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			port := ln.Addr().(*net.TCPAddr).Port
			_ = ln.Close()
			require.NoError(t, h.b.rebindSettingsProxy(profile.Name, port))
			before[profile.Name] = port
			for engine, want := range before {
				ready, got := p.Status(engine)
				require.True(t, ready, " (%v, %v, %v, %v)", engine, ready, got, want)
				require.True(t, got == want, " (%v, %v, %v, %v)", engine, ready, got, want)
			}
		})
	}
}

func TestExplicitSettingsBindFailurePreservesChosenPort(t *testing.T) {
	for _, profile := range engineProxyProfiles {
		t.Run(profile.Name, func(t *testing.T) {
			const requested = 25000
			b := &Broker{codec: NewCodec(&bytes.Buffer{}), engineSettingsLoaded: true,
				engineSettings: map[string]*engineSettingsRecord{profile.Name: {
					Explicit: true, Snapshot: settings.Snapshot{Settings: settings.Config{ServerPort: 24999, ProxyPort: requested}},
				}},
			}
			require.True(t, b.prepareExplicitEngineSettings(profile.Name), "explicit settings were not restored")
			failure := settingsJSON(map[string]any{"code": "bind-failed", "port": requested})
			switch profile.Name {
			case "ollama":
				b.forwardProxyNotification("error", failure)
			case "lmstudio":
				b.forwardLMStudioProxyNotification("error", failure)
			default:
				b.forwardDefaultEngineProxyNotification(profile, profile.addressed("error"), failure)
			}
			{
				got := b.engineProxy(profile).startupPort.Load()
				require.True(t, got == requested, "bind notification changed chosen port to (%v)", got)
			}
			client, server := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
			p := &proxyProcess{peer: NewPeer(NewCodec(client))}
			go p.peer.Serve(nil, nil)
			attempts := make(chan enableFacadeRequest, 2)
			serveFacadeEnable(t, server, map[int]bool{requested: true}, attempts)
			err := b.enableProxyFacadeWithFallback(context.Background(), p, enableFacadeRequest{Engine: profile.Name, Port: requested}, func(int) int {
				assert.Fail(t, "test expectation failed", "explicit port must not fall back")
				return requested + 1
			})
			require.Error(t, err, "bind failure was hidden")
			require.Len(t, attempts, 1)
		})
	}
}

func TestSettingsReservesStoppedProxySavedPort(t *testing.T) {
	test := func(name string, serverPort bool) {
		t.Run(name, func(t *testing.T) {
			h := newSettingsHarness(t)
			request := h.request(t)
			_, reserved := h.b.getLMStudioProxy().Status("lmstudio")
			h.b.setLMStudioProxy(nil)
			h.b.engineSettings["lmstudio"] = &engineSettingsRecord{
				Explicit: true,
				Snapshot: settings.Snapshot{Settings: settings.Config{ProxyPort: reserved}},
			}
			if serverPort {
				request.Settings.ServerPort = reserved
			} else {
				request.Settings.ProxyPort = reserved
			}
			preview, err := h.b.previewEngineSettings(context.Background(), request, "")
			require.NoError(t, err)
			require.NotEqual(t, 0, len(preview.Errors), "accepted stopped proxy's saved port (%v)", preview)
		})
	}
	test("server port cannot reuse saved proxy port", true)
	test("proxy port cannot reuse saved proxy port", false)
}

func TestSettingsMigrationRejectsReservedPorts(t *testing.T) {
	test := func(name string, reserve func(*settingsHarness, settings.Request) int) {
		t.Run(name, func(t *testing.T) {
			h := newSettingsHarness(t)
			request := h.request(t)
			delete(h.b.engineSettings, "ollama")
			port := reserve(h, request)
			path, err := h.b.engineSettingsPath()
			require.NoError(t, err)
			data, err := json.Marshal(map[string]int{"port": port})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), "proxy-port.json"), data, 0600))
			h.b.migrateLegacyEngineSettings()
			{
				_, explicit := h.b.explicitEngineSettings("ollama")
				require.False(t, explicit, "reserved legacy port became an explicit setting")
			}
		})
	}
	test("PAIR control port", func(h *settingsHarness, request settings.Request) int {
		return engineControlPort
	})
	test("inherited Ollama host alias", func(h *settingsHarness, request settings.Request) int {
		h.b.ollamaHostAliasMu.Lock()
		h.b.ollamaHostAlias.Port = request.Settings.ProxyPort
		h.b.ollamaHostAliasMu.Unlock()
		return request.Settings.ProxyPort
	})
	test("stopped proxy saved port", func(h *settingsHarness, request settings.Request) int {
		_, port := h.b.getLMStudioProxy().Status("lmstudio")
		h.b.setLMStudioProxy(nil)
		h.b.engineSettings["lmstudio"] = &engineSettingsRecord{
			Explicit: true,
			Snapshot: settings.Snapshot{Settings: settings.Config{ProxyPort: port}},
		}
		return port
	})
}

func TestEnabledEngineRestorationSurvivesInvalidSettingsJournal(t *testing.T) {
	b := &Broker{clusterDir: filepath.Join(t.TempDir(), "cluster")}
	path, err := b.engineSettingsPath()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, []byte("{invalid"), 0600))
	worker, codec := newTestRPCWorkerPipe(t)
	restored := make(chan string, 1)
	go func() {
		msg, err := codec.Read()
		if err != nil {
			assert.NoError(t, err)
			return
		}
		restored <- msg.Method
	}()
	b.restoreEnabledEngines(worker)
	select {
	case method := <-restored:
		require.True(t, method == restoreEnabledEnginesMethod, "method (%v)", method)
	case <-time.After(time.Second):
		require.FailNow(t, "test expectation failed", "invalid journal suppressed enabled-engine restoration")
	}
	require.NotNil(t, b.engineSettingsError, "invalid journal was not reported")
}

func TestSettingsFullCommandJournalMigratesBeforeRecovery(t *testing.T) {
	h := newSettingsHarness(t)
	request := h.request(t)
	h.b.engineConfigMu.Lock()
	record := h.b.engineSettings["ollama"]
	record.Snapshot.Format = "pair-launch-v1"
	record.Snapshot.Settings.LaunchText = "managed serve --fixture-option --future-option"
	record.Snapshot.Phase = "applying"
	record.Resume = true
	oldProxyPort := record.Snapshot.Settings.ProxyPort
	require.NoError(t, h.b.saveEngineSettingsLocked())
	h.b.engineConfigMu.Unlock()
	require.True(t, h.b.recoverEngineSettings(), "recovery failed")
	snapshot, err := h.b.getEngineSettings(context.Background(), settings.Request{Engine: "ollama"}, "")
	require.NoError(t, err)
	require.Equal(t, "pair-arguments-v1", snapshot.Format, "migration lost accepted configuration (%v)", snapshot)
	require.Equal(t, "succeeded", snapshot.Phase, "migration lost accepted configuration (%v)", snapshot)
	require.Equal(t, "--fixture-option --future-option", snapshot.Settings.LaunchText, "migration lost accepted configuration (%v)", snapshot)
	require.True(t, snapshot.Settings.ProxyPort == oldProxyPort, "migration lost accepted configuration (%v)", snapshot)
	require.True(t, snapshot.Settings.ServerPort == request.Settings.ServerPort, "migration lost accepted configuration (%v)", snapshot)
	require.Equal(t, int32(1), h.applies.Load(), "pending operation was not applied exactly once")
}
