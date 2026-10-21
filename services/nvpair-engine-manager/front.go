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
}

const (
	frontMaxPort       = 65535
	frontDialTimeout   = 5 * time.Second
	frontHeaderTimeout = 10 * time.Second
	frontIdleTimeout   = 90 * time.Second
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
type frontGateway struct {
	srv  *http.Server
	ln   net.Listener
	once sync.Once
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
			IdleConnTimeout:     frontIdleTimeout,
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
	g := &frontGateway{ln: ln, srv: &http.Server{
		Handler:           newFrontHandler(backend),
		ReadHeaderTimeout: frontHeaderTimeout,
		IdleTimeout:       frontIdleTimeout,
		ErrorLog:          log.New(io.Discard, "", 0),
	}}
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
		_ = g.srv.Close()
		// Closed here as well: Server.Close only closes a listener Serve has begun
		// to track, and a restart needs the port free the moment close returns.
		_ = g.ln.Close()
	})
}

// closeWhen closes the gateway when the engine process exits, however that
// happens, so a crashed engine does not leave a gateway answering for it.
func (g *frontGateway) closeWhen(done <-chan struct{}) {
	go func() {
		<-done
		g.close()
	}()
}

// startFront prepares and starts the gateway for an engine about to be spawned:
// it clears a stale engine on the backend port, then opens the public port. The
// caller starts the engine afterwards, and closes the gateway if that fails.
func (e *Executor) startFront(st *engineState, engine string, rt Runtime, host string, public int) (*frontGateway, error) {
	backend, err := frontBackendPort(rt.Front, public)
	if err != nil {
		return nil, err
	}
	if err := e.reclaimFrontBackend(st, engine, backend); err != nil {
		return nil, err
	}
	g, err := startFrontGateway(host, public, backend)
	if err != nil {
		return nil, err
	}
	st.mu.Lock()
	previous := st.front
	st.front = g
	st.mu.Unlock()
	previous.close()
	return g, nil
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
