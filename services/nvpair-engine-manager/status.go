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
// occupied/indeterminate target. Callers that delete files must fail closed when
// Occupied is true: a listener that did not pass the identity probe is not
// permission to overwrite it. Whether a port is in the way of a second copy is a
// narrower question, and the observation answers it — see
// portObservation.spawnBlocked.
type presenceResult struct {
	Identified bool
	// Port is the candidate that answered as this engine. It is not the port the
	// caller asked about whenever the engine keeps its own port in a settings
	// file, which is why the answer carries it instead of assuming.
	Port     int
	Occupied bool
	// obs is the round's observation, so a refusal can name the port and the
	// answer that was found there rather than describing the configured one.
	obs portObservation
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

// conflictingEnginePath answers whether the engine is already on this machine by a
// route PAIR does not own. It reads the manifest's conflict paths the way
// detectPath reads its detect paths, and names the first that exists, so a refusal
// can point at the folder the user would have to open instead of describing a
// condition nobody can see.
func conflictingEnginePath(st *engineState) string {
	for _, p := range st.plat.Conflicts {
		r, err := resolvePlaceholders(p, st.pathVars())
		if err != nil {
			continue // conflict path references something other than a known directory
		}
		if ep := expandPath(r); fileExists(ep) {
			return ep
		}
	}
	return ""
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
		return EngineStatus{}, err
	}
	for {
		enabled, _, _ := e.desired.get(engine)
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
			result := statusLocked(engine, st, enabled)
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
			result := statusLocked(engine, st, enabled)
			st.mu.Unlock()
			return result, nil
		}
		return e.snapshot(engine, st), nil
	}
}

