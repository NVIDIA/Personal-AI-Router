// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	unavailableConfirmations  = 3
	engineIdentityProbeHeader = "X-NVPAIR-Engine-Identity-Probe"
)

type listenerProbeResult uint8

const (
	listenerProbeIndeterminate listenerProbeResult = iota
	listenerProbeReachable
	listenerProbeRefused
)

// waitDetect polls Detect until it reports `want` or the timeout
// elapses. Installers/uninstallers (notably Windows Inno setups) often
// finish their file work asynchronously, so a single immediate check
// races them.
func (e *Executor) waitDetect(engine string, want bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if ok, _ := e.Detect(engine); ok == want {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// startOpts carries per-call start overrides. Zero values mean "use the
// manifest": Port 0 => the manifest port (which may itself be 0 =
// auto-assign a free loopback port); Bind "" => the manifest's
// runtime.bind (which itself defaults to loopback).
type startOpts struct {
	Port int
	Bind string
	// Model overrides runtime.model for this launch only, for an engine that
	// serves one model per process. "" => the manifest's configured model.
	Model string
}

// effectiveBind picks the listen address substituted as {host}: a per-call
// override wins, else the manifest's runtime.bind, else loopback (the safe
// default for an engine that does not opt into LAN exposure).
func effectiveBind(manifestBind, override string) string {
	switch {
	case override != "":
		return override
	case manifestBind != "":
		return manifestBind
	default:
		return "127.0.0.1"
	}
}

// Start launches the engine and waits for its readiness probe, then
// begins the health loop. No-op if already running. It branches on the
// runtime mode: "process" (spawn + own the process) or "command" (run
// bring-up commands; liveness comes from the probe).
func (e *Executor) Start(ctx context.Context, engine string) error {
	return e.StartWith(ctx, engine, startOpts{})
}

// StartWith is Start with per-call overrides. The overrides are not
// persisted — a service restart reverts to the manifest.
func (e *Executor) StartWith(ctx context.Context, engine string, opts startOpts) error {
	st, err := e.state(engine)
	if err != nil {
		return err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	if err := e.doStart(ctx, st, engine, opts); err != nil {
		return err
	}
	return e.setDesiredEnabled(engine, true)
}

// doStart performs the start. Callers must hold st.opMu.
func (e *Executor) doStart(ctx context.Context, st *engineState, engine string, opts startOpts) error {
	ctx, cancel := context.WithCancel(ctx)
	st.mu.Lock()
	st.startCancel = cancel
	st.mu.Unlock()
	defer func() {
		cancel()
		st.mu.Lock()
		st.startCancel = nil
		st.mu.Unlock()
	}()
	if e.shuttingDown.Load() {
		return context.Canceled
	}

	st.mu.Lock()
	running := st.running
	adopted := st.adopted
	if running && !adopted {
		st.mu.Unlock()
		return nil
	}
	st.mu.Unlock()

	pathInstalled, _ := e.Detect(engine)
	rt := st.plat.Runtime
	port := st.port
	if opts.Port > 0 {
		port = opts.Port
	}
	if port == 0 {
		p, err := freePort()
		if err != nil {
			return fmt.Errorf("allocate port: %w", err)
		}
		port = p
	}
	if err := e.reservedPortError(port); err != nil {
		return err
	}
	presence := e.reconcilePresence(ctx, engine, st, pathInstalled, port, true)
	if presence.Identified {
		slog.Info("adopting already-running external engine", "engine", engine, "port", port)
		st.mu.Lock()
		st.gen++
		gen := st.gen
		st.mu.Unlock()
		e.reporter.clear(startFailedID(engine))
		e.reporter.clear(exitedID(engine))
		e.reporter.clear(unhealthyID(engine))
		if rt.Health != nil {
			hctx, cancel := context.WithCancel(context.Background())
			st.mu.Lock()
			if st.healthStop != nil {
				st.healthStop()
			}
			st.healthStop = cancel
			st.mu.Unlock()
			go e.runHealth(hctx, st, engine, port, gen)
		}
		return nil
	}
	if presence.Occupied && rt.modeOrDefault() == "process" {
		return fmt.Errorf("cannot start engine %q on port %d: the port is occupied by a service that did not identify as %s", engine, port, st.manifest.DisplayName)
	}
	if !pathInstalled {
		return fmt.Errorf("engine %q is not installed", engine)
	}
	model := rt.Model
	if opts.Model != "" {
		model = opts.Model
	}
	// An engine whose launch template substitutes {model} serves exactly one
	// model per process and has nothing to serve until one is chosen. Fail with
	// guidance rather than spawning a process that cannot answer.
	if rt.referencesModel() && strings.TrimSpace(model) == "" {
		return fmt.Errorf("cannot start %s: it serves one model per process, and no model is configured — choose the model to serve in Engine settings", st.manifest.DisplayName)
	}
	vars := map[string]string{
		"host":        effectiveBind(rt.Bind, opts.Bind),
		"port":        strconv.Itoa(port),
		"install_dir": st.installDir,
	}
	if rt.CLI != "" {
		vars["cli"] = expandPath(rt.CLI)
	}
	if model != "" {
		vars["model"] = model
	}

	st.mu.Lock()
	st.port = port
	st.stopping = false
	st.mu.Unlock()

	if err := e.bringUp(ctx, st, engine, rt, port, vars); err != nil {
		return err
	}

	st.mu.Lock()
	st.running = true
	st.healthy = true
	st.adopted = false
	st.gen++
	gen := st.gen
	st.mu.Unlock()
	e.reporter.clear(startFailedID(engine))
	e.reporter.clear(exitedID(engine))
	e.reporter.clear(unhealthyID(engine))
	e.emitState(engine)

	if rt.Health != nil {
		hctx, cancel := context.WithCancel(context.Background())
		st.mu.Lock()
		st.healthStop = cancel
		st.mu.Unlock()
		go e.runHealth(hctx, st, engine, port, gen)
	}
	return nil
}

func (e *Executor) bringUp(ctx context.Context, st *engineState, engine string, rt Runtime, port int, vars map[string]string) error {
	if rt.modeOrDefault() == "command" {
		return e.bringUpCommand(ctx, st, engine, rt, port, vars)
	}
	return e.bringUpProcess(ctx, st, engine, rt, port, vars)
}

// bringUpProcess spawns the engine as a foreground process this service
// owns, then waits for readiness.
func (e *Executor) bringUpProcess(ctx context.Context, st *engineState, engine string, rt Runtime, port int, vars map[string]string) error {
	st.mu.Lock()
	binPath := st.binPath
	st.mu.Unlock()
	if binPath == "" {
		b, err := resolvePlaceholders(rt.Bin, vars)
		if err != nil {
			return err
		}
		binPath = expandPath(b)
	}
	vars["bin"] = binPath

	args, err := resolveArgs(rt.launchArgs(), vars)
	if err != nil {
		return err
	}
	env := make(map[string]string, len(rt.Env))
	for k, v := range rt.Env {
		rv, err := resolvePlaceholders(v, vars)
		if err != nil {
			return err
		}
		env[k] = rv
	}

	proc, err := startManagedProc(binPath, args, env, func(stream, line string) {
		st.logs.append(stream, line)
	})
	if err != nil {
		werr := fmt.Errorf("start %s: %w", engine, err)
		e.reportStartFailedUnlessShuttingDown(ctx, engine, werr)
		return werr
	}
	st.mu.Lock()
	st.proc = proc
	st.mu.Unlock()
	e.watch(st, engine, proc)

	if err := e.waitReady(ctx, rt.Ready, port); err != nil {
		// Mark stopping so the watcher doesn't report the deliberate kill
		// as an unexpected "exited" event.
		st.mu.Lock()
		st.stopping = true
		st.mu.Unlock()
		proc.stop()
		st.mu.Lock()
		st.proc = nil
		st.mu.Unlock()
		werr := fmt.Errorf("engine %q did not become ready: %w", engine, err)
		e.reportStartFailedUnlessShuttingDown(ctx, engine, werr)
		e.emitState(engine)
		return werr
	}
	return nil
}

// bringUpCommand runs the manifest's ordered start commands (for a
// daemon-style engine such as LM Studio), then waits for readiness.
// There is no owned process — liveness comes from the probe.
func (e *Executor) bringUpCommand(ctx context.Context, st *engineState, engine string, rt Runtime, port int, vars map[string]string) error {
	st.mu.Lock()
	st.proc = nil
	st.mu.Unlock()
	started := false
	cleanup := func(startErr error) error {
		if !started {
			return startErr
		}
		if stopErr := e.runCommandStop(st, engine, rt, port); stopErr != nil {
			// A non-zero stop CLI can still have taken the daemon down. Confirm
			// that before surfacing a cleanup failure.
			if rt.Ready != nil && e.waitUnavailable(rt.Ready, port, time.Second) {
				return startErr
			}
			return errors.Join(startErr, fmt.Errorf("clean up failed %s start: %w", engine, stopErr))
		}
		return startErr
	}
	for _, cmd := range rt.Start {
		argv, err := resolveArgs(cmd, vars)
		if err != nil {
			e.reportStartFailedUnlessShuttingDown(ctx, engine, err)
			return cleanup(err)
		}
		for i := range argv {
			argv[i] = expandPath(argv[i])
		}
		if err := e.runCommand(ctx, argv); err != nil {
			werr := fmt.Errorf("start command failed: %w", err)
			e.reportStartFailedUnlessShuttingDown(ctx, engine, werr)
			return cleanup(werr)
		}
		// A successful control command may have launched a detached daemon.
		// From here on, any readiness/cancellation failure must run the bounded
		// official stop command before Start releases the lifecycle lock.
		started = true
	}
	if err := e.waitReady(ctx, rt.Ready, port); err != nil {
		werr := fmt.Errorf("engine %q did not become ready: %w", engine, err)
		e.reportStartFailedUnlessShuttingDown(ctx, engine, werr)
		e.emitState(engine)
		return cleanup(werr)
	}
	return nil
}

// watch reports an unexpected engine exit (one not initiated by Stop).
func (e *Executor) watch(st *engineState, engine string, proc *managedProc) {
	go func() {
		<-proc.done
		st.mu.Lock()
		stopping := st.stopping
		current := st.proc == proc
		if current {
			st.proc = nil
			st.running = false
			st.healthy = false
			if st.healthStop != nil {
				st.healthStop()
				st.healthStop = nil
			}
		}
		st.mu.Unlock()
		if current && !stopping {
			e.reporter.report(serviceError{
				ID: exitedID(engine), Message: engine + " exited unexpectedly",
				Severity: "error", Action: "none", EngineType: engine,
			})
			e.emitState(engine)
		}
	}()
}

// Stop terminates a running engine. In process mode it signals the
// owned process (graceful then forced); in command mode it runs the
// manifest's stop command. No-op if not running.
func (e *Executor) Stop(engine string) error {
	st, err := e.state(engine)
	if err != nil {
		return err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	stopErr := e.doStop(st, engine)
	// A user-initiated Stop is explicit OFF intent. Persist it even when
	// doStop declines a genuinely-foreign listener we can't terminate, so the
	// health loop and RestoreEnabled honor the OFF instead of flipping the
	// engine back on. Restart and StopAll call doStop directly and never
	// rewrite intent — shutdown must not erase the user's saved choice.
	setErr := e.setDesiredEnabled(engine, false)
	return errors.Join(stopErr, setErr)
}

// doStop performs the stop. Callers must hold st.opMu.
func (e *Executor) doStop(st *engineState, engine string) error {
	rt := st.plat.Runtime
	mode := rt.modeOrDefault()
	st.mu.Lock()
	if !st.running {
		st.mu.Unlock()
		return nil
	}
	proc := st.proc
	port := st.port
	adopted := st.adopted
	binPath := st.binPath
	if mode != "command" && proc == nil {
		st.mu.Unlock()
		if adopted {
			if rt.Ready != nil && e.waitUnavailable(rt.Ready, port, time.Second) {
				e.markStopped(st, engine)
				return nil
			}
			// The listener is still up. If it's a PAIR-managed orphan on our
			// own port — our engine binary left running by a prior run that
			// lost its handle — reclaim it by terminating the PID bound to the
			// port, so a user OFF actually takes effect instead of being
			// refused forever. This is precise to the port, so a genuine
			// desktop app on a different port (e.g. Ollama's own :11434 while
			// we manage :11435) is never touched; we decline only a process
			// whose image we can't confirm is ours.
			pid, image, ok := pidOnPort(port)
			if ok && isManagedInstallPath(binPath, st.installDir) && isOurEngineImage(image, binPath) {
				st.mu.Lock()
				st.stopping = true
				if st.healthStop != nil {
					st.healthStop()
					st.healthStop = nil
				}
				st.mu.Unlock()
				grace := stopGrace(rt)
				terminatePID(pid, grace)
				if !pidAlive(pid) {
					e.markStopped(st, engine)
					return nil
				}
				if rt.Ready == nil || e.waitUnavailable(rt.Ready, port, grace+2*time.Second) {
					e.markStopped(st, engine)
					return nil
				}
			}
			e.emitState(engine)
			if ok {
				return fmt.Errorf("cannot stop engine %q: it is running under external management (pid %d, %s); stop it in its own application, then retry", engine, pid, image)
			}
			return fmt.Errorf("cannot stop engine %q: it is running under external management; stop it in its own application, then retry", engine)
		}
		e.emitState(engine)
		return fmt.Errorf("cannot stop engine %q: no NVPAIR-owned process is available to stop", engine)
	}
	st.stopping = true
	if st.healthStop != nil {
		st.healthStop()
		st.healthStop = nil
	}
	st.mu.Unlock()

	if mode == "command" {
		sp := rt.Stop
		if sp == nil || len(sp.Cmd) == 0 {
			return e.reconcileFailedCommandStop(st, engine, true, fmt.Errorf("cannot stop engine %q: no stop command is configured", engine))
		}
		if err := e.runCommandStop(st, engine, rt, port); err != nil {
			return e.reconcileFailedCommandStop(st, engine, rt.Ready == nil || !e.waitUnavailable(rt.Ready, port, time.Second), err)
		}
	} else if proc != nil {
		proc.stop()
	}

	e.markStopped(st, engine)
	return nil
}

// runCommandStop executes a command-mode engine's bounded official stop CLI
// and confirms that its listener closes. It is shared by normal Stop and by
// failed/cancelled startup cleanup after a start CLI has detached a daemon.
func (e *Executor) runCommandStop(st *engineState, engine string, rt Runtime, port int) error {
	sp := rt.Stop
	if sp == nil || len(sp.Cmd) == 0 {
		return fmt.Errorf("cannot stop engine %q: no stop command is configured", engine)
	}
	vars := map[string]string{"port": strconv.Itoa(port), "install_dir": st.installDir}
	if rt.CLI != "" {
		vars["cli"] = expandPath(rt.CLI)
	}
	argv, err := resolveArgs(sp.Cmd, vars)
	if err != nil {
		return fmt.Errorf("resolve %s stop command: %w", engine, err)
	}
	for i := range argv {
		argv[i] = expandPath(argv[i])
	}
	// Bound the stop CLI (e.g. `lms server stop`): the broker no longer
	// force-kills engine-manager on a timeout, so an unbounded stop command
	// would wedge StopAll and, in turn, the whole app shutdown.
	stopCtx, cancelStop := context.WithTimeout(context.Background(), commandStopTimeout(rt))
	runErr := e.runCommand(stopCtx, argv)
	cancelStop()
	if runErr != nil {
		return fmt.Errorf("stop %s: %w", engine, runErr)
	}
	if rt.Ready != nil {
		grace := 5 * time.Second
		if sp.GraceS > 0 {
			grace = time.Duration(sp.GraceS) * time.Second
		}
		if !e.waitUnavailable(rt.Ready, port, grace) {
			return fmt.Errorf("stop %s: engine is still serving on port %d after %s", engine, port, grace)
		}
	}
	return nil
}

// stopGrace is how long to wait after a graceful stop before escalating to a
// forced kill, taken from the manifest's stop spec: default 5s, the spec's
// grace_s when set, or 0 when the spec asks for an immediate kill.
func stopGrace(rt Runtime) time.Duration {
	grace := 5 * time.Second
	if sp := rt.Stop; sp != nil {
		if sp.GraceS > 0 {
			grace = time.Duration(sp.GraceS) * time.Second
		}
		if strings.EqualFold(sp.Signal, "kill") {
			grace = 0
		}
	}
	return grace
}

// commandStopTimeout bounds how long a command-mode stop CLI is allowed to run
// before it is cancelled. Derived from the manifest grace with a floor, so a
// hung stop command can never wedge StopAll now that the broker no longer
// force-kills engine-manager on a timeout.
func commandStopTimeout(rt Runtime) time.Duration {
	t := stopGrace(rt)
	if t < 5*time.Second {
		t = 5 * time.Second
	}
	return t
}

func (e *Executor) markStopped(st *engineState, engine string) {
	st.mu.Lock()
	if st.healthStop != nil {
		st.healthStop()
		st.healthStop = nil
	}
	st.running = false
	st.healthy = false
	st.stopping = true
	st.adopted = false
	st.proc = nil
	st.mu.Unlock()
	// A deliberately-stopped engine has no live unhealthy/crash condition.
	e.reporter.clear(unhealthyID(engine))
	e.reporter.clear(exitedID(engine))
	e.emitState(engine)
}

// waitUnavailable confirms that an engine's listener is gone before reporting
// it stopped. Readiness and reachability are deliberately separate here: a
// live listener returning 503 is unhealthy, not stopped. A successful control
// command is likewise not proof that its daemon actually exited.
func (e *Executor) waitUnavailable(p *Probe, port int, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	misses := 0
	for {
		result := probeListener(ctx, p, port)
		if result == listenerProbeRefused {
			misses++
			if misses == unavailableConfirmations {
				return true
			}
		} else {
			// A reachable listener, timeout, or malformed/indeterminate target
			// cannot prove shutdown. Require a fresh consecutive sequence.
			misses = 0
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// probeListener reports whether the transport endpoint described by a
// readiness probe accepts connections, explicitly refuses them, or cannot be
// determined. Callers must fail closed on indeterminate results.
func probeListener(ctx context.Context, p *Probe, port int) listenerProbeResult {
	addr, err := probeListenerAddress(p, port)
	if err != nil {
		return listenerProbeIndeterminate
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err == nil {
		_ = conn.Close()
		return listenerProbeReachable
	}
	if ctx.Err() != nil {
		return listenerProbeIndeterminate
	}
	if !isConnectionRefused(err) {
		return listenerProbeIndeterminate
	}
	// Only an explicit connection refusal proves there is no listener. DNS,
	// routing, firewall, timeout, and resource errors remain indeterminate.
	return listenerProbeRefused
}

func isConnectionRefused(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	if runtime.GOOS == "windows" {
		const wsaeconnrefused syscall.Errno = 10061
		return errno == wsaeconnrefused
	}
	return errno == syscall.ECONNREFUSED
}

func probeListenerAddress(p *Probe, port int) (string, error) {
	if p == nil {
		return "", errors.New("no probe configured")
	}
	vars := map[string]string{"port": strconv.Itoa(port)}
	if p.HTTP != "" {
		raw, err := resolvePlaceholders(p.HTTP, vars)
		if err != nil {
			return "", err
		}
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return "", fmt.Errorf("invalid HTTP probe URL %q", raw)
		}
		servicePort := u.Port()
		if servicePort == "" {
			switch strings.ToLower(u.Scheme) {
			case "http":
				servicePort = "80"
			case "https":
				servicePort = "443"
			default:
				return "", fmt.Errorf("HTTP probe URL %q has no port", raw)
			}
		}
		return net.JoinHostPort(u.Hostname(), servicePort), nil
	}
	if p.TCP != "" {
		addr, err := resolvePlaceholders(p.TCP, vars)
		if err != nil {
			return "", err
		}
		if _, _, err := net.SplitHostPort(addr); err != nil {
			return "", fmt.Errorf("invalid TCP probe address %q: %w", addr, err)
		}
		return addr, nil
	}
	return "", errors.New("probe has no HTTP or TCP endpoint")
}

// reconcileFailedCommandStop keeps the in-memory state aligned with the
// readiness endpoint after a stop command fails or fails to take effect.
func (e *Executor) reconcileFailedCommandStop(st *engineState, engine string, running bool, stopErr error) error {
	if !running {
		e.markStopped(st, engine)
		slog.Warn("engine stop command returned an error, but the endpoint is confirmed down", "engine", engine, "err", stopErr)
		return nil
	}
	rt := st.plat.Runtime
	var port int
	var healthCtx context.Context
	var gen int64

	st.mu.Lock()
	port = st.port
	st.running = true
	st.healthy = true
	st.stopping = false
	if rt.Health != nil {
		st.gen++
		gen = st.gen
		var cancel context.CancelFunc
		healthCtx, cancel = context.WithCancel(context.Background())
		st.healthStop = cancel
	}
	st.mu.Unlock()

	if healthCtx != nil {
		go e.runHealth(healthCtx, st, engine, port, gen)
	}
	e.emitState(engine)
	return stopErr
}

// Restart stops then starts the engine, holding the op lock across both
// so nothing can interleave between the stop and the start.
func (e *Executor) Restart(ctx context.Context, engine string) error {
	st, err := e.state(engine)
	if err != nil {
		return err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	if err := e.doStop(st, engine); err != nil {
		return err
	}
	if err := e.doStart(ctx, st, engine, startOpts{}); err != nil {
		return err
	}
	return e.setDesiredEnabled(engine, true)
}

// StopAll rejects new starts and terminates every running engine during
// shutdown without changing the user's saved ON/OFF intent.
func (e *Executor) StopAll() {
	e.shuttingDown.Store(true)
	e.mu.Lock()
	names := make([]string, 0, len(e.engines))
	for n := range e.engines {
		names = append(names, n)
	}
	e.mu.Unlock()
	// Stop engines concurrently. Each doStop can wait up to the manifest grace
	// (default 5s) for its engine to exit, so a sequential sweep would take
	// sum(grace) across engines and let a slow first engine delay — or, if the
	// parent severs us mid-sweep, entirely prevent — every later engine from
	// even being signaled. Running them in parallel bounds the whole sweep to
	// ~one grace and guarantees every engine gets its stop signal immediately.
	// Each engine still serializes on its own opMu, so this is safe.
	var wg sync.WaitGroup
	for _, n := range names {
		st, err := e.state(n)
		if err != nil {
			continue
		}
		wg.Add(1)
		go func(st *engineState, n string) {
			defer wg.Done()
			st.mu.Lock()
			cancel := st.startCancel
			st.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			st.opMu.Lock()
			defer st.opMu.Unlock()
			if err := e.doStop(st, n); err != nil {
				slog.Warn("engine stop during shutdown failed", "engine", n, "err", err)
			}
		}(st, n)
	}
	wg.Wait()
}

func (e *Executor) runHealth(ctx context.Context, st *engineState, engine string, port int, gen int64) {
	h := st.plat.Runtime.Health
	interval := 5 * time.Second
	if h.IntervalS > 0 {
		interval = time.Duration(h.IntervalS) * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			ok := e.probe(ctx, h, port)
			st.mu.Lock()
			// Bail if stopped or if a newer start superseded this loop —
			// a stale loop must never write health for a later run.
			if !st.running || st.gen != gen {
				st.mu.Unlock()
				return
			}
			was := st.healthy
			st.healthy = ok
			st.mu.Unlock()
			switch {
			case was && !ok:
				e.reporter.report(serviceError{
					ID: unhealthyID(engine), Message: engine + " failed its health probe",
					Severity: "warning", Action: "none", EngineType: engine,
				})
				e.emitState(engine)
			case !was && ok:
				e.reporter.clear(unhealthyID(engine))
				e.emitState(engine)
			}
		}
	}
}

func (e *Executor) waitReady(ctx context.Context, p *Probe, port int) error {
	if p == nil {
		return nil
	}
	timeout := 20 * time.Second
	if p.TimeoutS > 0 {
		timeout = time.Duration(p.TimeoutS) * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		if e.probe(ctx, p, port) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("not ready after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func (e *Executor) probe(ctx context.Context, p *Probe, port int) bool {
	if p == nil {
		return true
	}
	vars := map[string]string{"port": strconv.Itoa(port)}
	if p.HTTP != "" {
		u, err := resolvePlaceholders(p.HTTP, vars)
		if err != nil {
			return false
		}
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(pctx, http.MethodGet, u, nil)
		if err != nil {
			return false
		}
		req.Header.Set(engineIdentityProbeHeader, "1")
		resp, err := e.client.Do(req)
		if err != nil {
			return false
		}
		resp.Body.Close()
		want := p.Status
		if want == 0 {
			want = 200
		}
		return resp.StatusCode == want
	}
	if p.TCP != "" {
		addr, err := resolvePlaceholders(p.TCP, vars)
		if err != nil {
			return false
		}
		pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		conn, err := (&net.Dialer{}).DialContext(pctx, "tcp", addr)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	}
	return true
}

func (e *Executor) reportStartFailed(engine string, err error) {
	e.reporter.report(serviceError{
		ID: startFailedID(engine), Message: err.Error(),
		Severity: "error", Action: "retry", EngineType: engine, Operation: "start",
	})
}

func (e *Executor) reportStartFailedUnlessShuttingDown(ctx context.Context, engine string, err error) {
	// CommandContext can surface an exit status instead of context.Canceled on
	// Windows, so the canceled operation context is the authoritative signal.
	if e.shuttingDown.Load() && ctx.Err() != nil {
		return
	}
	e.reportStartFailed(engine, err)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func fileExists(p string) bool {
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}
