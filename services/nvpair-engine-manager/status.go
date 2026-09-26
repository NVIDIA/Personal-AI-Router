// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"nvpair-shared/engines"
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
	if engine == "vllm" {
		return detectManagedVLLM(st)
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
			if _, supported := m.HostPlatform(); !supported {
				return unavailableEngineStatus(engine, m.DisplayName), nil
			}
		}
		return EngineStatus{}, err
	}
	if engine == "llamacpp" {
		if !st.opMu.TryLock() {
			if probePort > 0 {
				return EngineStatus{}, fmt.Errorf("llama lifecycle is busy; cannot probe an alternate port")
			}
			return e.snapshot(engine, st), nil
		}
	} else {
		st.opMu.Lock()
	}
	defer st.opMu.Unlock()
	if engine == "vllm" && probePort == 0 {
		return e.refreshVLLMStatusLocked(context.Background(), st), nil
	}
	return e.statusAtPortLocked(context.Background(), engine, st, probePort), nil
}

type vllmStatusRefresh struct {
	done   chan struct{}
	status EngineStatus
	ok     bool
}

// refreshVLLMStatusLocked runs the full vLLM status reconciliation and
// publishes it while it holds opMu. The caller holds opMu.
func (e *Executor) refreshVLLMStatusLocked(ctx context.Context, st *engineState) EngineStatus {
	refresh := &vllmStatusRefresh{done: make(chan struct{})}
	st.mu.Lock()
	st.statusRefresh = refresh
	st.mu.Unlock()
	defer func() {
		st.mu.Lock()
		if st.statusRefresh == refresh {
			st.statusRefresh = nil
		}
		st.mu.Unlock()
		close(refresh.done)
	}()
	refresh.status = e.statusAtPortLocked(ctx, "vllm", st, 0)
	refresh.ok = ctx.Err() == nil
	return refresh.status
}

// statusAtPortLocked retains the full lifecycle/status reconciliation for
// callers that already own opMu. Inventory readers use a separate bounded
// wait so a long operation cannot queue every peer model/status request.
func (e *Executor) statusAtPortLocked(ctx context.Context, engine string, st *engineState, probePort int) EngineStatus {
	if engine == "vllm" {
		if err := e.reconcileManagedVLLMActivation(ctx, st); err != nil {
			return e.managedStateErrorStatus(engine, st, err)
		}
	}
	pathInstalled, detectErr := e.Detect(engine)
	if detectErr != nil {
		return e.managedStateErrorStatus(engine, st, detectErr)
	}
	st.mu.Lock()
	port := st.port
	st.mu.Unlock()
	if probePort > 0 {
		port = probePort
	}
	if engine == "vllm" && pathInstalled {
		if _, _, observeErr := e.observeManagedVLLMListener(ctx, st, activeVLLMRuntimeID(st)); observeErr != nil {
			return e.managedStateErrorStatus(engine, st, observeErr)
		}
	}
	e.reconcilePresence(ctx, engine, st, pathInstalled, port, false)
	return e.snapshot(engine, st)
}

// vllmStatusForRead never queues behind a vLLM lifecycle operation. Its one
// in-flight validation keeps ownership checks on the served-model path. When
// another status refresh holds the lock it shares that refresh's result, so
// routine status traffic cannot withdraw a served model; a lifecycle operation
// or a slow validation leaves read-only callers with cached state instead.
func (e *Executor) vllmStatusForRead(ctx context.Context, st *engineState) (EngineStatus, bool) {
	if ctx.Err() != nil {
		return e.cachedVLLMStatus(st), false
	}
	if !st.opMu.TryLock() {
		return e.sharedVLLMStatus(ctx, st)
	}
	result := make(chan EngineStatus, 1)
	go func() {
		defer st.opMu.Unlock()
		if ctx.Err() != nil {
			return
		}
		result <- e.refreshVLLMStatusLocked(ctx, st)
	}()
	select {
	case status := <-result:
		if ctx.Err() != nil {
			return e.cachedVLLMStatus(st), false
		}
		return status, true
	case <-ctx.Done():
		return e.cachedVLLMStatus(st), false
	}
}

func (e *Executor) sharedVLLMStatus(ctx context.Context, st *engineState) (EngineStatus, bool) {
	st.mu.Lock()
	refresh := st.statusRefresh
	st.mu.Unlock()
	if refresh == nil {
		return e.cachedVLLMStatus(st), false
	}
	select {
	case <-refresh.done:
		if refresh.ok {
			return refresh.status, true
		}
	case <-ctx.Done():
	}
	return e.cachedVLLMStatus(st), false
}

func (e *Executor) cachedVLLMStatus(st *engineState) EngineStatus {
	return e.snapshotWithSupport("vllm", st, false)
}

// GetInstalled lists every known engine with its current status.
func (e *Executor) GetInstalled() []EngineStatus {
	return e.GetInstalledContext(context.Background())
}

