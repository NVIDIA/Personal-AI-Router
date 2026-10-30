// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"
)

const presenceRefusalWindow = 500 * time.Millisecond

// A shared passive probe has its own bound, so one caller's cancellation does
// not stop other readers. It is canceled early when its last reader leaves.
const passiveProbeTimeout = 4 * time.Second

type statusProbeKey struct {
	revision uint64
	port     int
}

type statusProbeRun struct {
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
	waiters int
	err     error
	stale   bool
}

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
	installed, binPath := detectPath(st)
	st.mu.Lock()
	changed := st.installed != installed || (binPath != "" && st.binPath != binPath)
	st.installed = installed
	if binPath != "" {
		st.binPath = binPath
	}
	if changed {
		st.revision++
	}
	st.mu.Unlock()
	return installed, nil
}

// detectPath observes the filesystem without publishing a partial state. A
// passive status check applies this result together with its port observation.
func detectPath(st *engineState) (bool, string) {
	installed := false
	binPath := ""
	for _, p := range st.plat.Detect {
		r, err := resolvePlaceholders(p, st.pathVars())
		if err != nil {
			continue // detect path references something other than a known directory
		}
		if ep := expandPath(r); fileExists(ep) {
			installed = true
			binPath = ep
			break
		}
	}
	return installed, binPath
}

// Status refreshes installation and fixed-port presence when no lifecycle
// operation is active. During an operation it returns the current snapshot.
func (e *Executor) Status(engine string) (EngineStatus, error) {
	return e.StatusAtPort(engine, 0)
}

// StatusAtPort is Status with an optional one-shot probe port. A positive port
// lets upgrade orchestration identify a legacy listener before changing the
// persisted backend port; successful identification updates the live state.
func (e *Executor) StatusAtPort(engine string, probePort int) (EngineStatus, error) {
	return e.statusWithContext(context.Background(), engine, probePort, probePort > 0)
}

// statusWithContext lets deadline-bound callers leave a shared presence probe
// while other status readers continue to use its result.
func (e *Executor) statusWithContext(ctx context.Context, engine string, probePort int, waitForOperation bool) (EngineStatus, error) {
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
	for {
		st.mu.Lock()
		port := st.port
		if probePort > 0 {
			port = probePort
		}
		// Normal status reads can return the current transition state. A
		// one-shot port probe or model sweep must wait for an authoritative
		// post-operation observation, within its caller's deadline.
		if st.opDone != nil {
			if waitForOperation {
				done := st.opDone
				st.mu.Unlock()
				select {
				case <-done:
					continue
				case <-ctx.Done():
					return EngineStatus{}, ctx.Err()
				}
			}
			result := statusLocked(engine, st)
			st.mu.Unlock()
			return result, nil
		}
		key := statusProbeKey{revision: st.revision, port: port}
		if st.statusRuns == nil {
			st.statusRuns = make(map[statusProbeKey]*statusProbeRun)
		}
		run := st.statusRuns[key]
		if run == nil {
			probeCtx, cancel := context.WithTimeout(context.Background(), passiveProbeTimeout)
			run = &statusProbeRun{done: make(chan struct{}), ctx: probeCtx, cancel: cancel}
			st.statusRuns[key] = run
			go e.refreshPassiveStatus(engine, st, key, run)
		}
		run.waiters++
		st.mu.Unlock()
		var waitErr error
		select {
		case <-run.done:
		case <-ctx.Done():
			waitErr = ctx.Err()
		}
		cancelRun := false
		st.mu.Lock()
		run.waiters--
		if run.waiters == 0 && st.statusRuns[key] == run {
			delete(st.statusRuns, key)
			cancelRun = true
		}
		st.mu.Unlock()
		if cancelRun {
			run.cancel()
		}
		if waitErr != nil {
			return EngineStatus{}, waitErr
		}
		if waitForOperation {
			if run.err != nil {
				return EngineStatus{}, run.err
			}
			if run.stale {
				continue
			}
			st.mu.Lock()
			if st.opDone != nil {
				st.mu.Unlock()
				continue
			}
			result := statusLocked(engine, st)
			st.mu.Unlock()
			return result, nil
		}
		return e.snapshot(engine, st), nil
	}
}

