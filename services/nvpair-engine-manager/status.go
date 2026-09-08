// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"
)

const presenceRefusalWindow = 500 * time.Millisecond

// presenceResult distinguishes a positively identified engine from an
// occupied/indeterminate target. Callers that might spawn or install must fail
// closed when Occupied is true: a listener that did not pass the identity probe
// is not permission to overwrite it.
type presenceResult struct {
	Identified bool
	Occupied   bool
}

// Detect updates and returns whether the engine is installed, based on
// the manifest's detect paths (env-expanded existence checks).
func (e *Executor) Detect(engine string) (bool, error) {
	st, err := e.state(engine)
	if err != nil {
		return false, err
	}
	installed := false
	binPath := ""
	for _, p := range st.plat.Detect {
		r, err := resolvePlaceholders(p, map[string]string{"install_dir": st.installDir})
		if err != nil {
			continue // detect path references something other than {install_dir}
		}
		if ep := expandPath(r); fileExists(ep) {
			installed = true
			binPath = ep
			break
		}
	}
	st.mu.Lock()
	st.installed = installed
	if binPath != "" {
		st.binPath = binPath
	}
	st.mu.Unlock()
	return installed, nil
}

// Status returns a fresh snapshot (re-detecting installed-ness).
func (e *Executor) Status(engine string) (EngineStatus, error) {
	return e.StatusAtPort(engine, 0)
}

