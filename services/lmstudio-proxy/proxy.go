// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/cors"
	"nvpair-shared/errors"
	"nvpair-shared/netmon"
	"nvpair-shared/netpick"
	"nvpair-shared/nodeactivity"
	"nvpair-shared/noderec"
	"nvpair-shared/reach"
	"nvpair-shared/schedulerwire"
	"nvpair-shared/splitlisten"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
// See versions.json at the repo root for the source of truth.
var Version = "dev"

type ReadyParams struct {
	Version string `json:"version"`
	Port    int    `json:"port"`
}

// ErrorParams is sent as a JSON-RPC "error" notification when the
// proxy encounters a fatal startup-time condition it wants to surface
// to the orchestrator before exiting. Code is a short machine-readable
// tag ("bind-failed" today); Message is a human-friendly string suitable
// for an error bar.
type ErrorParams struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Port    int    `json:"port,omitempty"`
}

type NodesResult struct {
	Nodes []Node `json:"nodes"`
}

type SelectParams struct {
	ID string `json:"id"`
}

type SelectedResult struct {
	ID string `json:"id"`
}

// RequestStartedEvent is emitted as a `proxy/request-started`
// notification the moment we've resolved a target node and are about
// to forward the request to it. Pairs with the existing
// `proxy/request` completion event by ID so the orchestrator can
// track in-flight requests per target — increment on start, decrement
// on the matching completion. Rejection-path requests (no active
// node) never get a started event because they were never in flight;
// they go straight to a completion event with an unmatched ID.
//
// NodeID is the chosen node's identifier in the discovery list. It's
// the authoritative way to attribute activity to a node card in the
// UI: Target (host:port) would be ambiguous whenever the proxy
// rewrote a local-interface address to 127.0.0.1 (see nodeURL), so
// multiple nodes could plausibly match the same Target string.
// Cluster model-list fan-out has no single node, so it reports an empty
// NodeID and the explicit Target "cluster".
type RequestStartedEvent struct {
	ID     string `json:"id"`
	NodeID string `json:"node_id,omitempty"`
	Method string `json:"method"`
	Path   string `json:"path"`
	Target string `json:"target"`
}

// RequestEvent is emitted as a `proxy/request` notification when a
// proxied request finishes (or is rejected before forwarding). The
// ID is unique within one proxy process lifetime — paired with the
// matching RequestStartedEvent so consumers can pop it from an
// in-flight map. ID is always populated even on the rejection path
// (where no Started event was emitted) so consumers don't need a
// separate code path for ID-less completions.
//
// NodeID is empty on the rejection path (no target was resolved) and
// on cluster model-list fan-out, and is the chosen node's identifier otherwise. See RequestStartedEvent
// for why attribution by NodeID is needed instead of by Target.
//
// TTFB is the time-to-first-byte: milliseconds from the moment we
// started forwarding to the node until its HTTP response status line
// came back, captured via ReverseProxy.ModifyResponse. Omitted
// (serialized as absent rather than zero) when not applicable:
// rejection path (no forward happened) and upstream-error path
// (ModifyResponse is never called on connection/dial failures). This
// is the "is the node snappy?" signal — distinct from Duration,
// which for streaming LM Studio responses is dominated by token
// generation time and so doesn't really reflect latency at all.
type RequestEvent struct {
	ID       string `json:"id"`
	NodeID   string `json:"node_id,omitempty"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	Target   string `json:"target"`
	Status   int    `json:"status"`
	Duration int64  `json:"duration_ms"`
	TTFB     int64  `json:"ttfb_ms,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Workload lifecycle method names (workload-manager spec 7). The proxy is
// a workload *producer*: it emits one of these per forwarded inference
// request so the broker can stamp the origin (originatedFrom) and forward it to the
// workload-manager, which broadcasts it cluster-wide. We don't emit
// workload:submitted (the proxy never queues — it forwards immediately) or
// workloads:remove (retirement is a broker concern).
const (
	workloadStartedMethod   = "workload:started"
	workloadCompletedMethod = "workload:completed"
	workloadErroredMethod   = "workload:errored"

	// fallbackWorkloadEngine is the opaque engine identifier used when a
	// forwarded request could not be attributed to one of this proxy's engines —
	// a peer whose per-engine model attribution has not arrived yet. It is the
	// first engine in openaiEngines, which is also the tie-break order routing
	// uses, so the tag matches what the request was routed as.
	fallbackWorkloadEngine = "lmstudio"
)

// inferenceEndpoints is the set of request paths that count as cluster
// workloads. Health checks, model listings (/v1/models), and other control
// traffic are deliberately excluded so we don't flood the cluster with
// non-inference noise. Every engine behind this proxy serves the
// OpenAI-compatible API, so these are the OpenAI inference routes.
var inferenceEndpoints = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/completions":      true,
	"/v1/embeddings":       true,
}

// isInferenceRequest reports whether a request should be tracked as a
// workload — a POST to one of the known inference endpoints.
func isInferenceRequest(method, path string) bool {
	return method == http.MethodPost && inferenceEndpoints[path]
}

// Workload mirrors the workload-manager spec 6 object. The proxy populates
// the fields it can observe: originatedFrom is intentionally left empty for
// the broker to stamp with the authoritative local node id (exactly like
// errors:report), while scheduledOn is set to the node this proxy actually
// routed the request to (the served candidate's node id — the same
// authoritative attribution handle), so a consumer can attribute the workload
// to where it ran rather than to where it came from. requesterId is omitted.
// Pointer fields serialize as JSON null when unset, matching the spec's
// nullable columns.
type Workload struct {
	ID             string  `json:"id"`
	Model          string  `json:"model"`
	Engine         string  `json:"engine"`
	RunID          string  `json:"runId"`
	State          string  `json:"state"`
	OriginatedFrom string  `json:"originatedFrom"`
	ScheduledOn    string  `json:"scheduledOn,omitempty"`
	CreatedAt      int64   `json:"createdAt"`
	StartedAt      *int64  `json:"startedAt"`
	CompletedAt    *int64  `json:"completedAt"`
	Error          *string `json:"error"`
	RequesterID    *string `json:"requesterId"`
}

// workloadParams is the params envelope for a workload:* notification
// (spec 7.1): a single workloadInfo carrying the full Workload.
type workloadParams struct {
	WorkloadInfo Workload `json:"workloadInfo"`
}

// bufferBodyAndModel reads the request body once and returns the raw bytes
// (so each failover attempt can replay it — see the loop in handleHTTP) along
// with the JSON "model" field for workload tracking. Inference bodies are
// small (prompt + model), so full buffering is cheap. Returns (nil, "") when
// the body is absent and an empty model when none is parseable. The caller
// restores r.Body from the returned bytes before each forward attempt.
func bufferBodyAndModel(r *http.Request) ([]byte, string) {
	if r.Body == nil {
		return nil, ""
	}
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		return body, ""
	}
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return body, ""
	}
	return body, probe.Model
}

type statusCapture struct {
	http.ResponseWriter
	status int

	// idle bounds how long a single write of streamed bytes to the client may
	// block before it's abandoned. Zero disables the deadline. See
	// idleClientWriteTimeout for the rationale (killed-client / half-open
	// socket zombie jobs).
	idle time.Duration
	rc   *http.ResponseController
	// wroteErr retains the first error returned when writing the response body
	// to the client (e.g. a dead client's send buffer filling and the write
	// deadline tripping), so handleHTTP can mark the workload failed rather
	// than misreporting the truncated stream as completed.
	wroteErr error

	// upstreamAlive is called after each successful body write, but only once the
	// upstream has committed — handleHTTP sets it in ModifyResponse, so it stays
	// nil while the only thing this writer could carry is the proxy's own error
	// body. Past that point every byte written came from the node serving the
	// request, which is proof that node is working: the liveness evidence
	// discovery cannot obtain for itself while the node is too busy to answer a
	// probe. Called on the reverse proxy's copy goroutine, so it must be cheap.
	upstreamAlive func()
}

// Unwrap exposes the underlying ResponseWriter so http.ResponseController can
// reach the connection for SetWriteDeadline (and Flush) through this wrapper.
func (sc *statusCapture) Unwrap() http.ResponseWriter { return sc.ResponseWriter }

func (sc *statusCapture) WriteHeader(code int) {
	sc.status = code
	sc.ResponseWriter.WriteHeader(code)
}

// Write bounds each streamed write to the client with a deadline so a write
// blocked on a dead/half-open client fails promptly instead of hanging the
// reverse-proxy copy indefinitely. The deadline is cleared after every
// successful write, so a legitimately slow generation with long gaps between
// tokens is never penalized — only a write actively stuck on a gone client
// trips it. The first write error is retained (wroteErr) for the caller.
func (sc *statusCapture) Write(b []byte) (int, error) {
	if sc.idle > 0 {
		if sc.rc == nil {
			sc.rc = http.NewResponseController(sc.ResponseWriter)
		}
		_ = sc.rc.SetWriteDeadline(time.Now().Add(sc.idle))
	}
	n, err := sc.ResponseWriter.Write(b)
	if err != nil {
		if sc.wroteErr == nil {
			sc.wroteErr = err
		}
	} else if sc.rc != nil {
		_ = sc.rc.SetWriteDeadline(time.Time{})
	}
	// Reported on every chunk rather than once per response so a long generation
	// keeps vouching for its node for as long as it streams. The reporter
	// coalesces, so the cost of calling this per chunk is a mutex and a clock
	// read.
	if err == nil && sc.upstreamAlive != nil {
		sc.upstreamAlive()
	}
	return n, err
}

