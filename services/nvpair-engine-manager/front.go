// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	settings "nvpair-shared/enginesettings"
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
	// wakeFn loads the engine again, and wakeSt is the load in progress, if any.
	// Requests that arrive meanwhile wait on it.
	wakeFn func(context.Context) error
	wakeSt *wakeState
	// model is the model the engine serves, so that the model list can be
	// answered while the engine is unloaded, and so that a request naming another
	// model is known to need a switch.
	model string
	// switchFn changes the served model, and swSt is the switch in progress, if
	// any. Every request that needs the model waits on it.
	switchFn func(ctx context.Context, model string) error
	swSt     *switchState
	// modelsFn lists the models the engine can be switched to. The gateway adds
	// them to its model list, since the engine itself only names the one it serves.
	modelsFn func(ctx context.Context) []string
}

// switchState is one change of the served model. Every request that needs the
// engine while it runs waits on done, so any number of them share a single
// switch.
type switchState struct {
	model string
	done  chan struct{}
	err   error
}

// errModelNotDownloaded marks a request for a model the engine cannot serve
// because it is not in the cache. Nothing is switched or unloaded for it.
var errModelNotDownloaded = errors.New("the model is not downloaded on this node")

// frontPeekLimit bounds how much of a request body is read to find its model.
const frontPeekLimit = 32 << 20

// wakeState is one load of an unloaded engine. Every request that needs the
// engine waits on done, so any number of them share a single start.
type wakeState struct {
	done chan struct{}
	err  error
}

// wakePaths are the non-/v1 paths that run inference.
var wakePaths = map[string]bool{"/tokenize": true, "/detokenize": true, "/pooling": true, "/score": true, "/rerank": true, "/classify": true}

// wakeable reports whether a request needs the model and so should load it.
// Health checks, metrics and reads must not: they are made on a timer, and would
// keep the model loaded or load it for nothing.
func wakeable(r *http.Request) bool {
	return r.Method == http.MethodPost && (strings.HasPrefix(r.URL.Path, "/v1/") || wakePaths[r.URL.Path])
}

// requestWake returns the load in progress, starting one if the engine is
// unloaded. It returns nil when the engine is neither, so the caller can carry
// on. The gateway forwards again from the moment a load begins, so that the
// start's own readiness probes, made on the public port, reach the engine and not
// the idle answer.
func (g *frontGateway) requestWake() *wakeState {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.wakeSt != nil {
		return g.wakeSt
	}
	if !g.idle || g.wakeFn == nil {
		return nil
	}
	ws := &wakeState{done: make(chan struct{})}
	g.wakeSt = ws
	g.idle, g.up = false, false
	go func() { g.finishWake(ws, g.wakeFn(context.Background())) }()
	return ws
}

// finishWake ends a load. A failed one puts the gateway back to idle, so the next
// request tries again.
func (g *frontGateway) finishWake(ws *wakeState, err error) {
	g.mu.Lock()
	ws.err = err
	if err != nil {
		g.idle, g.up = true, false
	}
	if g.wakeSt == ws {
		g.wakeSt = nil
	}
	g.mu.Unlock()
	close(ws.done)
}

// requestSwitch returns the switch in progress, starting one if the request names
// a model other than the one served. It returns nil when no switch is needed, so
// the caller can carry on. The switch drains the requests already running on the
// current model before it unloads it, and a request that arrives meanwhile waits
// for it, so no request is cut off and none is answered by the wrong model.
func (g *frontGateway) requestSwitch(model string) *switchState {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.swSt != nil {
		return g.swSt
	}
	if model == "" || g.model == "" || model == g.model || g.switchFn == nil {
		return nil
	}
	ss := &switchState{model: model, done: make(chan struct{})}
	g.swSt = ss
	go func() { g.finishSwitch(ss, g.switchFn(context.Background(), model)) }()
	return ss
}

// finishSwitch ends a switch. Whatever its outcome, the next request that needs
// the engine decides what happens next: a failed switch leaves the engine
// unloaded, it does not put the previous model back.
func (g *frontGateway) finishSwitch(ss *switchState, err error) {
	g.mu.Lock()
	ss.err = err
	if g.swSt == ss {
		g.swSt = nil
	}
	g.mu.Unlock()
	close(ss.done)
}

// listedModels is what the gateway adds to the engine's model list: the models
// a request can switch to.
func (g *frontGateway) listedModels(ctx context.Context) []string {
	if g.modelsFn == nil {
		return nil
	}
	return g.modelsFn(ctx)
}

