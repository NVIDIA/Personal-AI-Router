// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Front puts a PAIR-owned HTTP gateway on an engine's public port and runs the
// engine itself on a loopback port derived from it. Clients, the proxy and every
// probe keep using the public port, so nothing outside this file sees the
// difference; the engine's process is what is behind it. Only an engine whose
// manifest declares it gets this; every other engine is untouched.
type Front struct {
	// BackendPortOffset is added to the public port to get the engine's own
	// port, so the port is a fixed function of the public one. A stale engine from
	// a crashed run can then be found and reclaimed, which a random port would
	// prevent.
	BackendPortOffset int `json:"backend_port_offset"`
	// IdleTimeoutS unloads the engine, stopping its process while the gateway and
	// the engine's ON state stay, after this many seconds with no inference
	// request. Zero never unloads.
	IdleTimeoutS int `json:"idle_timeout_s,omitempty"`
}

const (
	frontMaxPort         = 65535
	frontDialTimeout     = 5 * time.Second
	frontHeaderTimeout   = 10 * time.Second
	frontConnIdleTimeout = 90 * time.Second
)

// frontBackendPort is the port the engine itself listens on for a public port.
func frontBackendPort(f *Front, public int) (int, error) {
	backend := public + f.BackendPortOffset
	if backend > frontMaxPort {
		backend = public - f.BackendPortOffset
	}
	if backend < 1024 || backend > frontMaxPort || backend == public {
		return 0, fmt.Errorf("no usable backend port for public port %d with offset %d", public, f.BackendPortOffset)
	}
	return backend, nil
}

// frontGateway forwards every request to the engine on its backend port and
// streams the response back. It adds, removes and logs nothing: it is a
// transparent hop, and request or response bodies never pass through a log.
//
// It also keeps the one fact idle unload needs: when the engine last did
// inference work, and whether any is in flight.
type frontGateway struct {
	srv    *http.Server
	ln     net.Listener
	proxy  http.Handler
	once   sync.Once
	closed chan struct{}

	mu         sync.Mutex
	inflight   int
	lastActive time.Time
	// up is true while the engine behind the gateway is serving.
	up bool
	// idle is true while the engine has been unloaded for being idle. The
	// gateway stays, answering health checks, and refuses inference.
	idle bool
	// epoch identifies the engine process the gateway currently belongs to, so
	// the exit of an earlier process, stopped on purpose, does not close it.
	epoch int
}

// isActivity reports whether a request is inference work. Reads such as health
// checks, metrics and the model list are made by the proxy and by monitors on a
// timer, and must not keep an engine loaded.
func isActivity(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

func (g *frontGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	activity := isActivity(r)
	g.mu.Lock()
	if g.idle {
		g.mu.Unlock()
		if r.URL.Path == "/health" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"idle"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"the model is unloaded because the engine was idle"}`))
		return
	}
	if activity {
		g.inflight++
		g.lastActive = time.Now()
	}
	g.mu.Unlock()
	if activity {
		defer func() {
			g.mu.Lock()
			g.inflight--
			g.lastActive = time.Now()
			g.mu.Unlock()
		}()
	}
	g.proxy.ServeHTTP(w, r)
}

// markUp records that the engine behind the gateway is serving, and starts its
// idle clock.
func (g *frontGateway) markUp() {
	g.mu.Lock()
	g.up, g.idle = true, false
	g.lastActive = time.Now()
	g.mu.Unlock()
}

// expired reports whether the engine has been idle for timeout.
func (g *frontGateway) expired(timeout time.Duration) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.up && !g.idle && g.inflight == 0 && time.Since(g.lastActive) >= timeout
}

// enterIdle flips the gateway to idle, atomically with the check that no request
// is in flight, so a request cannot reach an engine that is being stopped. force
// skips the idle-time and in-flight conditions, for an explicit eject.
func (g *frontGateway) enterIdle(timeout time.Duration, force bool) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.up || g.idle {
		return false
	}
	if !force && (g.inflight > 0 || time.Since(g.lastActive) < timeout) {
		return false
	}
	g.up, g.idle = false, true
	g.epoch++ // the process about to be stopped must not take the gateway with it
	return true
}

func (g *frontGateway) alive() bool {
	select {
	case <-g.closed:
		return false
	default:
		return true
	}
}

func newFrontHandler(backendPort int) http.Handler {
	target := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(backendPort))}
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
		},
		// Flush each write as it arrives: completions stream as server-sent events.
		FlushInterval: -1,
		Transport: &http.Transport{
			// The environment's proxy settings must never apply to a loopback hop.
			Proxy:               nil,
			DialContext:         (&net.Dialer{Timeout: frontDialTimeout}).DialContext,
			DisableCompression:  true,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     frontConnIdleTimeout,
		},
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, context.Canceled) {
				return // the client left; there is nobody to answer
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"the engine is not ready"}`))
		},
	}
}

