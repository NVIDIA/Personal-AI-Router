// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	settings "nvpair-shared/enginesettings"
)

// Convert pending/failed full-command settings before replay, preserving the
// accepted arguments and proxy port. Component launch_args/launch_env need no
// migration because they have always stored only literal user overrides.
func (b *Broker) migrateSettingsArgumentsLocked(ctx context.Context, engine string, record *engineSettingsRecord) error {
	if record == nil || record.Snapshot.Format != "pair-launch-v1" {
		return nil
	}
	var preview settings.Preview
	if err := b.settingsWorkerCall(ctx, "engine:preview-launch", settings.Request{
		Engine: engine, Settings: record.Snapshot.Settings, Format: "pair-launch-v1",
	}, &preview); err != nil {
		return err
	}
	if len(preview.Errors) != 0 || preview.Conflict != nil {
		return fmt.Errorf("saved full-command settings could not be converted; existing settings were preserved")
	}
	previous := record.Snapshot
	record.Snapshot.Settings = preview.Settings
	record.Snapshot.Format = "pair-arguments-v1"
	record.Snapshot.Revision++
	if previous.AppliedRevision == previous.Revision {
		record.Snapshot.AppliedRevision = record.Snapshot.Revision
	}
	if err := b.saveEngineSettingsLocked(); err != nil {
		record.Snapshot = previous
		return err
	}
	return nil
}

func (b *Broker) explicitEngineSettings(engine string) (settings.Config, bool) {
	b.engineConfigMu.Lock()
	defer b.engineConfigMu.Unlock()
	return b.explicitEngineSettingsLocked(engine)
}

func (b *Broker) explicitEngineSettingsLocked(engine string) (settings.Config, bool) {
	if err := b.loadEngineSettingsLocked(); err != nil {
		return settings.Config{}, false
	}
	if record := b.engineSettings[engine]; record != nil && record.Explicit {
		return record.Snapshot.Settings, true
	}
	return settings.Config{}, false
}

// prepareExplicitEngineSettings restores a saved explicit choice onto an
// engine's proxy runtime before its facade is prepared, and reports whether
// automatic facade planning must stand down for it. It is the startup half of
// the contract rebindSettingsProxy keeps at run time: an explicit choice turns
// managed takeover off and fixes both the backend and the startup port.
func (b *Broker) prepareExplicitEngineSettings(engine string) bool {
	profile, ok := engineProxyProfileFor(engine)
	if !ok {
		return false
	}
	b.engineConfigMu.Lock()
	loadErr := b.loadEngineSettingsLocked()
	b.engineConfigMu.Unlock()
	if loadErr != nil {
		return true
	} // preserve component stores; recovery reports the journal error
	config, ok := b.explicitEngineSettings(engine)
	if !ok {
		return false
	}
	rt := b.engineProxy(profile)
	rt.explicitSettings.Store(true)
	rt.managedFacade.Store(false)
	rt.backendPort.Store(int32(config.ServerPort))
	rt.startupPort.Store(int32(config.ProxyPort))
	if engine == ollamaProxyProfile.Name {
		// Only Ollama carries a pending backend move and an inherited
		// OLLAMA_HOST alias reservation that have to follow the explicit choice.
		b.managedOllamaBackend.Store(0)
		b.syncCurrentEngineOllamaHostAliasReservation()
	}
	return true
}

// facadeBindFailure decodes a facade's bind-failed error notification and
// reports whether automatic recovery may act on it. A facade whose port is an
// explicit settings choice is never moved by that path: the choice stands, and
// the failure reaches the user through the settings operation that made it.
// This runs on the proxy reader goroutine, so it reads the atomic and takes no
// lock — a settings operation may hold engineConfigMu while waiting on that
// very reader.
func (b *Broker) facadeBindFailure(profile engineProxyProfile, method string, params json.RawMessage) (port int, recover bool) {
	if method != "error" {
		return 0, false
	}
	var ep struct {
		Code string `json:"code"`
		Port int    `json:"port"`
	}
	if json.Unmarshal(params, &ep) != nil || ep.Code != "bind-failed" {
		return 0, false
	}
	return ep.Port, !b.engineProxy(profile).explicitSettings.Load()
}

// settingsGovernFacadeLocked reports whether the engine-settings journal, not
// automatic ownership planning, decides where this engine's facade listens:
// when the user has made an explicit choice, and when the journal cannot be
// read, which suppresses every automatic component rewrite. Readiness
// reconciliation stands down in both cases and opens the engine's gate instead.
// Caller holds engineConfigMu.
func (b *Broker) settingsGovernFacadeLocked(profile engineProxyProfile) bool {
	if b.loadEngineSettingsLocked() != nil {
		return true
	}
	_, explicit := b.explicitEngineSettingsLocked(profile.Name)
	return explicit
}