// FlushError makes the streamed flush deadline-aware. For a streaming
// (chunked) upstream, ReverseProxy flushes after every write via
// http.NewResponseController(w).Flush — and because a small chunk buffers on
// Write without touching the socket, the actual network write for it happens
// here in Flush, not in Write. Without this method that flush reaches the
// underlying connection through Unwrap with no deadline and blocks unbounded on
// a stalled client (the same zombie the Write deadline guards against). So arm
// the same idle deadline around the flush, clear it on success, and retain a
// real flush error so the workload is classified failed. Implementing
// FlushError (which also satisfies the Flusher path via the ResponseController)
// means the flush routes through here instead of unwrapping past us.
func (sc *statusCapture) FlushError() error {
	if sc.rc == nil {
		sc.rc = http.NewResponseController(sc.ResponseWriter)
	}
	if sc.idle > 0 {
		_ = sc.rc.SetWriteDeadline(time.Now().Add(sc.idle))
	}
	err := sc.rc.Flush()
	if err != nil {
		// A ResponseWriter that genuinely can't flush is not a client failure;
		// only retain real I/O errors (e.g. the deadline tripping on a dead
		// client) so we don't misreport an unsupported-flush as a failed write.
		if !stderrors.Is(err, http.ErrNotSupported) && sc.wroteErr == nil {
			sc.wroteErr = err
		}
		return err
	}
	if sc.idle > 0 {
		_ = sc.rc.SetWriteDeadline(time.Time{})
	}
	return nil
}

type Proxy struct {
	codec     *Codec
	discovery *Discovery
	cancel    context.CancelFunc

	// httpMu guards port, the servers, the split listener, and ln across a
	// live set-port rebind. The HTTP handlers never read port (they route by
	// upstream node), so the only contention is set-port vs set-port
	// (serialized) and the initial serveHTTP store vs a later rebind.
	httpMu   sync.Mutex
	port     int
	plainSrv *http.Server
	tlsSrv   *http.Server
	split    *splitlisten.Splitter
	ln       net.Listener

	// mesh is this node's cluster mTLS trust fabric, loaded from --cluster-dir.
	// nil = unclustered: the LAN TLS ingress accepts nothing and the node does
	// only loopback-plaintext local routing. Read-only after startup.
	mesh *clustertrust.Mesh

	// backendMu guards backends, this node's own loopback engines keyed by
	// engine-manager name. The broker sets/clears each via
	// node/set-local-backend; they are never sourced from discovery, so an
	// ingress request can only ever reach this node's own local engines and can
	// never be re-routed to a peer. One node may run several at once, so each is
	// set and cleared independently.
	backendMu sync.RWMutex
	backends  map[string]localBackend

	// localModels caches which of this node's own engines serves which models,
	// read from the engines themselves. It is what the mTLS ingress uses to pick
	// the engine for a peer's request. See localModelsByEngine.
	localModels localModelsCache

	selectedMu sync.RWMutex
	selectedID string

	// activity coalesces the liveness reports raised when a peer's engine streams
	// response bytes back through us (see reportActivity).
	activity *nodeactivity.Reporter

	// priorityMu guards the scheduler's authoritative baseline and the
	// optimistic reservations made since that snapshot arrived. resolveCandidates
	// reads priority to form the failover list; reserveCandidate atomically adds
	// local dispatches before forwarding so a concurrent burst cannot repeatedly
	// choose from the same stale scheduler state.
	priorityMu           sync.RWMutex
	priority             []string
	priorityPending      map[string]int
	priorityGPUPressure  map[string]int
	priorityReservations map[string]int

	// targets remembers, per node, which of its published addresses accepted a
	// connection, so a repeated forward costs no confirmation. An entry is
	// re-confirmed when the node's candidate list changes and forgotten on an
	// upstream error, so the next request fails over to another address.
	targets *reach.Chooser

	// transportMu guards the long-lived HTTP transports reused across forwards
	// and model-list fetches. Allocating a new http.Transport per request
	// defeats connection pooling and leaks idle sockets until GC.
	transportMu    sync.Mutex
	plainTransport *http.Transport
	peerTransports map[string]*http.Transport

	// nextRequestID is a monotonic counter for tagging RequestStarted /
	// RequestEvent pairs. Atomic add returns the new value, so request
	// IDs start at 1 and never collide within a single proxy lifetime.
	// IDs deliberately reset across restarts — they're only meaningful
	// while the orchestrator's in-flight map is also alive, and a
	// fresh proxy session always starts that map empty on the
	// orchestrator side via the proxy:stopped → proxy:ready event
	// pair.
	nextRequestID atomic.Uint64

	// runID is a per-process nonce minted at startup and stamped on every
	// workload this proxy emits. It makes a workload's identity
	// (originatedFrom, engine, runId, id) globally unique even though
	// nextRequestID resets to 1 on restart and the Ollama proxy also counts
	// from 1 — without it, two concurrent cross-engine jobs, or a reused id
	// after a restart, would collide in the broker's store.
	runID string
}

func NewProxy(codec *Codec, discovery *Discovery, port int) *Proxy {
	return &Proxy{
		codec:     codec,
		discovery: discovery,
		port:      port,
		targets:   reach.NewChooser(),
		runID:     newRunID(),
		activity:  nodeactivity.NewReporter(activityReportInterval),
	}
}

// activityReportInterval is how often a single node's streaming may raise a
// liveness report. A generation writes hundreds of chunks and the scanner treats
// a report as good for a minute, so anything finer is pure noise on the broker
// pipe.
const activityReportInterval = 2 * time.Second

// reportActivity tells the broker a node's engine just returned response bytes,
// so discovery can keep that node even while it is too busy to answer a liveness
// probe. This is the only liveness signal that strengthens under load, which is
// exactly when the probe-based ones fail.
//
// Reports are not filtered to remote nodes here: this proxy knows targets by URL
// and port, not by whether a uuid is its own. The scanner holds that identity and
// drops its own (see noteActivity).
func (p *Proxy) reportActivity(nodeID string) {
	if !p.activity.Due(nodeID) {
		return
	}
	if err := p.codec.Notify(noderec.NotifyNodeActivity, noderec.NodeActivityParams{HostUUID: nodeID}); err != nil {
		slog.Debug("failed to report node activity", "node_id", nodeID, "err", err)
	}
}

// newRunID returns a short random per-process nonce (hex). A crypto/rand read
// failure falls back to a timestamp — uniqueness matters more than
// unpredictability here.
func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

func (p *Proxy) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	p.cancel = cancel
	defer cancel()

	// Keep the "is this address local?" set fresh as interfaces come and go,
	// so the loopback rewrite in nodeURL stays correct after VPN/dock changes
	// or a sleep/wake IP reassignment.
	startLocalAddrWatch(ctx)

	// Bind synchronously before announcing "ready": if the port is
	// already in use, we want the UI to see a real error reason
	// instead of being stuck on "Proxy running" while ListenAndServe
	// silently fails in a goroutine.
	ln, err := p.listen()
	if err != nil {
		// Best-effort: notify the orchestrator with a structured
		// reason. The process will exit non-zero regardless (main.go
		// log.Fatalf's on a non-nil Run error), so a failed Notify
		// here is not worth surfacing separately.
		_ = p.codec.Notify("error", ErrorParams{
			Code:    "bind-failed",
			Message: fmt.Sprintf("failed to bind port %d: %v", p.port, err),
			Port:    p.port,
		})
		return fmt.Errorf("failed to bind port %d: %w", p.port, err)
	}

	if err := p.codec.Notify("ready", ReadyParams{
		Version: Version,
		Port:    p.port,
	}); err != nil {
		// Don't leave a dangling listener holding the port if we
		// couldn't even tell the orchestrator about it.
		_ = ln.Close()
		return fmt.Errorf("failed to send ready notification: %w", err)
	}

	// Routing targets come from the broker's discovery relay. Subscribe for every
	// OpenAI-compatible engine service (lm, vl); they arrive as discovery:nodes
	// snapshots (handled in handleMessage), each replacing the subscribed
	// overlay. Non-fatal: if the parent isn't a relay-aware broker the proxy
	// still routes to manual nodes.
	services := subscribedServices()
	slog.Debug("subscribing to discovery relay for routing targets", "services", services)
	if err := p.codec.Notify(noderec.MethodSubscribe, noderec.SubscribeParams{Services: services}); err != nil {
		slog.Warn("failed to subscribe to discovery relay", "err", err)
	}

	p.serveHTTP(ctx, ln)

	err = p.readLoop(ctx)

	// The app is going away (stdin closed or ctx cancelled). Stop any
	// inference requests still in flight rather than letting them run to
	// completion: cancelling the proxy's root context propagates to every
	// in-flight request context — and thus the upstream reverse-proxy
	// connection — so the target LM Studio sees the client disconnect and stops
	// generating instead of burning the GPU on a result nobody will read.
	cancel()

	// srv.Shutdown then waits for the handlers to unwind (now fast, since
	// their upstream calls were just cancelled). As each returns it emits its
	// own terminal workload:errored, so peers don't keep showing the workload
	// as a "running" ghost. A hard kill (SIGKILL) bypasses all of this.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutCancel()
	p.shutdown(shutCtx)

	return err
}