func startFrontGateway(host string, public, backend int) (*frontGateway, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(public)))
	if err != nil {
		return nil, fmt.Errorf("listen on port %d: %w", public, err)
	}
	g := &frontGateway{ln: ln, proxy: newFrontHandler(backend), closed: make(chan struct{})}
	g.srv = &http.Server{
		Handler:           g,
		ReadHeaderTimeout: frontHeaderTimeout,
		IdleTimeout:       frontConnIdleTimeout,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() { _ = g.srv.Serve(ln) }()
	return g, nil
}

// close stops the gateway and drops its connections, as killing the engine does.
// It is safe to call more than once and on a nil gateway.
func (g *frontGateway) close() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		close(g.closed)
		_ = g.srv.Close()
		// Closed here as well: Server.Close only closes a listener Serve has begun
		// to track, and a restart needs the port free the moment close returns.
		_ = g.ln.Close()
	})
}

// closeWhen closes the gateway when the engine process exits, however that
// happens, so a crashed engine does not leave a gateway answering for it. A
// process stopped for being idle is not a crash: that stop moves the epoch on,
// and the gateway stays.
func (g *frontGateway) closeWhen(done <-chan struct{}) {
	g.mu.Lock()
	g.epoch++
	mine := g.epoch
	g.mu.Unlock()
	go func() {
		<-done
		g.mu.Lock()
		current := g.epoch == mine
		g.mu.Unlock()
		if current {
			g.close()
		}
	}()
}

// startFront prepares the gateway for an engine about to be spawned. A gateway
// left by an idle unload is reused, since the engine is only being woken;
// otherwise it clears a stale engine on the backend port and opens the public
// port. The caller starts the engine afterwards, and closes a new gateway if that
// fails. reused reports which case it was.
func (e *Executor) startFront(st *engineState, engine string, rt Runtime, host string, public int) (g *frontGateway, reused bool, err error) {
	st.mu.Lock()
	existing := st.front
	st.mu.Unlock()
	if existing != nil && existing.alive() {
		return existing, true, nil
	}
	backend, err := frontBackendPort(rt.Front, public)
	if err != nil {
		return nil, false, err
	}
	if err := e.reclaimFrontBackend(st, engine, backend); err != nil {
		return nil, false, err
	}
	g, err = startFrontGateway(host, public, backend)
	if err != nil {
		return nil, false, err
	}
	st.mu.Lock()
	previous := st.front
	st.front = g
	st.mu.Unlock()
	previous.close()
	go e.watchIdle(st, engine, g)
	return g, false, nil
}

// frontIdleTimeout is the engine's current idle limit; zero means never.
func frontIdleTimeout(st *engineState) time.Duration {
	if front := st.plat.Runtime.Front; front != nil && front.IdleTimeoutS > 0 {
		return time.Duration(front.IdleTimeoutS) * time.Second
	}
	return 0
}

// watchIdle unloads the engine once it has been idle for its limit. It runs for
// the life of the gateway, across unloads and wakes.
func (e *Executor) watchIdle(st *engineState, engine string, g *frontGateway) {
	interval := time.Second
	if timeout := frontIdleTimeout(st); timeout > 0 && timeout/4 < interval {
		interval = max(timeout/4, 50*time.Millisecond)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-g.closed:
			return
		case <-ticker.C:
			if timeout := frontIdleTimeout(st); timeout > 0 && g.expired(timeout) {
				e.unloadFront(st, engine, g, timeout, false)
			}
		}
	}
}

// unloadFront stops the engine's process and leaves the gateway, and the
// engine's ON state, in place. It never waits for another lifecycle operation: a
// start, stop or switch in progress wins, and the next tick tries again. force
// ejects regardless of the idle time or requests in flight.
func (e *Executor) unloadFront(st *engineState, engine string, g *frontGateway, timeout time.Duration, force bool) bool {
	if force {
		st.opMu.Lock()
	} else if !st.opMu.TryLock() {
		return false
	}
	defer st.opMu.Unlock()
	st.mu.Lock()
	proc := st.proc
	ok := st.running && !st.stopping && !st.idle && st.front == g && proc != nil
	st.mu.Unlock()
	if !ok || !g.enterIdle(timeout, force) {
		return false
	}
	st.mu.Lock()
	// The process is released from the state before it is stopped, so its exit is
	// not taken for a crash.
	st.proc = nil
	st.idle = true
	st.mu.Unlock()
	slog.Info("unloading an idle engine", "engine", engine, "forced", force)
	e.emitState(engine)
	proc.stop(e.stopGraceFor(st.plat.Runtime))
	return true
}

