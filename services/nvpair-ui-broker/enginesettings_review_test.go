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

	settings "nvpair-shared/enginesettings"
)

func TestSettingsRebindAddressesOnlyRequestedFacade(t *testing.T) {
	for _, profile := range engineProxyProfiles {
		t.Run(profile.Name, func(t *testing.T) {
			h := newSettingsHarness(t)
			p := h.b.getProxy()
			_, ollamaBefore := p.Status("ollama")
			_, lmstudioBefore := p.Status("lmstudio")
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := ln.Addr().(*net.TCPAddr).Port
			_ = ln.Close()
			if err := h.b.rebindSettingsProxy(profile.Name, port); err != nil {
				t.Fatal(err)
			}
			wantOllama, wantLMStudio := ollamaBefore, lmstudioBefore
			if profile.Name == "ollama" {
				wantOllama = port
			} else {
				wantLMStudio = port
			}
			for engine, want := range map[string]int{"ollama": wantOllama, "lmstudio": wantLMStudio} {
				ready, got := p.Status(engine)
				if !ready || got != want {
					t.Fatalf("%s ready=%v port=%d, want %d", engine, ready, got, want)
				}
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
			if !b.prepareExplicitEngineSettings(profile.Name) {
				t.Fatal("explicit settings were not restored")
			}
			failure := settingsJSON(map[string]any{"code": "bind-failed", "port": requested})
			if profile.Name == "ollama" {
				b.forwardProxyNotification("error", failure)
			} else {
				b.forwardLMStudioProxyNotification("error", failure)
			}
			if got := b.engineProxy(profile).startupPort.Load(); got != requested {
				t.Fatalf("bind notification changed chosen port to %d", got)
			}
			client, server := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
			p := &proxyProcess{peer: NewPeer(NewCodec(client))}
			go p.peer.Serve(nil, nil)
			attempts := make(chan enableFacadeRequest, 2)
			serveFacadeEnable(t, server, map[int]bool{requested: true}, attempts)
			err := b.enableProxyFacadeWithFallback(context.Background(), p, enableFacadeRequest{Engine: profile.Name, Port: requested}, func(int) int {
				t.Error("explicit port must not fall back")
				return requested + 1
			})
			if err == nil {
				t.Fatal("bind failure was hidden")
			}
			if len(attempts) != 1 {
				t.Fatalf("attempts=%d, want only the chosen port", len(attempts))
			}
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
			if err != nil {
				t.Fatal(err)
			}
			if len(preview.Errors) == 0 {
				t.Fatalf("accepted stopped proxy's saved port: %+v", preview)
			}
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
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(map[string]int{"port": port})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(filepath.Dir(path), "proxy-port.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			h.b.migrateLegacyEngineSettings()
			if _, explicit := h.b.explicitEngineSettings("ollama"); explicit {
				t.Fatal("reserved legacy port became an explicit setting")
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
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	worker, codec := newTestRPCWorkerPipe(t)
	restored := make(chan string, 1)
	go func() {
		msg, err := codec.Read()
		if err != nil {
			t.Error(err)
			return
		}
		restored <- msg.Method
	}()
	b.restoreEnabledEngines(worker)
	select {
	case method := <-restored:
		if method != restoreEnabledEnginesMethod {
			t.Fatalf("method=%q", method)
		}
	case <-time.After(time.Second):
		t.Fatal("invalid journal suppressed enabled-engine restoration")
	}
	if b.engineSettingsError == nil {
		t.Fatal("invalid journal was not reported")
	}
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
	if err := h.b.saveEngineSettingsLocked(); err != nil {
		t.Fatal(err)
	}
	h.b.engineConfigMu.Unlock()
	if !h.b.recoverEngineSettings() {
		t.Fatal("recovery failed")
	}
	snapshot, err := h.b.getEngineSettings(context.Background(), settings.Request{Engine: "ollama"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Format != "pair-arguments-v1" || snapshot.Phase != "succeeded" || snapshot.Settings.LaunchText != "--fixture-option --future-option" || snapshot.Settings.ProxyPort != oldProxyPort || snapshot.Settings.ServerPort != request.Settings.ServerPort {
		t.Fatalf("migration lost accepted configuration: %+v", snapshot)
	}
	if h.applies.Load() != 1 {
		t.Fatal("pending operation was not applied exactly once")
	}
}
