// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
// runtime.port so the chosen port survives a restart. Only the port is owned
// by this operation: arguments, environment and unrelated overrides survive.
// A host-platform port is updated too, when present, because platform values
// take precedence over shared runtime defaults at load time.
func (e *Executor) persistPort(engine string, port int) error {
	return e.persistRuntimeConfig(engine, port, nil, nil)
}

// persistRuntimeConfig writes the port override plus, when non-nil, the launch
// argument and environment overrides. A nil slice pointer leaves that override
// exactly as it is on disk; an empty slice clears it to "declared, but empty".
func (e *Executor) persistRuntimeConfig(engine string, port int, args, environment *[]string) error {
	if e.overrideDir == "" {
		return fmt.Errorf("no config directory available to persist the port")
	}
	if err := os.MkdirAll(e.overrideDir, 0o700); err != nil {
		return fmt.Errorf("create override dir: %w", err)
	}
	if err := os.Chmod(e.overrideDir, 0o700); err != nil {
		return fmt.Errorf("restrict override dir: %w", err)
	}
	path := filepath.Join(e.overrideDir, engine+".json")
	m := map[string]any{"engine": engine}
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read override: %w", err)
	}
	if err == nil {
		m = nil
		decoder := json.NewDecoder(bytes.NewReader(existing))
		decoder.UseNumber() // unrelated manifest numbers must not lose precision
		if !json.Valid(existing) {
			return fmt.Errorf("override must be a JSON object")
		}
		if err := decoder.Decode(&m); err != nil || m == nil {
			return fmt.Errorf("override must be a JSON object")
		}
		if name, ok := m["engine"].(string); !ok || name != engine {
			return fmt.Errorf("override engine does not match %q", engine)
		}
	}
	rt, err := overrideObject(m, "runtime")
	if err != nil {
		return err
	}
	platforms, err := overrideObject(m, "platforms")
	if err != nil {
		return err
	}
	host := runtime.GOOS + "/" + runtime.GOARCH
	platform, err := overrideObject(platforms, host)
	if err != nil {
		return err
	}
	hostRuntime, err := overrideObject(platform, "runtime")
	if err != nil {
		return err
	}
	def, bundled := e.reg.bundledDefaultPort(engine)
	if bundled && def == port {
		delete(rt, "port")
		delete(hostRuntime, "port")
	} else {
		rt["port"] = port
		_, hostPort := hostRuntime["port"]
		// A bundled platform-specific port would otherwise shadow the new
		// shared value even when the user's override has no platform block.
		var base struct {
			Platforms map[string]struct {
				Runtime map[string]json.RawMessage `json:"runtime"`
			} `json:"platforms"`
		}
		if raw := e.reg.bundledRaw[engine]; len(raw) != 0 {
			if err := json.Unmarshal(raw, &base); err != nil {
				return fmt.Errorf("read bundled port: %w", err)
			}
		}
		if _, bundledHostPort := base.Platforms[host].Runtime["port"]; hostPort || bundledHostPort {
			hostRuntime["port"] = port
		}
	}
	if args != nil {
		literal := append([]string{}, (*args)...)
		hostRuntime["launch_args"] = literal
	}
	if environment != nil {
		hostRuntime["launch_env"] = append([]string{}, (*environment)...)
	}
	setOverrideObject(platform, "runtime", hostRuntime)
	setOverrideObject(platforms, host, platform)
	setOverrideObject(m, "platforms", platforms)
	setOverrideObject(m, "runtime", rt)
	if bundled && len(m) == 1 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove override: %w", err)
		}
		return nil
	}
	return writeJSONAtomic(path, m)
}

func overrideObject(parent map[string]any, key string) (map[string]any, error) {
	value, exists := parent[key]
	if !exists {
		return make(map[string]any), nil
	}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return nil, fmt.Errorf("override %s must be a JSON object", key)
	}
	return object, nil
}

func setOverrideObject(parent map[string]any, key string, object map[string]any) {
	if len(object) == 0 {
		delete(parent, key)
	} else {
		parent[key] = object
	}
}

// writeJSONAtomic marshals v and writes it to path via a tmp file + rename so
// a crash mid-write can't leave a truncated manifest behind. The temporary file
// is flushed before the rename and the directory after it: rename makes the
// swap atomic for a concurrent reader, but on its own guarantees nothing about
// the bytes reaching stable storage, so a power loss could publish an empty file.
func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	// Named by file, not by caller: this also persists PATH ownership records,
	// and a failed receipt write reported as "write override" sends the reader
	// looking at port overrides.
	name := filepath.Base(path)
	file, err := os.CreateTemp(filepath.Dir(path), "."+name+"-*.tmp")
	if err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	// Closes the file on every path. 0600 is asserted by
	// TestSettingsOverrideRestrictsExistingPermissions and is stated rather
	// than inherited from CreateTemp, because the rename has to *restrict* a
	// pre-existing group- or world-readable file, not merely avoid widening it.
	// Every caller writes a per-user file: a launch environment that can hold
	// credentials, a desired-state record, or a PATH ownership receipt.
	if err := writeAndSync(file, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", name, err)
	}
	return syncDir(filepath.Dir(path))
}