// Timeouts for upstream connections. Logged at startup so they're always
// present in any captured log for post-mortem analysis.
const (
	proxyDialTimeout     = 10 * time.Second
	proxyKeepAlive       = 30 * time.Second
	proxyResponseTimeout = 120 * time.Second
	proxyMaxIdleConns    = 50
	proxyIdleConnTimeout = 90 * time.Second
	// Inbound http.Server limits — keep IdleTimeout aligned with client
	// IdleConnTimeout so idle keep-alives are reaped on both sides.
	proxyReadHeaderTimeout = 10 * time.Second
	proxyServerIdleTimeout = 90 * time.Second
	maxModelListBytes      = 16 << 20
)

// idleClientWriteTimeout bounds how long a single write of streamed response
// bytes to the client may block. A killed client can leave a half-open socket
// whose kernel send buffer fills and never drains; without this deadline the
// reverse-proxy copy blocks indefinitely (TCP retransmit backoff runs into
// minutes, and r.Context() never fires when no FIN/RST arrives), so the request
// handler never returns and its terminal workload event is never emitted — the
// "zombie job" left showing as running until PAIR restarts. statusCapture.Write
// resets the deadline after every successful write, so this only trips a write
// that is actively stuck on a gone client, never a slow-but-live generation.
//
// It is a var (not a const) only so a test can shorten it to exercise the
// deadline against a real socket; production never reassigns it.
var idleClientWriteTimeout = 30 * time.Second

var modelListClient = &http.Client{
	Transport: &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   proxyDialTimeout,
			KeepAlive: proxyKeepAlive,
		}).DialContext,
		ResponseHeaderTimeout: proxyDialTimeout,
		MaxIdleConns:          proxyMaxIdleConns,
		IdleConnTimeout:       proxyIdleConnTimeout,
	},
	Timeout: proxyDialTimeout,
}

// listen binds the proxy's TCP listener synchronously so bind failures
// (EADDRINUSE and friends) can be reported through a structured error
// notification before the process exits. The caller is responsible for
// closing the returned listener if it doesn't hand it to serveHTTP.
func (p *Proxy) listen() (net.Listener, error) {
	return net.Listen("tcp", fmt.Sprintf(":%d", p.port))
}

// serveHTTP takes the already-bound base listener and drives the two proxy
// personalities over it: a plaintext HTTP server (loopback-only, full local
// router) and a LAN mTLS ingress (pin-gated, forwards to the local engine),
// split by the connection's first byte via nvpair-shared/splitlisten. The two
// http.Servers are recorded so set-port can rebind both onto a fresh split
// without tearing the servers down.
func (p *Proxy) serveHTTP(ctx context.Context, ln net.Listener) {
	base := func(_ net.Listener) context.Context { return ctx }
	plainSrv := &http.Server{
		Handler:           http.HandlerFunc(p.handlePlain),
		BaseContext:       base,
		ReadHeaderTimeout: proxyReadHeaderTimeout,
		IdleTimeout:       proxyServerIdleTimeout,
	}
	tlsSrv := &http.Server{
		Handler:           http.HandlerFunc(p.handleClusterIngress),
		BaseContext:       base,
		ReadHeaderTimeout: proxyReadHeaderTimeout,
		IdleTimeout:       proxyServerIdleTimeout,
	}

	p.httpMu.Lock()
	p.plainSrv = plainSrv
	p.tlsSrv = tlsSrv
	p.ln = ln
	p.startSplitLocked(ln)
	p.httpMu.Unlock()

	slog.Info("proxy timeouts configured",
		"dial_timeout", proxyDialTimeout,
		"keep_alive", proxyKeepAlive,
		"response_header_timeout", proxyResponseTimeout,
		"max_idle_conns", proxyMaxIdleConns,
		"idle_conn_timeout", proxyIdleConnTimeout,
	)
	slog.Info("HTTP proxy listening", "port", p.port, "addr", ln.Addr().String(),
		"cluster_ingress", p.mesh.Clustered())
}

// startSplitLocked wraps base in a first-byte splitter and starts both servers
// on its sub-listeners. Caller holds httpMu. Reuses the persistent plainSrv /
// tlsSrv so set-port can call it repeatedly on fresh listeners.
func (p *Proxy) startSplitLocked(base net.Listener) {
	split := splitlisten.New(base)
	p.split = split
	go func() {
		if err := p.plainSrv.Serve(split.Plain()); err != nil && err != http.ErrServerClosed {
			slog.Error("plaintext HTTP server exited", "err", err)
		}
	}()
	go p.serveTLS(split.TLS())
}

// serveTLS terminates cluster mTLS on the split's TLS sub-listener. The server
// certificate is resolved per handshake from the live mesh, so this one
// sub-listener covers both states: while this node is unclustered there is no
// leaf to present and the handshake is refused (it exposes no LAN inference
// surface), and the moment the node becomes a member the same sub-listener
// serves the pin-gated ingress — no rebind, and no process restart to pick up a
// freshly-minted identity.
func (p *Proxy) serveTLS(l net.Listener) {
	if err := p.tlsSrv.Serve(tls.NewListener(l, p.mesh.ServerTLSConfig())); err != nil && err != http.ErrServerClosed {
		slog.Error("cluster mTLS ingress exited", "err", err)
	}
}

// shutdown gracefully stops both personalities and closes the split listener.
func (p *Proxy) shutdown(ctx context.Context) {
	p.httpMu.Lock()
	plainSrv, tlsSrv, split := p.plainSrv, p.tlsSrv, p.split
	p.httpMu.Unlock()
	if plainSrv != nil {
		_ = plainSrv.Shutdown(ctx)
	}
	if tlsSrv != nil {
		_ = tlsSrv.Shutdown(ctx)
	}
	if split != nil {
		_ = split.Close()
	}
	p.closeIdleTransports()
}

// setPort live-rebinds the HTTP listener onto newPort and persists the choice
// so it survives a restart. It binds the new listener first (so a bind
// failure leaves the current one serving), starts the same server on it, then
// closes the old listener — in-flight connections on the old port drain
// naturally. A fresh `ready` notification announces the new port so the
// orchestrator/UI learn where the proxy is now listening.
func (p *Proxy) setPort(newPort int) error {
	p.httpMu.Lock()
	defer p.httpMu.Unlock()

	if newPort == p.port {
		return nil
	}
	newLn, err := net.Listen("tcp", fmt.Sprintf(":%d", newPort))
	if err != nil {
		return fmt.Errorf("failed to bind port %d: %w", newPort, err)
	}
	oldSplit := p.split
	p.ln = newLn
	p.port = newPort

	// Re-serve both personalities on a fresh split over the new listener, then
	// close the old split (and its base listener) so in-flight connections on
	// the old port drain naturally. The plaintext and mTLS personalities always
	// move together as one unit.
	slog.Info("HTTP proxy listening", "port", newPort, "addr", newLn.Addr().String(),
		"cluster_ingress", p.mesh.Clustered())
	p.startSplitLocked(newLn)
	if oldSplit != nil {
		_ = oldSplit.Close()
	}

	if err := savePersistedPort(newPort); err != nil {
		slog.Warn("failed to persist proxy port", "port", newPort, "err", err)
	}
	if err := p.codec.Notify("ready", ReadyParams{Version: Version, Port: newPort}); err != nil {
		slog.Warn("failed to emit ready after rebind", "err", err)
	}
	return nil
}

// emitWorkload sends a workload:* lifecycle notification to the
// orchestrator. The broker stamps the origin (originatedFrom) and forwards it to the
// workload-manager; a failed write is logged but never blocks the request.
func (p *Proxy) emitWorkload(method string, w Workload) {
	if err := p.codec.Notify(method, workloadParams{WorkloadInfo: w}); err != nil {
		slog.Warn("failed to emit workload notification", "method", method, "err", err)
	}
}

// candidate is one forwarding target: the node's discovery ID (the
// authoritative attribution handle, stable across nodeURL's 127.0.0.1
// rewrite) and its resolved URL. peerUUID is set for a remote cluster peer:
// the request is dialed over cluster mTLS to the peer's promoted proxy (https),
// pinned to that peer's exact server cert. Empty peerUUID means a plain-HTTP
// dial — the local backend (self) or an explicit manual node.
type candidate struct {
	id  string
	url *url.URL
	// routeKey keys the reachability chooser. It equals id for a relay peer and
	// is engine-qualified for a manual node, so two manual entries for one host's
	// two engines never share a confirmed address. See Node.routeKey.
	routeKey string
	// engine is the OpenAI engine on that node this request is routed to. It is
	// what the emitted workload is tagged with, so a dual-engine node's work is
	// attributed to the engine that actually ran it.
	engine   string
	peerUUID string
}

// candidateTransport returns the reverse-proxy / model-list transport for a
// candidate. Plain/self/manual candidates share one long-lived Transport.
// Cluster peers share one long-lived mTLS Transport per peerUUID. Callers must
// not CloseIdleConnections on the returned value except via closeIdleTransports.
func (p *Proxy) candidateTransport(c candidate) *http.Transport {
	if c.peerUUID == "" {
		return p.plainHTTPTransport()
	}
	return p.peerHTTPTransport(c.peerUUID)
}

func newProxyTransport(tlsCfg *tls.Config) *http.Transport {
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   proxyDialTimeout,
			KeepAlive: proxyKeepAlive,
		}).DialContext,
		ResponseHeaderTimeout: proxyResponseTimeout,
		MaxIdleConns:          proxyMaxIdleConns,
		MaxIdleConnsPerHost:   proxyMaxIdleConns,
		IdleConnTimeout:       proxyIdleConnTimeout,
	}
	if tlsCfg != nil {
		tr.TLSClientConfig = tlsCfg
	}
	return tr
}

