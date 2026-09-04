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
	"strings"
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

// persistPort pins runtime.port in the per-engine manifest override so the
// chosen port survives a restart. Back at the bundled default, the port key is
// dropped again rather than pinned forever. See persistRuntimeField.
func (e *Executor) persistPort(engine string, port int) error {
	def, bundled := e.reg.bundledDefaultPort(engine)
	return e.persistRuntimeField(engine, "port", port, bundled && def == port)
}

// persistModel pins runtime.model in the same override file persistPort writes.
func (e *Executor) persistModel(engine, model string) error {
	def, bundled := e.reg.bundledDefaultModel(engine)
	return e.persistRuntimeField(engine, "model", model, bundled && def == model)
}

// persistRuntimeField writes one runtime.<field> value into the per-engine
// manifest override that deep-merges onto the bundled manifest at load, so the
// manifest stays the single source of truth for the setting and it is restored
// on the next start with no separate store.
//
// The file is always read-modify-written rather than replaced with a one-key
// delta: port and model are set independently, and a wholesale write would
// silently drop whichever the caller wasn't changing. atDefault removes just
// that field; the file itself is unlinked only once no override remains, which
// keeps the override set minimal without discarding a sibling setting. Atomic
// (tmp + rename).
func (e *Executor) persistRuntimeField(engine, field string, value any, atDefault bool) error {
	if e.overrideDir == "" {
		return fmt.Errorf("no config directory available to persist the %s", field)
	}
	if err := os.MkdirAll(e.overrideDir, 0o755); err != nil {
		return fmt.Errorf("create override dir: %w", err)
	}
	path := filepath.Join(e.overrideDir, engine+".json")

	doc := map[string]any{}
	existing, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(existing, &doc); err != nil {
			return fmt.Errorf("parse override %s: %w", path, err)
		}
	case !os.IsNotExist(err):
		return fmt.Errorf("read override: %w", err)
	}

	rt, _ := doc["runtime"].(map[string]any)
	if rt == nil {
		rt = map[string]any{}
	}
	if atDefault {
		delete(rt, field)
	} else {
		rt[field] = value
	}
	doc["engine"] = engine
	doc["runtime"] = rt

	// A bundled engine's override exists only to carry deltas, so an override
	// with none left is removed. A non-bundled engine's full manifest lives
	// only here and is never removed.
	if _, bundled := e.reg.bundledDefaultPort(engine); bundled && len(rt) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove override: %w", err)
		}
		return nil
	}
	return writeJSONAtomic(path, doc)
}

// SetModel persists the model an engine serves as a manifest override and
// applies it: a running engine is restarted onto the new model, since an engine
// that templates {model} into its launch command can only serve the model it
// was started with. Empty removes the override (back to the bundled default),
// which leaves an engine that requires a model unable to start until one is
// chosen again. Held under the engine's op lock, like SetPort.
//
// A running, adopted engine is refused for the same reason SetPort refuses one:
// NVPAIR cannot restart a process it did not start, and the externally-managed
// instance would keep serving its own model while the manifest claimed another.
func (e *Executor) SetModel(ctx context.Context, engine, model string) (EngineStatus, error) {
	model = strings.TrimSpace(model)
	st, err := e.state(engine)
	if err != nil {
		return EngineStatus{}, err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()

	st.mu.Lock()
	wasRunning := st.running
	adopted := st.adopted
	st.mu.Unlock()

	if wasRunning && adopted && !canMoveAdoptedEngine(st.plat.Runtime) {
		return EngineStatus{}, fmt.Errorf("cannot change the model %s serves: it is running under external management (NVPAIR adopted it rather than starting it), so NVPAIR cannot restart it — stop it in its own app first, then set the model", engine)
	}

	oldModel := st.plat.Runtime.Model
	if wasRunning {
		if err := e.doStop(st, engine); err != nil {
			return EngineStatus{}, err
		}
	}

	if err := e.persistModel(engine, model); err != nil {
		if !wasRunning {
			return EngineStatus{}, err
		}
		st.mu.Lock()
		st.plat.Runtime.Model = oldModel
		st.mu.Unlock()
		restartErr := e.doStart(ctx, st, engine, startOpts{})
		return EngineStatus{}, errors.Join(err, restartErr)
	}

	st.mu.Lock()
	st.plat.Runtime.Model = model
	st.mu.Unlock()

	if wasRunning {
		if err := e.doStart(ctx, st, engine, startOpts{}); err != nil {
			return EngineStatus{}, err
		}
	} else {
		e.emitState(engine)
	}
	return e.snapshot(engine, st), nil
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