// residentOnly reports whether a model list request asks for only the model the
// engine has loaded (?resident=1), which is how the manager reads what is
// resident, as against the list that clients and the proxy read.
func residentOnly(r *http.Request) bool {
	return r.URL.Query().Has("resident")
}

// withListedModels adds to an OpenAI-style model list body any of ids it does not
// already name. A body that is not such a list is returned as it is.
func withListedModels(body []byte, ids []string) []byte {
	var list map[string]json.RawMessage
	var data []json.RawMessage
	if json.Unmarshal(body, &list) != nil || json.Unmarshal(list["data"], &data) != nil {
		return body
	}
	named := map[string]bool{}
	for _, entry := range data {
		var model struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(entry, &model) == nil {
			named[model.ID] = true
		}
	}
	for _, id := range ids {
		if named[id] {
			continue
		}
		named[id] = true
		entry, _ := json.Marshal(map[string]any{"id": id, "object": "model", "created": 0, "owned_by": "vllm"})
		data = append(data, entry)
	}
	list["data"], _ = json.Marshal(data)
	merged, err := json.Marshal(list)
	if err != nil {
		return body
	}
	return merged
}

// setModel records the model the engine serves.
func (g *frontGateway) setModel(model string) {
	g.mu.Lock()
	g.model = model
	g.mu.Unlock()
}

// drain returns once no inference request is running, which is when the model
// can be unloaded without cutting one off. Requests that need the model are held
// while a switch is in progress, so nothing new starts meanwhile.
func (g *frontGateway) drain(ctx context.Context) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		g.mu.Lock()
		running := g.inflight
		g.mu.Unlock()
		if running == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("requests on the current model did not finish: %w", ctx.Err())
		case <-g.closed:
			return errors.New("the engine was stopped")
		case <-ticker.C:
		}
	}
}

// requestedModel returns the model a JSON inference request names, or "" when it
// names none or is not JSON. It reads only as far as the model field and puts the
// bytes back, so the engine receives the body unchanged.
func requestedModel(r *http.Request) string {
	if !wakeable(r) || r.Body == nil || !strings.Contains(r.Header.Get("Content-Type"), "json") {
		return ""
	}
	var seen bytes.Buffer
	model := peekModel(json.NewDecoder(io.TeeReader(io.LimitReader(r.Body, frontPeekLimit), &seen)))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(&seen, r.Body), r.Body}
	return model
}

// peekModel reads a JSON object up to its top-level "model" string.
func peekModel(dec *json.Decoder) string {
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	for dec.More() {
		tok, err := dec.Token()
		key, isString := tok.(string)
		if err != nil || !isString {
			return ""
		}
		if key == "model" {
			var model string
			if dec.Decode(&model) != nil {
				return ""
			}
			return model
		}
		var skipped json.RawMessage
		if dec.Decode(&skipped) != nil {
			return ""
		}
	}
	return ""
}

// shortReason flattens an error to one line short enough for a response.
func shortReason(err error) string {
	reason := strings.Join(strings.Fields(err.Error()), " ")
	if len(reason) > 400 {
		reason = reason[:400] + "..."
	}
	return reason
}