// GetInstalledContext bounds vLLM's inventory read while retaining the full
// status path for other engines. A busy vLLM reports its last in-memory state;
// mutating operations still take opMu and revalidate ownership themselves.
func (e *Executor) GetInstalledContext(parent context.Context) []EngineStatus {
	ctx, cancel := context.WithTimeout(parent, modelsTimeout)
	defer cancel()
	var out []EngineStatus
	for _, name := range e.reg.Names() {
		st, err := e.state(name)
		if err != nil {
			// Known engine with no block for this host: surface a shell
			// status so the UI can still list it as unavailable here.
			dn := name
			reason := ""
			if m, ok := e.reg.Get(name); ok {
				dn = m.DisplayName
				if _, supported := m.HostPlatform(); supported {
					reason = err.Error()
				}
			}
			status := unavailableEngineStatus(name, dn)
			if reason != "" {
				status.InstallReason = reason
			}
			out = append(out, status)
			continue
		}
		if name == "vllm" {
			status, _ := e.vllmStatusForRead(ctx, st)
			out = append(out, status)
			continue
		}
		if name == "llamacpp" {
			if !st.opMu.TryLock() {
				out = append(out, e.snapshot(name, st))
				continue
			}
		} else {
			st.opMu.Lock()
		}
		pathInstalled, detectErr := e.Detect(name)
		if detectErr != nil {
			out = append(out, e.managedStateErrorStatus(name, st, detectErr))
			st.opMu.Unlock()
			continue
		}
		st.mu.Lock()
		port := st.port
		st.mu.Unlock()
		e.reconcilePresence(context.Background(), name, st, pathInstalled, port, false)
		out = append(out, e.snapshot(name, st))
		st.opMu.Unlock()
	}
	return out
}

func (e *Executor) managedStateErrorStatus(engine string, st *engineState, stateErr error) EngineStatus {
	status := e.snapshot(engine, st)
	unsupported := false
	status.Installed, status.Running, status.Healthy, status.Managed, status.Routable = false, false, false, false, false
	status.InstallSupported = &unsupported
	status.InstallReason = "Managed state unavailable: " + stateErr.Error()
	status.Version = ""
	return status
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
	return e.snapshotWithSupport(engine, st, true)
}

func (e *Executor) snapshotWithSupport(engine string, st *engineState, probeSupport bool) EngineStatus {
	enabled, known, err := e.desired.get(engine)
	if err != nil || !known {
		if catalog, ok := engines.ByName(engine); ok {
			enabled = catalog.EnabledByDefault
		} else {
			enabled = false
		}
	}
	var installSupported *bool
	installReason := ""
	if probeSupport {
		supported, reason := installSupport(engine, st.plat)
		installSupported, installReason = &supported, reason
	}
	st.mu.Lock()
	managed := !st.adopted && st.installed && isManagedInstallPath(st.binPath, st.installDir)
	group, rank := st.vllmGroup, st.vllmRank
	installDir := st.installDir
	status := EngineStatus{
		Engine:           engine,
		DisplayName:      st.manifest.DisplayName,
		Installed:        st.installed,
		Running:          st.running,
		Healthy:          st.healthy,
		Enabled:          enabled,
		Managed:          managed,
		Adopted:          st.adopted,
		Routable:         enabled && st.running && st.healthy,
		InstallSupported: installSupported,
		InstallReason:    installReason,
		Version:          st.version,
		Port:             st.port,
	}
	st.mu.Unlock()
	if engine == "llamacpp" && status.Managed {
		status.Acceleration, status.Devices = llamaReceiptAcceleration(installDir)
	}
	if engine == "vllm" {
		status.SelectedModel, _ = selectedVLLMModel(st)
		if rank != nil && rank.reserved() {
			return rank.routeStatus(status)
		}
		if group != nil {
			return group.routeStatus(e.vllmNodeID, status)
		}
	}
	return status
}

func unavailableEngineStatus(engine, displayName string) EngineStatus {
	supported := false
	return EngineStatus{
		Engine: engine, DisplayName: displayName,
		InstallSupported: &supported, InstallReason: unavailablePlatformInstallReason(engine),
	}
}

func unavailablePlatformInstallReason(engine string) string {
	switch engine {
	case "vllm":
		return vllmUnsupportedPlatformReason(runtime.GOOS, runtime.GOARCH)
	case "llamacpp":
		if supported, reason := llamaInstallSupport(runtime.GOOS, runtime.GOARCH); !supported {
			return reason
		}
		return "No llama installer recipe is available for this operating system and architecture."
	}
	return "No managed recipe for this platform"
}

func installSupportReason(supported bool) string {
	if supported {
		return ""
	}
	return "Installation is not available for this engine on this platform"
}

func installSupport(engine string, platform *Platform) (bool, string) {
	if engine == "llamacpp" {
		supported, reason := llamaInstallSupport(runtime.GOOS, runtime.GOARCH)
		if missing := llamaPrerequisite(); supported && missing != "" {
			return false, missing
		}
		return supported, reason
	}
	if platform == nil || platform.Install == nil {
		return false, installSupportReason(false)
	}
	for _, command := range platform.Install.Requires {
		if _, err := exec.LookPath(command); err != nil {
			return false, fmt.Sprintf("Required command %s is not available", command)
		}
	}
	if engine == "vllm" {
		return probeVLLMInstallSupport(context.Background())
	}
	return true, ""
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