// GetInstalled lists every engine this host can run with its current status.
func (e *Executor) GetInstalled() []EngineStatus {
	var out []EngineStatus
	for _, name := range e.reg.Names() {
		status, err := e.Status(name)
		if err != nil {
			// No block for this host, such as vLLM on Windows: the engine
			// cannot be installed or run here, so it is not offered at all.
			continue
		}
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
	enabled, _, _ := e.desired.get(engine)
	st.mu.Lock()
	defer st.mu.Unlock()
	return statusLocked(engine, st, enabled)
}

func statusLocked(engine string, st *engineState, enabled bool) EngineStatus {
	// Enabled model-selecting engines remain available for another Load or
	// request after their serving process exits unexpectedly.
	awaitingModel := st.awaitingModel || (enabled && st.plat != nil && st.plat.Runtime.WaitForModel)
	status := EngineStatus{
		Engine:        engine,
		DisplayName:   st.manifest.DisplayName,
		Installed:     st.installed,
		Enabled:       enabled || st.enabling || st.modelSwitching || st.awaitingModel,
		Running:       st.running,
		Starting:      (st.starting || st.modelSwitching) && !st.running,
		AwaitingModel: st.installed && awaitingModel && !st.running && !st.starting && !st.modelSwitching,
		Healthy:       st.healthy,
		Port:          st.port,
		Owner:         installOwner(st),
	}
	status.CanInstall = canInstallHere(st)
	return status
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
	var obs portObservation
	var refusedConfirmed bool
	if checkPresence {
		// Every candidate port is asked, not only the one being watched. An
		// engine the user started themselves answers on whatever port their own
		// app chose, and that is the only place it will ever answer.
		ports := e.candidatePorts(st, key.port)
		obs = e.probePortObservation(ctx, ready, ports)
		if obs.quiet && running && adopted {
			refusedConfirmed = e.confirmPortsQuiet(ctx, ready, ports, presenceRefusalWindow)
		}
	}
	if ctx.Err() != nil {
		run.err = ctx.Err()
		return
	}

	enabled, _, _ := e.desired.get(engine)
	st.mu.Lock()
	if st.opDone != nil || st.revision != key.revision {
		run.stale = true
		st.mu.Unlock()
		return
	}
	before := statusLocked(engine, st, enabled)
	oldBinPath, oldAdopted := st.binPath, st.adopted
	emit := false
	st.installed = pathInstalled
	if pathInstalled {
		st.binPath = binPath
	}
	if checkPresence {
		switch {
		case obs.quiet && refusedConfirmed:
			emit = before.Running || before.Healthy || oldAdopted || before.Installed != pathInstalled
			if st.healthStop != nil {
				st.healthStop()
				st.healthStop = nil
			}
			st.running = false
			st.healthy = false
			st.adopted = false
			// An adopted service's port was never PAIR's to keep. Once it is
			// gone, the next start goes back to the port PAIR configured for
			// this engine instead of launching a duplicate on the stranger's.
			st.port = st.plat.Runtime.Port
			if !pathInstalled {
				st.binPath = ""
			}
		case !obs.occupied():
			// A single unconfirmed round, or an uncertain transport result,
			// cannot prove an adopted service stopped.
			if running && adopted {
				st.installed = true
				if !pathInstalled {
					st.binPath = ""
				}
			}
		case obs.identified != 0 && !st.stopping:
			emit = !before.Running || !before.Healthy || !oldAdopted || before.Port != obs.identified
			st.installed = true
			st.running = true
			st.healthy = true
			st.adopted = true
			st.port = obs.identified
			if !pathInstalled {
				st.binPath = ""
			}
		case obs.identified == 0 && running && adopted:
			// The port holds something that is not this engine, or could not be
			// read. Neither proves the adopted service stopped, so it keeps its
			// installation; only a listener answering something else is called
			// unhealthy, and its port must not be treated as free. A stop that
			// is still in flight is not a health failure, and an engine that
			// still identifies itself during that stop is not unhealthy either.
			if obs.answered() {
				emit = before.Healthy
				st.healthy = false
			}
			st.installed = true
			if !pathInstalled {
				st.binPath = ""
			}
		}
	}
	// A port that answers PAIR's health check with a credential refusal is this
	// engine, set up in a way PAIR cannot work with, and the state alone would
	// read as "offline". Say so, but only for an engine the user turned on, and
	// only while that is what the port does.
	blocked := enabled && obs.blocked != 0
	notice := blocked != st.authBlocked
	st.authBlocked = blocked
	after := statusLocked(engine, st, enabled)
	if before != after || oldBinPath != st.binPath || oldAdopted != st.adopted {
		st.revision++
	}
	st.mu.Unlock()
	if emit {
		e.emitState(engine)
	}
	switch {
	case blocked && notice:
		e.reporter.report(serviceError{
			ID: authBlockedID(engine), Message: obs.authBlockedNotice(st.manifest.DisplayName),
			Severity: "warning", Action: "none", EngineType: engine,
		})
	case notice:
		e.reporter.clear(authBlockedID(engine))
	}
}

// reconcilePresence reconciles filesystem detection with a fixed-port engine
// service. A successful engine-specific readiness probe is authoritative even
// when the binary lives outside NVPAIR's managed paths. Conversely, readiness
// failure is not proof of shutdown: an HTTP 503 still has a live listener.
// An adopted service is only marked stopped after every candidate port refuses
// repeatedly, so a transient failed readiness response cannot clear it and a
// service that moved to another port is not reported gone.
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
		return presenceResult{
			Identified: running,
			Port:       port,
			Occupied:   running,
			obs:        portObservation{bind: port},
		}
	}

	ports := e.candidatePorts(st, port)
	obs := e.probePortObservation(ctx, ready, ports)
	identified := obs.identified
	if obs.quiet {
		if !running || !adopted || !e.confirmPortsQuiet(ctx, ready, ports, presenceRefusalWindow) {
			return presenceResult{obs: obs}
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
		// The stranger's port is not PAIR's to keep: a later start goes back to
		// the port PAIR configured for this engine.
		st.port = st.plat.Runtime.Port
		if !pathInstalled {
			// A service-only adoption has no managed image. Never leave a stale
			// path that orphan reclamation could terminate.
			st.binPath = ""
		}
		st.mu.Unlock()
		if changed {
			e.emitState(engine)
		}
		return presenceResult{obs: obs}
	}
	if identified == 0 {
		if running && adopted {
			st.mu.Lock()
			changed := obs.answered() && st.healthy
			st.installed = true // previously identified external service
			if obs.answered() {
				st.healthy = false
			}
			if !pathInstalled {
				st.binPath = ""
			}
			st.mu.Unlock()
			if changed {
				e.emitState(engine)
			}
		}
		// A listener is present but did not identify as this engine, so it is
		// never adopted. Whether it also stands in the way of a second copy is a
		// narrower question, answered by spawnBlocked at the callers that would
		// bring one up: only a port that copy needs counts against them.
		return presenceResult{Occupied: true, obs: obs}
	}

	st.mu.Lock()
	if st.stopping && !allowWhileStopping {
		st.mu.Unlock()
		return presenceResult{Identified: true, Port: identified, Occupied: true, obs: obs}
	}
	// Detect resets installed=false on every path-miss. Do not treat that
	// internal refresh as a state transition for an already-adopted service,
	// or every status poll would emit a duplicate state-changed event.
	changed := !st.running || !st.healthy || !st.adopted || st.port != identified
	st.installed = true
	st.running = true
	st.awaitingModel = false
	st.healthy = true
	st.adopted = true
	st.port = identified
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
	return presenceResult{Identified: true, Port: identified, Occupied: true, obs: obs}
}

func (e *Executor) emitState(engine string) {
	st, err := e.state(engine)
	if err != nil {
		return
	}
	e.notify("engine:state-changed", e.snapshot(engine, st))
}