// unavailable answers a request the engine cannot serve right now.
func unavailable(w http.ResponseWriter, status int, message string) {
	body, _ := json.Marshal(map[string]string{"error": message})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// loadFailed answers a request whose model could not be loaded.
func loadFailed(w http.ResponseWriter, err error) {
	unavailable(w, http.StatusServiceUnavailable, "the model could not be loaded: "+shortReason(err))
}

// switchFailed answers a request whose model the engine could not be switched to.
// The engine is left unloaded, and the next request tries again.
func switchFailed(w http.ResponseWriter, model string, err error) {
	if errors.Is(err, errModelNotDownloaded) {
		unavailable(w, http.StatusNotFound, fmt.Sprintf("the model %q is not downloaded on this node", model))
		return
	}
	unavailable(w, http.StatusServiceUnavailable, fmt.Sprintf("could not switch to model %q: %s. The engine is unloaded and will try again on the next request.", model, shortReason(err)))
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
	requested := requestedModel(r)
	for {
		if wakeable(r) {
			if ss := g.requestSwitch(requested); ss != nil {
				select {
				case <-ss.done:
					if ss.err != nil && ss.model == requested {
						switchFailed(w, requested, ss.err)
						return
					}
					continue
				case <-r.Context().Done():
					return // the client left; the switch carries on for the next one
				case <-g.closed:
					return
				}
			}
		}
		g.mu.Lock()
		if (g.idle || g.wakeSt != nil) && wakeable(r) {
			g.mu.Unlock()
			ws := g.requestWake()
			if ws == nil {
				continue // it loaded between the check and now
			}
			select {
			case <-ws.done:
				if ws.err != nil {
					loadFailed(w, ws.err)
					return
				}
				continue
			case <-r.Context().Done():
				return // the client left; the load carries on for the next one
			case <-g.closed:
				return
			}
		}
		if g.idle || (g.wakeSt != nil && r.Method == http.MethodGet && r.URL.Path == "/v1/models") {
			model := g.model
			g.mu.Unlock()
			if r.Method == http.MethodGet && r.URL.Path == "/v1/models" && model != "" {
				// The proxy builds its model list from this call, and an engine that is
				// only unloaded still answers for its model and every one it can load.
				body, _ := json.Marshal(map[string]any{"object": "list", "data": []map[string]any{
					{"id": model, "object": "model", "created": 0, "owned_by": "vllm"},
				}})
				if !residentOnly(r) {
					body = withListedModels(body, g.listedModels(r.Context()))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
				return
			}
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
		break
	}
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

func newFrontHandler(backendPort int, listed func(context.Context) []string) http.Handler {
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
		// The engine's model list names only the model it serves; the gateway adds
		// the ones a request can switch to.
		ModifyResponse: func(resp *http.Response) error {
			if listed == nil || resp.StatusCode != http.StatusOK || resp.Request.Method != http.MethodGet || resp.Request.URL.Path != "/v1/models" || residentOnly(resp.Request) {
				return nil
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, frontPeekLimit))
			_ = resp.Body.Close()
			if err != nil {
				return err
			}
			body = withListedModels(body, listed(resp.Request.Context()))
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
			return nil
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
	g := &frontGateway{ln: ln, closed: make(chan struct{})}
	g.proxy = newFrontHandler(backend, g.listedModels)
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
	g.wakeFn = func(ctx context.Context) error { return e.wakeEngine(ctx, engine) }
	g.switchFn = func(ctx context.Context, model string) error { return e.switchFrontModel(ctx, st, engine, g, model) }
	g.modelsFn = func(ctx context.Context) []string {
		models, _ := e.cachedModels(ctx, engine)
		return models
	}
	st.mu.Lock()
	previous := st.front
	st.front = g
	st.mu.Unlock()
	previous.close()
	go e.watchIdle(st, engine, g)
	return g, false, nil
}

// frontIdleTimeout is the engine's current idle limit; zero means never. A limit
// chosen at run time wins over the manifest's.
func frontIdleTimeout(st *engineState) time.Duration {
	if st.idleSet.Load() {
		return time.Duration(st.idleSecs.Load()) * time.Second
	}
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
	if !ok {
		return false
	}
	// Read while this holds the lifecycle lock: the engine goes on advertising
	// this model, and nothing may read the launch settings during the unload.
	model, _ := e.servedModelLocked(engine, st)
	g.mu.Lock()
	g.model = model
	g.mu.Unlock()
	// The state says idle before the gateway does, so a reader of the inventory
	// never sees an engine that is neither loaded nor idle and drops its model.
	st.mu.Lock()
	st.idle = true
	st.idleModel = model
	st.mu.Unlock()
	if !g.enterIdle(timeout, force) {
		st.mu.Lock()
		st.idle = false
		st.idleModel = ""
		st.mu.Unlock()
		return false
	}
	st.mu.Lock()
	// The process is released from the state before it is stopped, so its exit is
	// not taken for a crash.
	st.proc = nil
	st.mu.Unlock()
	slog.Info("unloading an idle engine", "engine", engine, "forced", force)
	e.emitState(engine)
	proc.stop(e.stopGraceFor(st.plat.Runtime))
	return true
}

// wakeIdle loads an engine that was unloaded for being idle, and returns once it
// is serving. It shares the load with any request already waiting for it.
func (e *Executor) wakeIdle(ctx context.Context, engine string) error {
	st, err := e.state(engine)
	if err != nil {
		return err
	}
	st.mu.Lock()
	g := st.front
	st.mu.Unlock()
	if g == nil || !g.alive() {
		return nil
	}
	ws := g.requestWake()
	if ws == nil {
		return nil
	}
	select {
	case <-ws.done:
		return ws.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// wakeEngine is the load itself: it starts the engine's process behind the
// gateway that is already in place. A failed start leaves the engine idle, not
// stopped, so the next request can try again. The load is bounded by the engine's
// readiness limit, and not by the request that asked for it, so a client leaving
// does not abandon it.
func (e *Executor) wakeEngine(ctx context.Context, engine string) error {
	st, err := e.state(engine)
	if err != nil {
		return err
	}
	st.opMu.Lock()
	defer st.opMu.Unlock()
	st.mu.Lock()
	if !st.idle || st.front == nil {
		st.mu.Unlock()
		return nil
	}
	st.mu.Unlock()
	limit := 10 * time.Minute
	if ready := st.plat.Runtime.Ready; ready != nil && ready.TimeoutS > 0 {
		limit = time.Duration(ready.TimeoutS+60) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	// The engine stays marked running and idle throughout, so the status and the
	// model inventory do not flicker while it loads.
	if err := e.doStart(ctx, st, engine, startOpts{Wake: true}); err != nil {
		st.mu.Lock()
		st.stopping = false
		st.mu.Unlock()
		e.emitState(engine)
		return err
	}
	st.mu.Lock()
	st.idle = false
	st.idleModel = ""
	st.mu.Unlock()
	e.emitState(engine)
	return nil
}

// switchFrontModel makes the engine serve another model, the way Ollama and
// llama.cpp swap one in for a request: requests already running on the current
// model finish, the model is unloaded, and the new one is loaded behind the same
// gateway, which holds the requests that need it meanwhile. A model that is not
// downloaded is refused before anything changes. A load that fails is not rolled
// back: the engine is left unloaded, still on, and the next request loads again.
func (e *Executor) switchFrontModel(ctx context.Context, st *engineState, engine string, g *frontGateway, model string) error {
	if err := e.requireCachedModel(ctx, engine, model); err != nil {
		return err
	}
	limit := 10 * time.Minute
	if ready := st.plat.Runtime.Ready; ready != nil && ready.TimeoutS > 0 {
		limit = time.Duration(ready.TimeoutS+60) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	if err := g.drain(ctx); err != nil {
		return err
	}
	e.unloadFront(st, engine, g, 0, true)
	if err := e.setIdleModel(st, engine, g, model); err != nil {
		return err
	}
	slog.Info("switching the served model", "engine", engine, "model", model)
	return e.wakeIdle(ctx, engine)
}

// cachedModels lists the models the engine has a complete download of.
func (e *Executor) cachedModels(ctx context.Context, engine string) ([]string, error) {
	st, err := e.state(engine)
	if err != nil {
		return nil, err
	}
	act, ok := st.manifest.Actions["list_models"]
	if !ok || act.Result == nil {
		return nil, fmt.Errorf("engine %q cannot list its models", engine)
	}
	raw, err := e.Action(ctx, engine, "list_models", nil)
	if err != nil {
		return nil, err
	}
	models, _ := extractStringsResult(raw, act.Result)
	return models, nil
}

// requireCachedModel refuses a model the engine has no complete download of.
func (e *Executor) requireCachedModel(ctx context.Context, engine, model string) error {
	if !hfModelIDRe.MatchString(model) {
		return fmt.Errorf("%w: %q is not a model id", errModelNotDownloaded, model)
	}
	models, err := e.cachedModels(ctx, engine)
	if err != nil {
		return err
	}
	if !slices.Contains(models, model) {
		return fmt.Errorf("%w: %s", errModelNotDownloaded, model)
	}
	return nil
}

// setIdleModel saves model as the one the unloaded engine serves next, and
// advertises it, without starting the engine.
func (e *Executor) setIdleModel(st *engineState, engine string, g *frontGateway, model string) error {
	st.opMu.Lock()
	defer st.opMu.Unlock()
	current := e.launchStateLocked(engine, st)
	text, changed, err := withLaunchFlag(current.LaunchText, "--model", model)
	if err != nil {
		return err
	}
	if changed {
		preview := e.previewLaunchLocked(st, settings.Request{
			Engine:   engine,
			Settings: settings.Config{ServerPort: current.ServerPort, LaunchText: text},
		})
		if len(preview.Errors) > 0 || preview.Conflict != nil {
			return fmt.Errorf("the launch arguments do not accept model %s", model)
		}
		if err := e.commitLaunchLocked(st, engine, preview, true); err != nil {
			return err
		}
	}
	st.mu.Lock()
	st.idleModel = model
	st.mu.Unlock()
	g.setModel(model)
	e.emitState(engine)
	return nil
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

// servedModelLocked is the model the engine's launch names. The caller holds the
// lifecycle lock, which the launch settings are read under.
func (e *Executor) servedModelLocked(engine string, st *engineState) (string, error) {
	state := e.launchStateLocked(engine, st)
	tokens, err := parseLaunchText(state.LaunchText)
	if err != nil {
		return "", err
	}
	model, ok := launchFlagValue(tokens, "--model")
	if !ok || model == "" {
		return "", fmt.Errorf("the launch arguments name no --model")
	}
	return model, nil
}

// idleModelOf is the model an unloaded engine goes on advertising.
func (e *Executor) idleModelOf(engine string) string {
	st, err := e.state(engine)
	if err != nil {
		return ""
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.idleModel
}

// enterLazy brings an engine with a front up idle: the gateway is listening and
// answering, the engine is ON, and nothing is loaded until a request or a Load
// asks for the model. Only the cheap checks a start can make without loading
// run: the launch must resolve and pass its validation, and the ports must be
// free.
func (e *Executor) enterLazy(st *engineState, engine string, rt Runtime, vars map[string]string, port int) error {
	st.mu.Lock()
	binPath := st.binPath
	st.mu.Unlock()
	launch, err := resolveProcessLaunch(rt, binPath, vars)
	if err != nil {
		return err
	}
	if err := validateEffectiveLaunch(rt, launch, vars["host"], vars["port"]); err != nil {
		return err
	}
	model, _ := e.servedModelLocked(engine, st)
	g, _, err := e.startFront(st, engine, rt, vars["host"], port)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.idle, g.up, g.model = true, false, model
	g.mu.Unlock()
	st.mu.Lock()
	st.running, st.healthy, st.adopted = true, true, false
	st.idle, st.idleModel = true, model
	st.proc = nil
	st.gen++
	st.mu.Unlock()
	e.reporter.clear(startFailedID(engine))
	e.reporter.clear(exitedID(engine))
	e.reporter.clear(unhealthyID(engine))
	e.emitState(engine)
	return nil
}

// frontIdleAction reads or changes how long the engine may sit idle before it is
// unloaded. A change applies at once, is saved so it survives a restart, and 0
// means never.
func (e *Executor) frontIdleAction(engine, mode string, params json.RawMessage) (json.RawMessage, error) {
	st, err := e.state(engine)
	if err != nil {
		return nil, err
	}
	if st.plat.Runtime.Front == nil {
		return nil, fmt.Errorf("engine %q has no idle timeout", engine)
	}
	if mode == "set" {
		var request struct {
			Seconds *int `json:"seconds"`
		}
		if err := json.Unmarshal(params, &request); err != nil || request.Seconds == nil {
			return nil, errors.New(`params must be {"seconds": <0 to 86400>}`)
		}
		if *request.Seconds < 0 || *request.Seconds > 86400 {
			return nil, fmt.Errorf("seconds must be between 0 and 86400, got %d", *request.Seconds)
		}
		if err := e.persistFrontIdle(engine, *request.Seconds); err != nil {
			return nil, err
		}
		st.idleSecs.Store(int64(*request.Seconds))
		st.idleSet.Store(true)
	}
	return json.Marshal(map[string]any{"seconds": int(frontIdleTimeout(st) / time.Second)})
}

// persistFrontIdle saves the idle limit in the engine's override file, next to
// the port and launch the user has set there.
func (e *Executor) persistFrontIdle(engine string, seconds int) error {
	if e.overrideDir == "" {
		return errors.New("no config directory is available to save the idle timeout")
	}
	if err := os.MkdirAll(e.overrideDir, 0o700); err != nil {
		return fmt.Errorf("create override dir: %w", err)
	}
	path := filepath.Join(e.overrideDir, engine+".json")
	m := map[string]any{"engine": engine}
	if existing, err := os.ReadFile(path); err == nil {
		decoder := json.NewDecoder(bytes.NewReader(existing))
		decoder.UseNumber() // unrelated manifest numbers must not lose precision
		m = nil
		if err := decoder.Decode(&m); err != nil || m == nil {
			return errors.New("override must be a JSON object")
		}
		if name, ok := m["engine"].(string); !ok || name != engine {
			return fmt.Errorf("override engine does not match %q", engine)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read override: %w", err)
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
	front, err := overrideObject(hostRuntime, "front")
	if err != nil {
		return err
	}
	front["idle_timeout_s"] = seconds
	hostRuntime["front"] = front
	setOverrideObject(platform, "runtime", hostRuntime)
	setOverrideObject(platforms, host, platform)
	setOverrideObject(m, "platforms", platforms)
	return writeJSONAtomic(path, m)
}