func (p *Proxy) plainHTTPTransport() *http.Transport {
	p.transportMu.Lock()
	defer p.transportMu.Unlock()
	if p.plainTransport == nil {
		p.plainTransport = newProxyTransport(nil)
	}
	return p.plainTransport
}

func (p *Proxy) peerHTTPTransport(peerUUID string) *http.Transport {
	p.transportMu.Lock()
	defer p.transportMu.Unlock()
	if tr, ok := p.peerTransports[peerUUID]; ok {
		if p.mesh != nil && p.mesh.HasPin(peerUUID) {
			return tr
		}
		tr.CloseIdleConnections()
		delete(p.peerTransports, peerUUID)
	}
	if p.mesh == nil {
		return newProxyTransport(nil)
	}
	cfg, ok := p.mesh.ClientTLSConfig(peerUUID)
	if !ok {
		return newProxyTransport(nil)
	}
	tr := newProxyTransport(cfg)
	if p.peerTransports == nil {
		p.peerTransports = make(map[string]*http.Transport)
	}
	p.peerTransports[peerUUID] = tr
	return tr
}

// dropUnpinnedPeerTransports closes idle conns for peer Transports whose pins
// are gone. Safe to call from the mesh Watch callback.
func (p *Proxy) dropUnpinnedPeerTransports() {
	p.transportMu.Lock()
	defer p.transportMu.Unlock()
	for uuid, tr := range p.peerTransports {
		if p.mesh != nil && p.mesh.HasPin(uuid) {
			continue
		}
		tr.CloseIdleConnections()
		delete(p.peerTransports, uuid)
	}
}

func (p *Proxy) closeIdleTransports() {
	p.transportMu.Lock()
	defer p.transportMu.Unlock()
	if p.plainTransport != nil {
		p.plainTransport.CloseIdleConnections()
	}
	for uuid, tr := range p.peerTransports {
		tr.CloseIdleConnections()
		delete(p.peerTransports, uuid)
	}
}

// retrySignal is returned from ModifyResponse to abort a retryable upstream
// response before its body streams to the client, so handleHTTP can fail over
// to the next candidate. It's a distinct type rather than errors.New(...)
// because this package aliases nvpair-shared/errors as `errors` (which has no New).
type retrySignal struct{}

func (retrySignal) Error() string { return "lmstudio-proxy: retry next candidate" }

type modelListItem struct {
	key string
	raw json.RawMessage
}

type modelListResult struct {
	items []modelListItem
	ok    bool
	err   error
}

// serveModelList queries every LM Studio candidate concurrently and returns
// the native /v1/models envelope with duplicate IDs removed. Results are
// merged in candidate order, not completion order, so duplicate metadata is
// deterministic while an unavailable peer cannot hide healthy inventories.
func (p *Proxy) serveModelList(w http.ResponseWriter, r *http.Request, candidates []candidate) (int, error) {
	writeJSON := func(status int, body []byte) {
		cors.Apply(w.Header())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}
	results := make([]modelListResult, len(candidates))
	var wg sync.WaitGroup
	for i, cand := range candidates {
		target := *cand.url
		target.Path = r.URL.Path
		target.RawPath = r.URL.RawPath
		target.RawQuery = r.URL.RawQuery

		// A cluster-peer candidate is queried over mTLS to its promoted proxy;
		// self/manual candidates use the shared plain client.
		client := modelListClient
		if cand.peerUUID != "" {
			client = &http.Client{Timeout: modelListClient.Timeout, Transport: p.candidateTransport(cand)}
		}

		wg.Add(1)
		go func(i int, cand candidate, target url.URL, client *http.Client) {
			defer wg.Done()
			items, err := fetchModelList(r.Context(), client, &target)
			if err != nil {
				if isTransportError(err) {
					p.targets.Forget(cand.routeKey)
				}
				results[i].err = err
				return
			}
			results[i] = modelListResult{items: items, ok: true}
		}(i, cand, target, client)
	}
	wg.Wait()

	success := false
	models := make([]json.RawMessage, 0)
	seen := make(map[string]bool)
	for i, result := range results {
		if !result.ok {
			slog.Debug("model list candidate unavailable",
				"node_id", candidates[i].id, "target", candidates[i].url.Host, "err", result.err)
			continue
		}
		success = true
		for _, item := range result.items {
			if !seen[item.key] {
				seen[item.key] = true
				models = append(models, item.raw)
			}
		}
	}
	if !success {
		err := fmt.Errorf("no valid model list from %d candidate(s)", len(candidates))
		writeJSON(http.StatusServiceUnavailable, []byte(`{"error":"model inventory unavailable"}`))
		return http.StatusServiceUnavailable, err
	}
	body, err := json.Marshal(struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}{Object: "list", Data: models})
	if err != nil {
		writeJSON(http.StatusInternalServerError, []byte(`{"error":"failed to encode model inventory"}`))
		return http.StatusInternalServerError, err
	}
	writeJSON(http.StatusOK, body)
	return http.StatusOK, nil
}

