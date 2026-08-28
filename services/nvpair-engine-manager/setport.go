// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func canMoveAdoptedEngine(rt Runtime) bool {
	return rt.modeOrDefault() == "command" && rt.Stop != nil && len(rt.Stop.Cmd) > 0
}

// SetPort persists an engine's chosen server port as a manifest override and
// applies it: the running session is bounced onto the new port (if running)
// and the cached port is updated so a later start/adopt uses it. Persistence
// is via the manifest (the single source of truth) — see persistPort — so
// the port survives a restart with no separate override store. Held under the
// engine's op lock so it can't interleave with another lifecycle op.
//
// A running, adopted process-mode engine is refused. An identified command-mode
// engine may be moved only when its manifest provides an official stop command.
func (e *Executor) SetPort(ctx context.Context, engine string, port int) (EngineStatus, error) {
	if port < 1 || port > 65535 {
		return EngineStatus{}, fmt.Errorf("port must be between 1 and 65535")
	}
	if err := e.reservedPortError(port); err != nil {
		return EngineStatus{}, err
	}
	st, err := e.state(engine)
	if err != nil {
		return EngineStatus{}, err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()

	st.mu.Lock()
	wasRunning := st.running
	adopted := st.adopted
	oldPort := st.port
	st.mu.Unlock()

	// Adopted process-mode engines and command-mode engines without an official
	// stop command remain externally managed. Refuse rather than killing an
	// unknown process or spawning a duplicate listener on the new port.
	if wasRunning && adopted && !canMoveAdoptedEngine(st.plat.Runtime) {
		return EngineStatus{}, fmt.Errorf("cannot change %s's port: it is running under external management (NVPAIR adopted it rather than starting it), so NVPAIR cannot move it — stop it in its own app first, then set the port", engine)
	}

	// Stop on the old port before switching, so a port-dependent stop (e.g.
	// a command-mode engine) targets the address it actually started on.
	if wasRunning {
		if err := e.doStop(st, engine); err != nil {
			return EngineStatus{}, err
		}
	}

	if err := e.persistPort(engine, port); err != nil {
		if !wasRunning {
			return EngineStatus{}, err
		}
		st.mu.Lock()
		st.port = oldPort
		if st.plat != nil {
			st.plat.Runtime.Port = oldPort
		}
		st.mu.Unlock()
		restartErr := e.doStart(ctx, st, engine, startOpts{})
		return EngineStatus{}, errors.Join(err, restartErr)
	}

	st.mu.Lock()
	st.port = port
	if st.plat != nil {
		st.plat.Runtime.Port = port
	}
	st.mu.Unlock()

	if wasRunning {
		// doStart re-reads st.port and emits engine:state-changed itself.
		if err := e.doStart(ctx, st, engine, startOpts{}); err != nil {
			return EngineStatus{}, err
		}
	} else {
		// No process to bounce, but the port changed — let subscribers see it.
		e.emitState(engine)
	}
	return e.snapshot(engine, st), nil
}

// persistPort writes (or removes) the per-engine manifest override that pins
// runtime.port so the chosen port survives a restart. For a bundled engine it
// writes only the {engine, runtime:{port}} delta (which deep-merges onto the
// bundled manifest at load), and removes the override entirely when the port
// is back at the bundled default — keeping the override set minimal. For a
// non-bundled engine (a full manifest that lives only in the override dir) it
// merges the port into the existing file rather than clobbering it. Atomic
// (tmp + rename).
func (e *Executor) persistPort(engine string, port int) error {
	if e.overrideDir == "" {
		return fmt.Errorf("no config directory available to persist the port")
	}
	if err := os.MkdirAll(e.overrideDir, 0o755); err != nil {
		return fmt.Errorf("create override dir: %w", err)
	}
	path := filepath.Join(e.overrideDir, engine+".json")
	delta := map[string]any{"engine": engine, "runtime": map[string]any{"port": port}}

	if def, ok := e.reg.bundledDefaultPort(engine); ok {
		// Bundled engine: back to default ⇒ drop the override; else persist
		// just the delta so bundled upgrades to everything else still apply.
		if def == port {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove override: %w", err)
			}
			return nil
		}
		return writeJSONAtomic(path, delta)
	}

	// Non-bundled engine: the full manifest lives only here, so merge the
	// port into it rather than overwriting the file with a partial.
	existing, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return writeJSONAtomic(path, delta)
		}
		return fmt.Errorf("read override: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(existing, &m); err != nil {
		return fmt.Errorf("parse override %s: %w", path, err)
	}
	return writeJSONAtomic(path, deepMerge(m, delta))
}

// writeJSONAtomic marshals v and writes it to path via a tmp file + rename so
// a crash mid-write can't leave a truncated manifest behind.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write override: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename override: %w", err)
	}
	return nil
}