// obsoleteLegacyProxyDefault reports a saved proxy port that an older proxy
// wrote as its own default rather than as a record of a user's choice. Only LM
// Studio has one: its standalone proxy defaulted to 1235, the port its managed
// backend is now relocated onto, and its store migration already discards it.
// Ollama's saved port has always been preserved, and llama.cpp's store is newer
// than the journal, so neither has a default to exclude.
func obsoleteLegacyProxyDefault(profile engineProxyProfile, port int) bool {
	return profile.Name == lmstudioProxyProfile.Name && port == managedLMStudioBackendStart
}

// Legacy proxy stores did not distinguish user choices from automatic moves.
// Preserve a valid non-colliding saved choice as explicit on first upgrade.
// Every engine's proxy keeps such a store, named by its profile's PortFile.
func (b *Broker) migrateLegacyEngineSettings() {
	b.engineConfigMu.Lock()
	defer b.engineConfigMu.Unlock()
	if b.loadEngineSettingsLocked() != nil || b.getEngineMgr() == nil {
		return
	}
	journal, err := b.engineSettingsPath()
	if err != nil {
		return
	}
	for _, profile := range engineProxyProfiles {
		engine := profile.Name
		if b.engineSettings[engine] != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(filepath.Dir(journal), profile.PortFile))
		if err != nil {
			continue
		}
		var saved struct {
			Port int `json:"port"`
		}
		if json.Unmarshal(data, &saved) != nil || saved.Port < 1 || saved.Port > 65535 || obsoleteLegacyProxyDefault(profile, saved.Port) {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = b.settingsWorkerCall(ctx, "engine:status", map[string]string{"engine": engine}, nil)
		var launch settings.LaunchState
		err = b.settingsWorkerCall(ctx, "engine:get-launch", map[string]string{"engine": engine}, &launch)
		if err != nil || !launch.Editable || launch.ServerPort == saved.Port {
			cancel()
			continue
		}
		config := settings.Config{ServerPort: launch.ServerPort, ProxyPort: saved.Port, LaunchText: launch.LaunchText}
		current := settings.Snapshot{Running: launch.Running, EffectiveServerPort: launch.EffectivePort}
		if proxy := b.settingsProxy(engine); proxy != nil {
			_, current.EffectiveProxyPort = proxy.Status(engine)
		}
		err = b.validateSettingsPortsLocked(ctx, engine, config, current)
		cancel()
		if err != nil {
			continue
		}
		b.engineSettings[engine] = &engineSettingsRecord{Explicit: true, Receipts: map[string]settingsReceipt{}, Snapshot: settings.Snapshot{NodeID: b.nodeID, Engine: engine, Revision: 1, AppliedRevision: 1, Epoch: b.engineSettingsEpoch, Phase: "idle", Settings: settings.Config{ServerPort: launch.ServerPort, ProxyPort: saved.Port, LaunchText: launch.LaunchText}}}
		if err := b.saveEngineSettingsLocked(); err != nil {
			delete(b.engineSettings, engine)
			b.engineSettingsError = err
			return
		}
	}
}

// Replay accepted operations before automatic engine startup. Component stores
// can be at any phase after a crash; configure revalidates and converges them
// under the same engine lock used by a live Apply, then records its outcome.
func (b *Broker) recoverEngineSettings() bool {
	// Same order a live Apply uses: the operation lock, then the journal lock
	// that runSettingsOperationLocked releases around the engine restart.
	b.settingsApplyMu.Lock()
	defer b.settingsApplyMu.Unlock()
	b.engineConfigMu.Lock()
	defer b.engineConfigMu.Unlock()
	if err := b.loadEngineSettingsLocked(); err != nil {
		slog.Error("engine settings recovery blocked", "err", err)
		return false
	}
	for engine, record := range b.engineSettings {
		if record.Snapshot.Phase != "applying" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), settings.OperationBudget)
		_ = b.runSettingsOperationLocked(ctx, engine, record)
		cancel()
	}
	return true
}

// Legacy lifecycle/port RPCs share the node lock. Reconcile their component
// changes into the same revision stream before allowing another Apply.
func (b *Broker) reconcileLegacySettingsLocked() {
	if !b.engineSettingsLoaded || b.engineSettingsError != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	changed := false
	for engine, record := range b.engineSettings {
		before := record.Snapshot
		after, err := b.settingsSnapshotLocked(ctx, engine)
		if err == nil && before != after {
			changed = true
		}
	}
	if changed {
		b.publishSettingsLocked()
	}
}