func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// Allocate the request ID up front so both code paths (rejection
	// and forward) can stamp the same value into their notification.
	// The rejection path never emits a Started event, so its ID won't
	// appear in any orchestrator in-flight map — that's fine; the
	// completion event still bumps the failed counter regardless of
	// whether a matching Started was seen.
	reqID := strconv.FormatUint(p.nextRequestID.Add(1), 10)

	// Parse the request's model before choosing a node. Model eligibility only
	// applies to inference routes; control endpoints retain their existing
	// routing behavior even when their JSON happens to contain a model field.
	bodyBytes, model := bufferBodyAndModel(r)
	isInf := isInferenceRequest(r.Method, r.URL.Path)
	routingModel := ""
	if isInf {
		routingModel = model
	}
	candidates := p.resolveCandidates(routingModel)
	if isInf && model != "" {
		candidates = p.reserveCandidate(candidates)
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		if len(candidates) > 0 {
			p.codec.Notify("proxy/request-started", RequestStartedEvent{
				ID: reqID, Method: r.Method, Path: r.URL.Path, Target: "cluster",
			})
		}
		status, err := p.serveModelList(w, r, candidates)
		errText := ""
		if err != nil {
			errText = err.Error()
		}
		p.codec.Notify("proxy/request", RequestEvent{
			ID: reqID, Method: r.Method, Path: r.URL.Path, Target: "cluster",
			Status: status, Duration: time.Since(start).Milliseconds(), Error: errText,
		})
		return
	}
	if len(candidates) == 0 {
		// With no engine to consult, retain the local permissive preflight used
		// for engines that do not publish a CORS policy.
		if cors.WritePreflight(w, r) {
			return
		}
		cors.Apply(w.Header())
		rejectionBody := `{"error":"no active node selected or available"}`
		rejectionError := "no active node"
		if isInf && model != "" {
			rejectionBody = `{"error":"no available node advertises the requested model"}`
			rejectionError = "no node advertises requested model"
		}
		slog.Warn("proxy request rejected",
			"id", reqID, "method", r.Method, "path", r.URL.Path,
			"remote", r.RemoteAddr, "reason", rejectionError)
		http.Error(w, rejectionBody, http.StatusBadGateway)
		p.codec.Notify("proxy/request", RequestEvent{
			ID:       reqID,
			Method:   r.Method,
			Path:     r.URL.Path,
			Status:   http.StatusBadGateway,
			Duration: time.Since(start).Milliseconds(),
			Error:    rejectionError,
		})
		return
	}
	// shouldRetry reports whether an upstream status warrants failing over to
	// the next candidate: busy/unavailable/gateway statuses, plus a 404 on an
	// inference call (an advertised owner's inventory may have become stale).
	// Genuine client errors (400/401/422…) are not retried — they'd fail
	// identically on every node.
	shouldRetry := func(code int) bool {
		switch code {
		case http.StatusRequestTimeout,
			http.StatusTooManyRequests,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true
		case http.StatusNotFound:
			return isInf
		}
		return code >= 500
	}

	var (
		servedNodeID string
		servedTarget string
		ttfbMs       int64
		proxyErr     string
		finalStatus  int
		started      bool
		wl           *Workload
	)

	// Emit workload:started up front, the moment we begin forwarding, naming
	// the first candidate we'll try. A burst of concurrent inference requests
	// must surface as job cards immediately; the upstream engine serializes
	// concurrent requests on a single GPU slot, so gating "started" on the
	// upstream response headers (the commit point) left every queued-but-
	// forwarded job invisible until the node dequeued it — only one card at a
	// time (a prior regression). If failover later commits a different
	// node, the commit block re-points scheduledOn; the terminal
	// completed/errored transition is emitted once at the end regardless.
	if isInf && model != "" {
		createdMs := start.UnixMilli()
		wl = &Workload{
			ID:          reqID,
			Model:       model,
			Engine:      candidateEngine(candidates[0]),
			RunID:       p.runID,
			State:       "running",
			ScheduledOn: candidates[0].id,
			CreatedAt:   createdMs,
			StartedAt:   &createdMs,
		}
		p.emitWorkload(workloadStartedMethod, *wl)
	}

	// The terminal workload transition (completed/errored) can be reached from
	// two places: the normal path after the stream copy unwinds below, and the
	// disconnect watcher that fires while the copy is still blocked. terminalOnce
	// guarantees exactly one is emitted; wlMu guards the shared wl fields the
	// watcher (a separate goroutine) and ModifyResponse's failover re-point both
	// touch; terminated suppresses a late started re-point once we've finalized.
	var (
		terminalOnce sync.Once
		wlMu         sync.Mutex
		terminated   bool
	)
	emitTerminal := func(state, errMsg string) {
		if wl == nil {
			return
		}
		terminalOnce.Do(func() {
			now := time.Now().UnixMilli()
			wlMu.Lock()
			terminated = true
			wl.CompletedAt = &now
			wl.State = state
			if errMsg != "" {
				wl.Error = &errMsg
			}
			snapshot := *wl
			wlMu.Unlock()
			method := workloadCompletedMethod
			if state != "completed" {
				method = workloadErroredMethod
			}
			p.emitWorkload(method, snapshot)
		})
	}

	// Watch for the client going away while the request is in flight. The
	// terminal event is otherwise emitted only after the stream copy returns;
	// a client that disconnects mid-stream can leave the copy blocked, so we
	// emit the terminal here the moment r.Context() is cancelled instead of
	// waiting for the unwind. Cancelling r.Context() (client close, or our own
	// shutdown) also propagates to the ReverseProxy's upstream request, so the
	// engine stops generating. terminalOnce keeps this from double-emitting
	// with the normal path. The half-open case (no FIN, r.Context() never
	// fires) is caught instead by statusCapture's write deadline below.
	if wl != nil {
		reqCtx := r.Context()
		finished := make(chan struct{})
		defer close(finished)
		go func() {
			select {
			case <-reqCtx.Done():
				emitTerminal("failed", "client disconnected before completion")
			case <-finished:
			}
		}()
	}

	// committedSC is the statusCapture of the candidate we committed to
	// streaming; its wroteErr tells us after the fact whether the client write
	// failed (dead/half-open client) so we can mark the workload failed.
	var committedSC *statusCapture

	// Failover loop: try candidates in order until one returns a
	// usable response or the list is exhausted. We can only retry before the
	// first byte reaches the client; once a response starts streaming we're
	// committed. proxy/request-started fires at that commit point so it names
	// the node that actually serves the request, not one we failed over from;
	// workload:started was already emitted above (and is re-pointed there on a
	// failover). The self-forward guard lives in resolveCandidates.
	for i := range candidates {
		cand := candidates[i]
		last := i == len(candidates)-1
		if bodyBytes != nil {
			r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
		}
		retry := false
		sc := &statusCapture{ResponseWriter: w, status: http.StatusOK, idle: idleClientWriteTimeout}

		proxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = cand.url.Scheme
				req.URL.Host = cand.url.Host
				req.Host = cand.url.Host
			},
			// A remote cluster peer is dialed over mTLS (per-peer pinned config);
			// self/manual candidates use the plain transport. See candidateTransport.
			Transport: p.candidateTransport(cand),
			// ModifyResponse fires when the upstream's status line + headers
			// have arrived but before the body streams. That's both the retry
			// decision point and, on commit, the time-to-first-byte boundary.
			ModifyResponse: func(resp *http.Response) error {
				if !last && shouldRetry(resp.StatusCode) {
					// Abort before streaming: ReverseProxy closes resp.Body and
					// calls ErrorHandler with our sentinel, then we try next.
					retry = true
					return retrySignal{}
				}
				// Prefer an engine-declared preflight policy so an exact origin plus
				// Allow-Credentials can pass a credentialed browser fetch. Engines
				// that publish no policy retain the proxy's permissive 204 fallback.
				cors.CompletePreflightFallback(resp)
				// Committing to this candidate — body stream is about to begin.
				ttfbMs = time.Since(start).Milliseconds()
				servedNodeID = cand.id
				servedTarget = cand.url.Host
				proxyErr = "" // clear any error recorded from a failed-over candidate
				// Arm the liveness report only now. statusCapture also carries
				// the proxy's OWN error bodies — ReverseProxy's ErrorHandler
				// writes a failed dial's message through it — and those bytes
				// prove nothing about the node. Reaching here means the upstream
				// returned a status line, so everything written from this point
				// came from the node. Same goroutine as the body copy, so no
				// synchronization is needed.
				sc.upstreamAlive = func() { p.reportActivity(cand.id) }
				// The engine may enforce its own origin policy. Honor it:
				// overwriting a declared Access-Control-Allow-Origin would
				// silently widen the user's policy, and a wildcard is invalid
				// alongside Allow-Credentials, so it would break a credentialed
				// response outright. An engine that omits the header has
				// expressed nothing to preserve, so the proxy supplies its own.
				if resp.Header.Get("Access-Control-Allow-Origin") == "" {
					cors.Apply(resp.Header)
				}
				if !started {
					started = true
					p.codec.Notify("proxy/request-started", RequestStartedEvent{
						ID:     reqID,
						NodeID: cand.id,
						Method: r.Method,
						Path:   r.URL.Path,
						Target: cand.url.Host,
					})
					// workload:started was already emitted up front naming the
					// first candidate. If failover landed us on a different
					// node, re-point scheduledOn so the card — and the terminal
					// completed/errored event, which carries the same wl — name
					// the node that actually served. Guarded by wlMu against the
					// disconnect watcher, and skipped once terminated so a late
					// re-point can't resurrect a workload we've already failed.
					if wl != nil {
						wlMu.Lock()
						if !terminated && wl.ScheduledOn != cand.id {
							wl.ScheduledOn = cand.id
							snapshot := *wl
							wlMu.Unlock()
							p.emitWorkload(workloadStartedMethod, snapshot)
						} else {
							wlMu.Unlock()
						}
					}
				}
				return nil
			},
			ErrorHandler: func(ew http.ResponseWriter, _ *http.Request, err error) {
				if _, ok := err.(retrySignal); ok {
					return // retryable status — the loop advances to the next candidate
				}
				// Transport/dial error (not a status-based retry): forget this
				// node's confirmed address so the next request re-confirms and
				// can fail over to another of its published addresses
				// (multi-homed peer). The in-request failover below moves on to
				// the next node.
				p.targets.Forget(cand.routeKey)
				if !last {
					// Transport/dial error with candidates left: fail over.
					retry = true
					proxyErr = err.Error()
					slog.Warn("proxy upstream error, failing over",
						"id", reqID, "node_id", cand.id, "target", cand.url.Host,
						"path", r.URL.Path, "err", err)
					return
				}
				// Last candidate failed at the transport: terminal, surface it.
				servedNodeID = cand.id
				servedTarget = cand.url.Host
				if cors.WritePreflight(ew, r) {
					proxyErr = ""
					return
				}
				proxyErr = err.Error()
				slog.Warn("proxy upstream error, candidates exhausted",
					"id", reqID, "node_id", cand.id, "target", cand.url.Host,
					"method", r.Method, "path", r.URL.Path,
					"duration_ms", time.Since(start).Milliseconds(), "err", err)
				body, mErr := json.Marshal(map[string]string{
					"error": "upstream error: " + err.Error(),
				})
				if mErr != nil {
					body = []byte(`{"error":"upstream error"}`)
				}
				cors.Apply(ew.Header())
				ew.Header().Set("Content-Type", "application/json")
				ew.Header().Set("X-Content-Type-Options", "nosniff")
				ew.WriteHeader(http.StatusBadGateway)
				ew.Write(body)
			},
		}

		proxy.ServeHTTP(sc, r)
		if !retry {
			finalStatus = sc.status
			committedSC = sc
			break
		}
	}

	slog.Debug("proxy request complete",
		"id", reqID,
		"node_id", servedNodeID,
		"method", r.Method,
		"path", r.URL.Path,
		"target", servedTarget,
		"status", finalStatus,
		"duration_ms", time.Since(start).Milliseconds(),
		"ttfb_ms", ttfbMs,
		"err", proxyErr,
	)

	p.codec.Notify("proxy/request", RequestEvent{
		ID:       reqID,
		NodeID:   servedNodeID,
		Method:   r.Method,
		Path:     r.URL.Path,
		Target:   servedTarget,
		Status:   finalStatus,
		Duration: time.Since(start).Milliseconds(),
		TTFB:     ttfbMs,
		Error:    proxyErr,
	})

	// Terminal workload transition pairs with the workload:started emitted at
	// the commit point above. Cancellation, an upstream/transport error, a
	// failed client write (dead/half-open client), or any non-2xx status is a
	// failure; a clean 2xx is a completion. The Workload carries the same id so
	// the broker (and peers) can collapse the start/finish pair. Routed through
	// emitTerminal so the disconnect watcher and this path emit exactly once.
	if wl != nil {
		switch {
		case r.Context().Err() != nil:
			// The request was cancelled before it finished — either the
			// client disconnected or, on shutdown, we cancelled it to stop
			// the in-flight inference. A mid-stream cancel never reaches
			// ErrorHandler (the 200 headers are already sent), so without this
			// branch it would be misreported as completed. (The watcher above
			// usually beats us to it; emitTerminal makes that a no-op.)
			emitTerminal("failed", "request cancelled before completion")
		case committedSC != nil && committedSC.wroteErr != nil:
			// The response committed but a write to (or flush toward) the
			// client failed — typically the idle deadline tripping on a
			// dead/half-open client. The stream is truncated, so this is a
			// failure, not the completion the 200 status would otherwise
			// suggest.
			emitTerminal("failed", "client connection lost: "+committedSC.wroteErr.Error())
		case proxyErr != "" || finalStatus >= http.StatusBadRequest:
			msg := proxyErr
			if msg == "" {
				msg = fmt.Sprintf("upstream returned HTTP %d", finalStatus)
			}
			emitTerminal("failed", msg)
		default:
			emitTerminal("completed", "")
		}
	}
}