// StatusAtPort is Status with an optional one-shot probe port. A positive port
// lets upgrade orchestration identify a legacy listener before changing the
// persisted backend port; successful identification updates the live state.
func (e *Executor) StatusAtPort(engine string, probePort int) (EngineStatus, error) {
	st, err := e.state(engine)
	if err != nil {
		// Known engine with no host-platform block: return a shell status
		// (matching get-installed) rather than erroring, so status is
		// consistent for the same engine across methods.
		if m, ok := e.reg.Get(engine); ok {
			return EngineStatus{Engine: engine, DisplayName: m.DisplayName}, nil
		}
		return EngineStatus{}, err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	pathInstalled, _ := e.Detect(engine)
	st.mu.Lock()
	port := st.port
	st.mu.Unlock()
	if probePort > 0 {
		port = probePort
	}
	e.reconcilePresence(context.Background(), engine, st, pathInstalled, port, false)
	return e.snapshot(engine, st), nil
}

// GetInstalled lists every known engine with its current status.
func (e *Executor) GetInstalled() []EngineStatus {
	var out []EngineStatus
	for _, name := range e.reg.Names() {
		st, err := e.state(name)
		if err != nil {
			// Known engine with no block for this host: surface a shell
			// status so the UI can still list it as unavailable here.
			dn := name
			if m, ok := e.reg.Get(name); ok {
				dn = m.DisplayName
			}
			out = append(out, EngineStatus{Engine: name, DisplayName: dn})
			continue
		}
		st.opMu.Lock()
		pathInstalled, _ := e.Detect(name)
		st.mu.Lock()
		port := st.port
		st.mu.Unlock()
		e.reconcilePresence(context.Background(), name, st, pathInstalled, port, false)
		out = append(out, e.snapshot(name, st))
		st.opMu.Unlock()
	}
	return out
}

// Logs returns the engine's captured-output ring.
func (e *Executor) Logs(engine string) ([]LogLine, error) {
	st, err := e.state(engine)
	if err != nil {
		return nil, err
	}
	return st.logs.snapshot(), nil
}

// Errors returns the recent structured-error ring.
func (e *Executor) Errors() []serviceError {
	return e.reporter.snapshot()
}

func (e *Executor) snapshot(engine string, st *engineState) EngineStatus {
	st.mu.Lock()
	defer st.mu.Unlock()
	status := EngineStatus{
		Engine:      engine,
		DisplayName: st.manifest.DisplayName,
		Installed:   st.installed,
		Running:     st.running,
		Healthy:     st.healthy,
		Port:        st.port,
	}
	if st.plat != nil {
		status.Model = st.plat.Runtime.Model
	}
	return status
}

// reconcilePresence reconciles filesystem detection with a fixed-port engine
// service. A successful engine-specific readiness probe is authoritative even
// when the binary lives outside NVPAIR's managed paths. Conversely, readiness
// failure is not proof of shutdown: an HTTP 503 still has a live listener.
// An adopted service is only marked stopped after repeated explicit connection
// refusals, preventing a transient failed readiness response from clearing it.
//
// allowWhileStopping is true only for an explicit Start. Passive status checks
// must not undo a deliberate OFF by re-adopting a service behind st.stopping.
func (e *Executor) reconcilePresence(ctx context.Context, engine string, st *engineState, pathInstalled bool, port int, allowWhileStopping bool) presenceResult {
	st.mu.Lock()
	running := st.running
	adopted := st.adopted
	proc := st.proc
	ready := st.plat.Runtime.Ready
	st.mu.Unlock()

	// A command-mode engine needs its control CLI. A compatible HTTP endpoint
	// alone (for example another OpenAI server on LM Studio's port) is not an
	// installation and must not suppress the installer.
	if !pathInstalled && st.plat.Runtime.modeOrDefault() != "process" {
		return presenceResult{}
	}

	if port == 0 || ready == nil || proc != nil || (running && !adopted) {
		return presenceResult{Identified: running, Occupied: running}
	}

	probeCtx, cancel := context.WithTimeout(ctx, presenceRefusalWindow)
	listener := probeListener(probeCtx, ready, port)
	cancel()
	if listener == listenerProbeRefused {
		if !running || !adopted || !e.waitUnavailable(ready, port, presenceRefusalWindow) {
			return presenceResult{}
		}
		st.mu.Lock()
		changed := st.running || st.healthy || st.adopted || st.installed != pathInstalled
		if st.healthStop != nil {
			st.healthStop()
			st.healthStop = nil
		}
		st.installed = pathInstalled
		st.running = false
		st.healthy = false
		st.adopted = false
		st.proc = nil
		if !pathInstalled {
			// A service-only adoption has no managed image. Never leave a stale
			// path that orphan reclamation could terminate.
			st.binPath = ""
		}
		st.mu.Unlock()
		if changed {
			e.emitState(engine)
		}
		return presenceResult{}
	}
	if listener == listenerProbeIndeterminate {
		// DNS/routing/resource failures cannot prove the port is free. Preserve
		// existing state and prevent install/start from writing over it.
		if running && adopted {
			st.mu.Lock()
			st.installed = true
			if !pathInstalled {
				st.binPath = ""
			}
			st.mu.Unlock()
		}
		return presenceResult{Occupied: true}
	}

	identified := e.probe(ctx, ready, port)
	if !identified {
		if running && adopted {
			st.mu.Lock()
			changed := st.healthy
			st.installed = true // previously identified external service
			st.healthy = false
			if !pathInstalled {
				st.binPath = ""
			}
			st.mu.Unlock()
			if changed {
				e.emitState(engine)
			}
			return presenceResult{Occupied: true}
		}
		// A listener is present but did not identify as this engine. Do not
		// adopt it, spawn over it, or download a second copy for this port.
		return presenceResult{Occupied: true}
	}

	st.mu.Lock()
	if st.stopping && !allowWhileStopping {
		st.mu.Unlock()
		return presenceResult{Identified: true, Occupied: true}
	}
	// Detect resets installed=false on every path-miss. Do not treat that
	// internal refresh as a state transition for an already-adopted service,
	// or every status poll would emit a duplicate state-changed event.
	changed := !st.running || !st.healthy || !st.adopted || st.port != port
	st.installed = true
	st.running = true
	st.healthy = true
	st.adopted = true
	st.port = port
	if allowWhileStopping {
		st.stopping = false
	}
	if !pathInstalled {
		// Detection missed, so this is explicitly an external service. Keep
		// binPath empty so stop/uninstall can never treat it as our orphan.
		st.binPath = ""
	}
	st.mu.Unlock()
	if changed {
		e.emitState(engine)
	}
	return presenceResult{Identified: true, Occupied: true}
}

func (e *Executor) emitState(engine string) {
	st, err := e.state(engine)
	if err != nil {
		return
	}
	e.notify("engine:state-changed", e.snapshot(engine, st))
}