// GetInstalled lists every known engine with its current status.
func (e *Executor) GetInstalled() []EngineStatus {
	var out []EngineStatus
	for _, name := range e.reg.Names() {
		status, _ := e.Status(name)
		out = append(out, status)
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
	return statusLocked(engine, st)
}

func statusLocked(engine string, st *engineState) EngineStatus {
	return EngineStatus{
		Engine:      engine,
		DisplayName: st.manifest.DisplayName,
		Installed:   st.installed,
		Running:     st.running,
		Healthy:     st.healthy,
		Port:        st.port,
	}
}

// refreshPassiveStatus stages filesystem and network observations without an
// engine operation lock. Its short commit discards the result if any lifecycle
// operation or another state update ran in the meantime. Concurrent readers of
// the same port and revision wait on one probe.
func (e *Executor) refreshPassiveStatus(engine string, st *engineState, key statusProbeKey, run *statusProbeRun) {
	defer func() {
		st.mu.Lock()
		if st.statusRuns[key] == run {
			delete(st.statusRuns, key)
		}
		close(run.done)
		st.mu.Unlock()
	}()
	defer run.cancel()
	ctx := run.ctx

	st.mu.Lock()
	if st.opDone != nil || st.revision != key.revision {
		run.stale = true
		st.mu.Unlock()
		return
	}
	running, adopted, proc := st.running, st.adopted, st.proc
	ready := st.plat.Runtime.Ready
	mode := st.plat.Runtime.modeOrDefault()
	st.mu.Unlock()

	pathInstalled, binPath := detectPath(st)
	checkPresence := pathInstalled || mode == "process"
	checkPresence = checkPresence && key.port != 0 && ready != nil && proc == nil && (!running || adopted)
	var listener listenerProbeResult
	var identified, refusedConfirmed bool
	if checkPresence {
		probeCtx, stopProbe := context.WithTimeout(ctx, presenceRefusalWindow)
		listener = probeListener(probeCtx, ready, key.port)
		stopProbe()
		switch listener {
		case listenerProbeRefused:
			if running && adopted {
				refusedConfirmed = e.waitUnavailableContext(ctx, ready, key.port, presenceRefusalWindow)
			}
		case listenerProbeReachable:
			identified = e.probe(ctx, ready, key.port)
		}
	}
	if ctx.Err() != nil {
		run.err = ctx.Err()
		return
	}

	st.mu.Lock()
	if st.opDone != nil || st.revision != key.revision {
		run.stale = true
		st.mu.Unlock()
		return
	}
	before := statusLocked(engine, st)
	oldBinPath, oldAdopted := st.binPath, st.adopted
	emit := false
	st.installed = pathInstalled
	if pathInstalled {
		st.binPath = binPath
	}
	if checkPresence {
		switch {
		case listener == listenerProbeRefused && refusedConfirmed:
			emit = before.Running || before.Healthy || oldAdopted || before.Installed != pathInstalled
			if st.healthStop != nil {
				st.healthStop()
				st.healthStop = nil
			}
			st.running = false
			st.healthy = false
			st.adopted = false
			if !pathInstalled {
				st.binPath = ""
			}
		case listener == listenerProbeRefused || listener == listenerProbeIndeterminate:
			// A single refusal or an uncertain transport result cannot prove
			// an adopted service stopped.
			if running && adopted {
				st.installed = true
				if !pathInstalled {
					st.binPath = ""
				}
			}
		case identified && !st.stopping:
			emit = !before.Running || !before.Healthy || !oldAdopted || before.Port != key.port
			st.installed = true
			st.running = true
			st.healthy = true
			st.adopted = true
			st.port = key.port
			if !pathInstalled {
				st.binPath = ""
			}
		case !identified && listener == listenerProbeReachable && running && adopted:
			emit = before.Healthy
			// A live listener that no longer identifies as this engine is
			// unhealthy, but its port must not be treated as free.
			st.installed = true
			st.healthy = false
			if !pathInstalled {
				st.binPath = ""
			}
		}
	}
	after := statusLocked(engine, st)
	if before != after || oldBinPath != st.binPath || oldAdopted != st.adopted {
		st.revision++
	}
	st.mu.Unlock()
	if emit {
		e.emitState(engine)
	}
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