// resolveCandidates returns the ordered list of nodes to try for the current
// request. A model-bearing request first filters a request-local node copy to
// advertised owners. A user-selected eligible node then leads, followed by
// scheduler priority and stable ID fallback. The failover loop walks the
// resulting owner list until a node returns a usable response.
//
// A node that resolves to this proxy's own listen address is dropped
// (self-forward guard): a local LM Studio advertisement could otherwise
// resolve back to this proxy's own port and loop.
//
// Returns an empty slice when no forwarding target is available; the caller
// treats that as the rejection path.
func (p *Proxy) resolveCandidates(model string) []candidate {
	p.selectedMu.RLock()
	id := p.selectedID
	p.selectedMu.RUnlock()

	priority := p.PriorityList()

	p.httpMu.Lock()
	selfPort := p.port
	p.httpMu.Unlock()

	// Re-derive membership and pins before resolving so a cluster joined or left,
	// and a peer paired or removed, since the last request is reflected without a
	// restart: a removed peer stops being a routable candidate immediately, and a
	// freshly-paired one becomes one.
	p.mesh.Refresh()

	nodes := p.discovery.Nodes()
	known := len(nodes)
	if model != "" {
		owners := make([]Node, 0, len(nodes))
		for _, node := range nodes {
			if nodeAdvertisesModel(node, model) {
				owners = append(owners, node)
			}
		}
		nodes = owners
	}
	// Sort by ID so candidate order is stable across calls — Discovery.Nodes()
	// iterates a map, whose order is randomized per call, which otherwise bounced
	// back-to-back requests between nodes. The ID sort is also the
	// fallback order for nodes the scheduler's priority list doesn't mention.
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	byID := make(map[string]Node, len(nodes))
	for _, n := range nodes {
		byID[n.ID] = n
	}

	out := make([]candidate, 0, len(nodes))
	// Dedup by resolved backend host: the same physical node can appear under two
	// IDs (e.g. a manually-added entry and its relay-discovered record), and
	// routing to the same engine twice is wasteful. It is also what collapses a
	// peer that advertises several engine keys back into one candidate: every
	// key on a peer names that peer's single proxy port.
	seenHost := make(map[string]bool, len(nodes))
	// placed tracks node IDs already considered so the priority-ordered and
	// fallback passes don't reconsider one (scheduler ordering).
	placed := make(map[string]bool, len(nodes))
	add := func(n Node) {
		placed[n.ID] = true
		// targetURL picks a reachable address for a multi-homed node (cached,
		// TCP-probed), falling back to the first candidate; nil only when the
		// node advertises no usable address.
		u := p.targetURL(n)
		if u == nil {
			return
		}
		engine := p.nodeEngineFor(n, model)
		peerUUID := ""
		switch {
		case isSelfTarget(u, selfPort):
			// Our own advertised endpoint (the engine keys now point at this
			// proxy). Serve it from the explicit local backends — the loopback
			// engines — rather than dialing our own mTLS ingress, which would
			// recurse. Ranking still used this node's real (discovered) model
			// list above. With no routing model there is nothing to attribute, so
			// every healthy local engine becomes its own candidate and the
			// aggregated model list covers them all.
			for _, local := range p.selfTargets(n, model, engine) {
				out = appendCandidate(out, seenHost, candidate{
					id: n.ID, routeKey: n.routeKey(), engine: local.Engine, url: local.URL,
				}, selfPort)
			}
			return
		case p.mesh.HasPin(n.ClusterUUID):
			// A pinned cluster peer: reach it only over mTLS to its promoted
			// proxy (the lm port now advertises the proxy, not the engine).
			// The pin is read from the live mesh refreshed above, not from the
			// relayed n.Trusted: that flag is the scanner's answer from whenever
			// it last saw this peer's mDNS record, so a peer discovered before
			// this node's pins were written stays false until its record next
			// changes. It is also strictly weaker than what we hold here — the
			// dial itself is gated on ClientTLSConfig finding the same pin — so
			// the relayed value can only ever disagree by being stale.
			u.Scheme = "https"
			peerUUID = n.ClusterUUID
		case p.discovery.IsManual(n.ID):
			// An explicit user-added manual node: dialed plain to the address
			// the user supplied (a deliberate, separately-labeled bypass).
		default:
			// A relay peer we don't hold a pin for (untrusted, or this node is
			// unclustered). Its engine is loopback-only and its proxy refuses
			// plaintext from the LAN, so it is not a routable target.
			slog.Debug("resolveCandidates: dropping unpinned relay peer",
				"node_id", n.ID, "cluster_uuid", n.ClusterUUID)
			return
		}
		out = appendCandidate(out, seenHost, candidate{
			id:       n.ID,
			routeKey: n.routeKey(),
			engine:   engine,
			url:      u,
			peerUUID: peerUUID,
		}, selfPort)
	}

	// Capability has already been enforced. An eligible explicit selection wins,
	// then the scheduler's least-loaded order, then unlisted owners by stable ID.
	if id != "" {
		if n, ok := byID[id]; ok && !placed[id] {
			add(n)
		}
	}
	for _, pid := range priority {
		if n, ok := byID[pid]; ok && !placed[pid] {
			add(n)
		}
	}
	for _, n := range nodes {
		if !placed[n.ID] {
			add(n)
		}
	}

	slog.Debug("resolveCandidates resolved",
		"selected", id, "priority", len(priority), "candidates", len(out),
		"eligible", len(nodes), "known", known)
	return out
}

// reserveCandidate atomically moves the least estimated loaded scheduler-listed
// candidate to the front of this request's failover list. The scheduler's
// pending count and GPU pressure form the authoritative baseline; reservations
// are local dispatches made since that snapshot arrived and have not necessarily
// completed the proxy→broker→scheduler→proxy feedback loop yet.
//
// Model eligibility was enforced before this function receives the list. An
// explicit node/select pin bypasses reservations, and unlisted/manual owners
// retain their existing fallback position.
func (p *Proxy) reserveCandidate(candidates []candidate) []candidate {
	if len(candidates) == 0 {
		return candidates
	}
	if selectedID := p.SelectedID(); selectedID != "" {
		for _, cand := range candidates {
			if cand.id == selectedID {
				return candidates
			}
		}
	}

	candidateIndex := make(map[string]int, len(candidates))
	for i, cand := range candidates {
		candidateIndex[cand.id] = i
	}

	p.priorityMu.Lock()
	defer p.priorityMu.Unlock()
	if len(p.priority) == 0 {
		return candidates
	}
	if p.priorityReservations == nil {
		p.priorityReservations = make(map[string]int)
	}

	bestIndex := -1
	bestOrder := len(p.priority)
	var bestLoad uint64
	for order, id := range p.priority {
		index, ok := candidateIndex[id]
		if !ok {
			continue
		}
		load := uint64(p.priorityPending[id]) +
			uint64(p.priorityGPUPressure[id]) +
			uint64(p.priorityReservations[id])
		if bestIndex < 0 || load < bestLoad || (load == bestLoad && order < bestOrder) {
			bestIndex = index
			bestOrder = order
			bestLoad = load
		}
	}
	if bestIndex < 0 {
		return candidates
	}

	chosen := candidates[bestIndex]
	p.priorityReservations[chosen.id]++
	if bestIndex > 0 {
		copy(candidates[1:bestIndex+1], candidates[:bestIndex])
		candidates[0] = chosen
	}
	return candidates
}

func nodeAdvertisesModel(n Node, model string) bool {
	if model == "" {
		return false
	}
	for _, available := range n.Models {
		if available == model {
			return true
		}
	}
	return false
}