// wakeIdle starts the engine again behind its gateway after an idle unload. A
// failed start leaves it idle, not stopped, so the next request can try again.
func (e *Executor) wakeIdle(ctx context.Context, engine string) error {
	st, err := e.state(engine)
	if err != nil {
		return err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	st.mu.Lock()
	idle, g := st.idle, st.front
	if !idle || g == nil {
		st.mu.Unlock()
		return nil
	}
	// doStart does nothing for an engine marked running, so it is marked stopped
	// for the duration of the start.
	st.running, st.healthy, st.idle = false, false, false
	st.mu.Unlock()
	// The gateway forwards again from here, so that the start's readiness probes,
	// made on the public port, reach the engine and not the idle answer.
	g.mu.Lock()
	g.idle, g.up = false, false
	g.mu.Unlock()
	err = e.doStart(ctx, st, engine, startOpts{})
	if err != nil {
		st.mu.Lock()
		if st.front != nil && st.front.alive() {
			st.running, st.healthy, st.idle = true, true, true
			st.front.mu.Lock()
			st.front.up, st.front.idle = false, true
			st.front.mu.Unlock()
		}
		st.mu.Unlock()
		e.emitState(engine)
	}
	return err
}

// reclaimFrontBackend frees the backend port when it holds an engine that PAIR
// started in an earlier run and lost, as doStop does for the public port. Anything
// else on the port is not ours to touch, and is reported.
func (e *Executor) reclaimFrontBackend(st *engineState, engine string, backend int) error {
	if ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(backend))); err == nil {
		_ = ln.Close()
		return nil
	}
	st.mu.Lock()
	binPath := st.binPath
	st.mu.Unlock()
	pid, image, ok := pidOnPort(backend)
	if !ok || !isManagedInstallPath(binPath, st.installDir) || !(isOurEngineImage(image, binPath) || procRunsScript(pid, binPath)) {
		return fmt.Errorf("cannot start engine %q: its internal port %d is in use by another program", engine, backend)
	}
	slog.Info("reclaiming a stale engine on its internal port", "engine", engine, "port", backend, "pid", pid)
	terminatePID(pid, e.stopGraceFor(st.plat.Runtime))
	if ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(backend))); err == nil {
		_ = ln.Close()
		return nil
	}
	return fmt.Errorf("cannot start engine %q: its internal port %d is still in use after stopping a stale engine", engine, backend)
}

// frontBackendValue renders a networking control's value for the engine's own
// listener: loopback and the backend port. A control that carries anything else,
// such as CORS origins, is not about where the engine listens and is left alone.
func frontBackendValue(template string, backend int) (string, bool) {
	value := strings.ReplaceAll(template, "{server.host}", "127.0.0.1")
	value = strings.ReplaceAll(value, "{server.port}", strconv.Itoa(backend))
	if value == template || strings.Contains(value, "{") {
		return "", false
	}
	return value, true
}

// applyFrontBackend points the engine's launch at its backend port. The launch
// the user sees and validates keeps the public port; only the process that is
// spawned listens elsewhere, on loopback. It is a no-op for an engine with no
// front.
func applyFrontBackend(rt Runtime, command launchCommand, public int) (launchCommand, error) {
	if rt.Front == nil || rt.EditableLaunch == nil {
		return command, nil
	}
	backend, err := frontBackendPort(rt.Front, public)
	if err != nil {
		return command, err
	}
	args := append([]string(nil), command.Args...)
	seen := map[*LaunchControl]bool{}
	policy := rt.EditableLaunch
	for i := len(policy.FixedArgs); i < len(args); i++ {
		control, _, last, err := policy.readControl(args, i)
		if err != nil {
			return command, err
		}
		if control == nil {
			continue
		}
		value, ok := frontBackendValue(control.Value, backend)
		if !ok || len(control.Flags) == 0 {
			i = last
			continue
		}
		seen[control] = true
		token := args[i]
		switch {
		case last > i: // "--flag value"
			args[last] = value
		case strings.Contains(token, "="): // "--flag=value"
			args[i] = token[:strings.Index(token, "=")+1] + value
		default: // "-pVALUE"
			args[i] = token[:2] + value
		}
		i = last
	}
	for ci := range policy.Controls {
		control := &policy.Controls[ci]
		if seen[control] || len(control.Flags) == 0 {
			continue
		}
		if value, ok := frontBackendValue(control.Value, backend); ok {
			args = append(args, control.Flags[0], value)
		}
	}
	command.Args = args
	return command, nil
}

// presenceForStart is reconcilePresence for a start, except that a gateway PAIR
// itself left on the port, while the engine behind it is unloaded, is not another
// engine to adopt or to refuse.
func (e *Executor) presenceForStart(ctx context.Context, engine string, st *engineState, pathInstalled bool, port int) presenceResult {
	st.mu.Lock()
	ours := st.front != nil && st.front.alive()
	st.mu.Unlock()
	if ours {
		return presenceResult{}
	}
	return e.reconcilePresence(ctx, engine, st, pathInstalled, port, true)
}