// isSelfTarget reports whether u points back at this proxy's own listener.
// nodeURL has already rewritten local-interface addresses to 127.0.0.1, so a
// loopback host on our own port is us.
func isSelfTarget(u *url.URL, selfPort int) bool {
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port != selfPort {
		return false
	}
	switch host {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// nodeURL returns the single best forward URL for a node (the first candidate
// in deterministic, loopback-first order). It does no reachability probing —
// p.targetURL is the request-path entry point. Kept as a free function so the
// URL-construction unit test can exercise it without a Proxy.
func nodeURL(n Node) *url.URL {
	candidates := nodeCandidates(n)
	if len(candidates) == 0 {
		return nil
	}
	return &url.URL{Scheme: "http", Host: candidates[0]}
}

// targetURL picks the forward URL for a node, preferring an address we can
// actually reach. With a single candidate it's just that candidate; with
// several (a multi-homed peer) the shared reach.Chooser returns the confirmed
// last-good address, or the node's own top-ranked one while it confirms in the
// background. The confirmation is transport-neutral: a TCP accept proves the
// address is reachable, while the real pinned mTLS request still authenticates
// which peer answered there.
//
// reach.Prefer, not a blocking confirmation: this runs once per discovered node per
// request, including nodes this request will not be routed to, so a handshake here
// would charge every request for every node's connectivity. An address that is
// wrong is caught by the ErrorHandler below, which forgets it and fails over.
func (p *Proxy) targetURL(n Node) *url.URL {
	candidates := nodeCandidates(n)
	if len(candidates) == 0 {
		return nil
	}
	host := p.targets.Prefer(n.routeKey(), candidates)
	return &url.URL{Scheme: "http", Host: host}
}

// nodeCandidates returns the ordered, de-duplicated host:port targets for a node.
//
// Order comes from the node itself: netpick.Candidates keeps the node's published
// ranking, which it derived from evidence no observer has, and appends anything
// else it advertised. Re-sorting here by address class is what previously put a
// two-host direct-connect link ahead of a peer's real LAN address.
//
// Any local-interface address is rewritten to loopback (the engine binds loopback
// only) and floated to the front because it's unambiguously reachable.
func nodeCandidates(n Node) []string {
	port := strconv.Itoa(n.Port)
	sorted := netpick.Candidates(n.TXT, n.Addresses)
	if len(sorted) == 0 {
		// A non-IP entry (a .local hostname) that netpick cannot parse.
		hosts := n.Addresses
		if len(hosts) == 0 {
			if n.Host == "" {
				return nil
			}
			hosts = []string{n.Host}
		}
		sorted = append([]string(nil), hosts...)
	}

	seen := make(map[string]bool, len(sorted))
	var loopback, rest []string
	for _, h := range sorted {
		// If the address belongs to a local interface, use loopback instead;
		// connecting via the machine's own external IP would be refused.
		if isLocalAddress(h) {
			h = "127.0.0.1"
		}
		// net.JoinHostPort bracket-wraps IPv6 literals (fe80::1 -> [fe80::1]).
		hp := net.JoinHostPort(h, port)
		if seen[hp] {
			continue
		}
		seen[hp] = true
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			loopback = append(loopback, hp)
		} else {
			rest = append(rest, hp)
		}
	}
	return append(loopback, rest...)
}

var (
	localAddrsMu sync.RWMutex
	// localAddrs is the set of IPs currently bound to this host's interfaces.
	// It's used to decide whether a discovered node is actually us, so we can
	// dial loopback instead of our own external IP (the engine binds loopback
	// only). The initial value is a one-shot enumeration; startLocalAddrWatch
	// then keeps it in sync with live interface changes, so a late VPN/dock
	// interface or a sleep/wake IP reassignment can't strand us dialing a
	// stale address.
	localAddrs = netmon.Enumerate().LocalIPs
)

func setLocalAddrs(s map[string]bool) {
	localAddrsMu.Lock()
	localAddrs = s
	localAddrsMu.Unlock()
}

func isLocalAddress(addr string) bool {
	localAddrsMu.RLock()
	defer localAddrsMu.RUnlock()
	return localAddrs[addr]
}

// startLocalAddrWatch keeps localAddrs in sync with the host's live interface
// set for the lifetime of ctx. If the network monitor can't start, the set
// stays at its initial enumeration rather than failing the proxy.
func startLocalAddrWatch(ctx context.Context) {
	mon, err := netmon.Watch(ctx)
	if err != nil {
		slog.Warn("proxy: network monitor unavailable; local address set is static", "err", err)
		return
	}
	setLocalAddrs(mon.LocalIPs())
	ch := mon.Subscribe()
	go func() {
		for range ch {
			setLocalAddrs(mon.LocalIPs())
			slog.Debug("proxy: refreshed local address set after network change")
		}
	}()
}

func (p *Proxy) SelectedID() string {
	p.selectedMu.RLock()
	defer p.selectedMu.RUnlock()
	return p.selectedID
}

func (p *Proxy) SetSelected(id string) {
	p.selectedMu.Lock()
	p.selectedID = id
	p.selectedMu.Unlock()
}

// clearSelectionIfNotPresent resets the user-selected node (and notifies the
// client) when it's neither in the relay-fed set nor a manual node, so a stale
// selection can't pin routing to a departed target.
func (p *Proxy) clearSelectionIfNotPresent(present map[string]bool) {
	p.selectedMu.Lock()
	sel := p.selectedID
	p.selectedMu.Unlock()
	if sel == "" || present[sel] || p.discovery.IsManual(sel) {
		return
	}
	p.selectedMu.Lock()
	cleared := p.selectedID == sel
	if cleared {
		p.selectedID = ""
	}
	p.selectedMu.Unlock()
	if cleared {
		p.codec.Notify("node/selection-changed", SelectedResult{ID: ""})
	}
}

// PriorityList returns a copy of the current scheduler-supplied priority order.
func (p *Proxy) PriorityList() []string {
	p.priorityMu.RLock()
	defer p.priorityMu.RUnlock()
	return append([]string(nil), p.priority...)
}

// SetPriority stores the auto-routing priority order (highest first) and returns
// the number of ids stored. The list is kept verbatim — unknown ids are retained
// (a node may appear in discovery later) and only consulted at request time. An
// empty list clears the scheduler's influence.
func (p *Proxy) SetPriority(nodes []string) int {
	return p.SetPrioritySnapshot(schedulerwire.Priority{Nodes: nodes})
}

// SetPrioritySnapshot replaces the scheduler baseline and clears optimistic
// reservations made against the previous snapshot. Nodes-only callers remain
// valid: a missing rank supplies zero pending and GPU-pressure baselines.
func (p *Proxy) SetPrioritySnapshot(priority schedulerwire.Priority) int {
	cleaned := append([]string(nil), priority.Nodes...)
	pending := make(map[string]int, len(priority.Ranks))
	gpuPressure := make(map[string]int, len(priority.Ranks))
	for _, rank := range priority.Ranks {
		if rank.ID == "" {
			continue
		}
		if rank.Pending < 0 {
			rank.Pending = 0
		}
		if rank.GPUPressure < 0 {
			rank.GPUPressure = 0
		} else if rank.GPUPressure > schedulerwire.MaxGPUPressure {
			rank.GPUPressure = schedulerwire.MaxGPUPressure
		}
		pending[rank.ID] = rank.Pending
		gpuPressure[rank.ID] = rank.GPUPressure
	}

	p.priorityMu.Lock()
	p.priority = cleaned
	p.priorityPending = pending
	p.priorityGPUPressure = gpuPressure
	p.priorityReservations = make(map[string]int)
	p.priorityMu.Unlock()
	return len(cleaned)
}

// replaceSubscribed replaces the proxy's relay-fed routing overlay from a
// discovery:nodes snapshot: it projects every node advertising lm with a dialable
// IP into the overlay (dropping the rest) and clears a user selection pinned to a
// node that's no longer routable. The broker sends the full filtered set on every
// change, so this is a wholesale replace, not a per-node apply — a departed node
// is simply absent from the next snapshot.
func (p *Proxy) replaceSubscribed(params json.RawMessage) {
	var res noderec.GetNodesResult
	if err := json.Unmarshal(params, &res); err != nil {
		slog.Warn("invalid discovery:nodes snapshot", "err", err)
		return
	}
	nodes := make([]Node, 0, len(res.Nodes))
	present := make(map[string]bool, len(res.Nodes))
	for _, dn := range res.Nodes {
		n, ok := subscribedToNode(dn)
		if !ok {
			continue
		}
		nodes = append(nodes, n)
		present[n.ID] = true
	}
	discovered, updated, removed := p.discovery.SetSubscribed(nodes)
	// Surface the relay-fed set to the client as node/* events — the signal a
	// consumer (the UI) uses to show which peers run this engine — mirroring how
	// manual nodes are announced. Without this the routing overlay updates
	// silently and peers appear engine-less. A node dropping out is also the
	// proxy's "this upstream is gone" signal, surfaced through the errors
	// pipeline (the broker forwards these to nvpair-errors); a re-appearance clears
	// it. NodeID/Timestamp are left unset so the broker stamps the authoritative
	// values.
	for _, n := range discovered {
		p.codec.Notify("node/discovered", n.withPrimaryIP())
		if err := p.codec.Notify("errors:clear", errors.ClearParams{ID: upstreamUnreachableID(n.ID)}); err != nil {
			slog.Debug("failed to send errors:clear", "node", n.ID, "err", err)
		}
	}
	for _, n := range updated {
		p.codec.Notify("node/updated", n.withPrimaryIP())
	}
	for _, n := range removed {
		p.codec.Notify("node/removed", n.withPrimaryIP())
		if err := p.codec.Notify("errors:report", errors.ServiceError{
			ID:       upstreamUnreachableID(n.ID),
			Message:  fmt.Sprintf("Upstream node %q is no longer reachable (dropped from discovery)", n.Host),
			Severity: "warning",
			Action:   "none",
		}); err != nil {
			slog.Debug("failed to send errors:report", "node", n.ID, "err", err)
		}
	}
	p.clearSelectionIfNotPresent(present)
}

// upstreamUnreachableID is the canonical ServiceError id for an upstream the
// proxy no longer sees in discovery. Kept as a single function so the report and
// clear can't drift (nvpair-errors matches by literal id).
func upstreamUnreachableID(nodeID string) string {
	return "lmstudio-proxy:upstream-unreachable:" + nodeID
}

// subscribedToNode projects a relay DirectoryNode onto the proxy's routable Node
// for the lm service, returning false when the node doesn't advertise lm or has
// no dialable address. The engine port comes from the lm service key (the real
// LM Studio port the broker's engine poller registered, not the proxy's listen
// port).
func subscribedToNode(n noderec.DirectoryNode) (Node, bool) {
	if n.IP == "" {
		return Node{}, false
	}
	// A node may advertise several OpenAI engine keys at once. They all carry the
	// same value — the port of that node's one OpenAI-compatible proxy — so the
	// node projects to a single routable entry whose per-engine model attribution
	// records which engine actually serves what.
	port := 0
	byEngine := make(map[string][]string, len(openaiEngines))
	for _, e := range openaiEngines {
		svc, ok := n.Services[e.Service]
		if !ok {
			continue
		}
		if port == 0 {
			port = svc.Port
		}
		byEngine[e.Name] = append([]string(nil), n.EngineModels(e.Name)...)
	}
	if port == 0 {
		return Node{}, false
	}
	// Key routing by the stable per-host UUID, not the hostname: candidate ids,
	// scheduledOn, node selection, and the scheduler's priority list are all this
	// value, so routing survives a PC rename and never conflates two same-named
	// machines. Host stays the hostname (display / dial name). A relay
	// DirectoryNode always carries a hostUuid (the scanner guarantees it at the
	// browse boundary), so there is no name fallback here.
	return Node{
		ID:   n.HostUUID,
		Host: n.Name,
		Port: port,
		// The node's whole ranked address list, not just its canonical one: a
		// multi-homed peer's best address from its own vantage point may be a
		// direct-connect link this host cannot reach, and routing needs somewhere
		// to fail over to when that happens.
		Addresses:   n.CandidateIPs(),
		TXT:         n.AddressTXT(),
		IP:          n.IP,
		ClusterUUID: n.ClusterUUID,
		// Filter on this node's OpenAI-engine models only, not the cross-engine
		// union, so a model a multi-engine node serves solely via Ollama isn't
		// accepted as an owner here (each key falls back to the union for a peer
		// that sends no attribution — see DirectoryNode.EngineModels).
		Models:         unionModels(byEngine),
		ModelsByEngine: byEngine,
	}, true
}

func (p *Proxy) readLoop(ctx context.Context) error {
	for {
		msg, err := p.codec.Read()
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			log.Printf("JSON-RPC read error: %v", err)
			continue
		}
		p.handleMessage(msg)
	}
}

func (p *Proxy) handleMessage(msg *Message) {
	if msg.Method == applog.SetLevelMethod {
		resolved, err := applog.HandleSetLevelParams(msg.Params)
		if msg.IsRequest() {
			if err != nil {
				p.codec.RespondError(msg.ID, -32602, err.Error())
				return
			}
			p.codec.Respond(msg.ID, map[string]string{"level": resolved})
		}
		if err != nil {
			slog.Warn("log/set-level rejected", "err", err)
		} else {
			slog.Info("log level changed", "level", resolved)
		}
		return
	}

	switch msg.Method {
	case noderec.NotifyNodes:
		p.replaceSubscribed(msg.Params)
		return
	}

	if !msg.IsRequest() {
		if msg.IsNotification() {
			log.Printf("ignoring incoming notification: %s", msg.Method)
		}
		return
	}

	switch msg.Method {
	case "nodes/list":
		nodes := p.discovery.Nodes()
		if err := p.codec.Respond(msg.ID, NodesResult{Nodes: nodes}); err != nil {
			log.Printf("failed to respond to nodes/list: %v", err)
		}

	case "node/select":
		var params SelectParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			p.codec.RespondError(msg.ID, -32602, "invalid params: expected {\"id\": \"...\"}")
			return
		}
		if params.ID != "" {
			found := false
			for _, n := range p.discovery.Nodes() {
				if n.ID == params.ID {
					found = true
					break
				}
			}
			if !found {
				p.codec.RespondError(msg.ID, -32602, fmt.Sprintf("node %q not found", params.ID))
				return
			}
		}
		p.SetSelected(params.ID)
		log.Printf("node selection changed to %q", params.ID)
		if err := p.codec.Respond(msg.ID, SelectedResult{ID: params.ID}); err != nil {
			log.Printf("failed to respond to node/select: %v", err)
		}
		p.codec.Notify("node/selection-changed", SelectedResult{ID: params.ID})

	case "node/selected":
		if err := p.codec.Respond(msg.ID, SelectedResult{ID: p.SelectedID()}); err != nil {
			log.Printf("failed to respond to node/selected: %v", err)
		}

	case "node/set-priority":
		var params schedulerwire.Priority
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			p.codec.RespondError(msg.ID, -32602, "invalid params: expected {\"nodes\": [\"id\", ...], \"ranks\": [...]}")
			return
		}
		count := p.SetPrioritySnapshot(params)
		log.Printf("priority snapshot set (%d nodes, %d ranks): %v", count, len(params.Ranks), params.Nodes)
		if err := p.codec.Respond(msg.ID, map[string]int{"count": count}); err != nil {
			log.Printf("failed to respond to node/set-priority: %v", err)
		}

	case "set-port":
		var params struct {
			Port int `json:"port"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			p.codec.RespondError(msg.ID, -32602, "invalid params: expected {\"port\": <int>}")
			return
		}
		if params.Port < 1 || params.Port > 65535 {
			p.codec.RespondError(msg.ID, -32602, "port must be between 1 and 65535")
			return
		}
		if err := p.setPort(params.Port); err != nil {
			p.codec.RespondError(msg.ID, -32000, err.Error())
			return
		}
		if err := p.codec.Respond(msg.ID, ReadyParams{Version: Version, Port: params.Port}); err != nil {
			log.Printf("failed to respond to set-port: %v", err)
		}

	case "node/add-manual":
		var node Node
		if err := json.Unmarshal(msg.Params, &node); err != nil {
			p.codec.RespondError(msg.ID, -32602, "invalid params: expected {\"id\",\"host\",\"port\",\"addresses\"}")
			return
		}
		if node.ID == "" || node.Port == 0 || len(node.Addresses) == 0 {
			p.codec.RespondError(msg.ID, -32602, "id, port, and at least one address are required")
			return
		}
		// engine names which OpenAI engine on that host this address serves.
		// LM Studio and vLLM listen on different ports, so a node running both is
		// added once per engine and the entries must not collide.
		if !isOpenAIEngine(node.Engine) {
			p.codec.RespondError(msg.ID, -32602, "engine must be one of "+strings.Join(engineNames(), ", "))
			return
		}
		// A manual node's whole inventory belongs to the engine it was added for.
		node.ModelsByEngine = map[string][]string{node.Engine: append([]string(nil), node.Models...)}
		added := p.discovery.AddManual(node)
		if err := p.codec.Respond(msg.ID, map[string]bool{"added": added}); err != nil {
			log.Printf("failed to respond to node/add-manual: %v", err)
		}
		if added {
			log.Printf("manual node added: %s/%s (%s:%d)", node.Engine, node.ID, node.Addresses[0], node.Port)
			p.codec.Notify("node/discovered", node.withPrimaryIP())
		} else {
			log.Printf("manual node updated: %s/%s (%s:%d)", node.Engine, node.ID, node.Addresses[0], node.Port)
			p.codec.Notify("node/updated", node.withPrimaryIP())
		}

	case "node/remove-manual":
		var params struct {
			ID     string `json:"id"`
			Engine string `json:"engine"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			p.codec.RespondError(msg.ID, -32602, "invalid params: expected {\"id\": \"...\", \"engine\": \"...\"}")
			return
		}
		if !isOpenAIEngine(params.Engine) {
			p.codec.RespondError(msg.ID, -32602, "engine must be one of "+strings.Join(engineNames(), ", "))
			return
		}
		removed := p.discovery.RemoveManual(params.Engine, params.ID)
		if err := p.codec.Respond(msg.ID, map[string]bool{"removed": removed}); err != nil {
			log.Printf("failed to respond to node/remove-manual: %v", err)
		}
		// The node itself is only gone once its last engine's entry is dropped;
		// until then it is still a routing target for the other engine, so the
		// selection and the node/removed event must not fire early.
		if removed && !p.discovery.IsManual(params.ID) {
			log.Printf("manual node removed: %s", params.ID)
			p.selectedMu.Lock()
			if p.selectedID == params.ID {
				p.selectedID = ""
				p.selectedMu.Unlock()
				p.codec.Notify("node/selection-changed", SelectedResult{ID: ""})
			} else {
				p.selectedMu.Unlock()
			}
			p.codec.Notify("node/removed", Node{ID: params.ID})
		}

	case "node/set-local-backend":
		var b localBackend
		if err := json.Unmarshal(msg.Params, &b); err != nil {
			p.codec.RespondError(msg.ID, -32602, "invalid params: expected {\"engine\",\"host\",\"port\",\"healthy\"}")
			return
		}
		if !isOpenAIEngine(b.Engine) {
			p.codec.RespondError(msg.ID, -32602, "engine must be one of "+strings.Join(engineNames(), ", "))
			return
		}
		p.setLocalBackend(b)
		slog.Info("local backend updated", "engine", b.Engine, "host", b.Host, "port", b.Port, "healthy", b.Healthy)
		if err := p.codec.Respond(msg.ID, map[string]bool{"ok": true}); err != nil {
			log.Printf("failed to respond to node/set-local-backend: %v", err)
		}

	case "shutdown":
		if err := p.codec.Respond(msg.ID, nil); err != nil {
			log.Printf("failed to respond to shutdown: %v", err)
		}
		log.Println("shutdown requested via JSON-RPC")
		p.cancel()

	default:
		if err := p.codec.RespondError(msg.ID, -32601, fmt.Sprintf("method not found: %s", msg.Method)); err != nil {
			log.Printf("failed to send error response: %v", err)
		}
	}
}
