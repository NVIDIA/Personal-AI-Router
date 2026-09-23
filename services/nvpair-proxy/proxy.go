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
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/cors"
	"nvpair-shared/engines"
	"nvpair-shared/ingressauth"
	"nvpair-shared/netmon"
	"nvpair-shared/netpick"
	"nvpair-shared/nodeactivity"
	"nvpair-shared/noderec"
	"nvpair-shared/schedulerwire"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
// See versions.json at the repo root for the source of truth.
var Version = "dev"

type ReadyParams struct {
	Version string `json:"version"`
	Port    int    `json:"port"`
}

// ErrorParams is sent as a JSON-RPC "error" notification when a facade hits a
// bring-up condition it wants to surface to the orchestrator. Code is a short
// machine-readable tag ("bind-failed" today); Message is a human-friendly
// string suitable for an error bar.
//
// It is no longer a pre-exit notice: a failed bind ends one facade's enable,
// not the process. The broker also learns this from the enable response, which
// carries codeFacadeBindFailed; this notification is what lets it choose the
// fallback port before that response is dispatched.
type ErrorParams struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Port    int    `json:"port,omitempty"`
}

// codeFacadeBindFailed is the JSON-RPC error code for a facade/enable that
// failed only because the port was taken. The broker matches on it to retry on
// a fallback port; see enableProxyFacadeWithFallback in nvpair-ui-broker. It
// sits in the implementation-defined -32000..-32099 server range.
const codeFacadeBindFailed = -32010

type NodesResult struct {
	Nodes []Node `json:"nodes"`
}

type SelectParams struct {
	ID string `json:"id"`
}

type SelectedResult struct {
	ID string `json:"id"`
}

// Request events are facade-scoped: they name an engine's method, path and
// target, so they go out addressed like every other facade notification. They
// were unaddressed until the process began hosting several facades, at which
// point the broker's process-scoped router — which claims only workload and
// node-activity methods — silently dropped them instead of relaying them to
// <engine>-proxy: subscribers.
//
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
// which for streaming Ollama responses is dominated by token
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
	workloadSubmittedMethod = "workload:submitted"
	workloadStartedMethod   = "workload:started"
	workloadCompletedMethod = "workload:completed"
	workloadErroredMethod   = "workload:errored"
)

// isInferenceRequest reports whether a request should be tracked as a cluster
// workload. Health checks, model listings and other control traffic are
// deliberately excluded so we don't flood the cluster with non-inference
// noise; the engine's profile declares which paths qualify.
//
// Kept a free function taking the profile rather than a method, so the
// classification is testable without constructing a Proxy.
func isInferenceRequest(p engineProfile, method, path string) bool {
	role, ok := p.roleFor(method, path)
	return ok && role == roleInferencePOST
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
	// Seq counts this workload's events, from 1, in emission order.
	//
	// The workload-manager's inter-node dedup is a permanent set, so an event
	// that repeats an earlier (state, scheduledOn) pair is indistinguishable
	// from a redelivery and is dropped by every peer. The retry loop produces
	// exactly that routinely: queued on A, placement cleared between attempts,
	// then queued on A again. Peers kept the interim unplaced record while the
	// job was running on A, so their schedulers stopped counting it against the
	// node actually doing the work.
	//
	// Sequencing each event makes the dedup exact without weakening it: a
	// broadcast retry resends an identical frame, sequence included, so a true
	// redelivery is still suppressed.
	Seq int64 `json:"seq"`
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
	// facadeMu guards facades, which is empty until the broker enables one. The
	// process starts with no engine presence at all, so every reader has to
	// tolerate a miss rather than assume a facade exists.
	//
	// Keyed by engine because one process hosts every enabled engine's facade.
	// Each owns its own listeners, ports and dialect; what they share is this
	// host's transports, workload stream, and reservation map. See facade.go.
	facadeMu sync.Mutex
	facades  map[string]*facade

	// serveCtx is the process lifetime, and is what a facade's HTTP servers use
	// as their BaseContext. It must not be an enable request's context: that one
	// is cancelled when the request returns, which would cancel the context of
	// every request the facade later serves.
	serveCtx context.Context

	codec  *Codec
	cancel context.CancelFunc

	// mesh is this node's cluster mTLS trust fabric, loaded from --cluster-dir.
	// nil = unclustered: the LAN TLS ingress accepts nothing and the node does
	// only loopback-plaintext local routing. Read-only after startup.
	mesh *clustertrust.Mesh

	// lanAuth is the opt-in API-key gate for non-loopback plaintext callers
	// (nvpair-shared/ingressauth). nil or disabled: plaintext is loopback-only.
	// Set once at startup; the gate re-reads its own key file on demand. It
	// lives on Proxy, not on a facade: the key is one credential for the node,
	// so every enabled engine's facade enforces the same gate identically.
	lanAuth *ingressauth.Gate

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

	// appliedPriorityGeneration is the newest snapshot generation applied, so a
	// redelivered or superseded one cannot clear reservations twice. See
	// SetPrioritySnapshot.
	appliedPriorityGeneration uint64

	// transportMu guards the long-lived HTTP transports reused across forwards
	// and model-list fetches. Allocating a new http.Transport per request
	// defeats connection pooling and leaks idle sockets until GC.
	//
	// Shared across facades: the pool is keyed by peer, not by engine, so a
	// second copy would double the connections this node opens to every peer.
	transportMu    sync.Mutex
	plainTransport *http.Transport
	peerTransports map[string]*http.Transport

	// runID is a per-process nonce minted at startup and stamped on every
	// workload this proxy emits. Shared across facades is correct: a workload's
	// identity is (originatedFrom, engine, runId, id), so engine already
	// separates two facades' records and only the process needs the nonce. It
	// exists because each facade's request counter restarts at 1, so without it
	// a reused id after a restart would collide in the broker's store.
	runID string
}

// NewProxy builds a facade-less process host. Facades arrive via
// enableFacade, so nothing is listening when this returns.
func NewProxy(codec *Codec) *Proxy {
	return &Proxy{
		codec:    codec,
		runID:    newRunID(),
		activity: nodeactivity.NewReporter(activityReportInterval),
	}
}

// facadeFor resolves the facade an addressed message names, or nil when that
// engine has no facade enabled here.
//
// An unaddressed facade-scoped message resolves to nothing: an empty engine is
// not a key. Falling back to "the only facade" would misroute as soon as a
// second one exists, which is the bug addressing is here to prevent.
func (p *Proxy) facadeFor(engine string) *facade {
	p.facadeMu.Lock()
	defer p.facadeMu.Unlock()
	return p.facades[engine]
}

// enabledFacades snapshots the live facades so a caller can iterate without
// holding facadeMu across work that might take it again.
func (p *Proxy) enabledFacades() []*facade {
	p.facadeMu.Lock()
	defer p.facadeMu.Unlock()
	out := make([]*facade, 0, len(p.facades))
	for _, f := range p.facades {
		out = append(out, f)
	}
	return out
}

// requireFacade resolves the facade for an engine-scoped request, answering the
// caller itself when that engine has none. Every engine-scoped method needs it
// now that the process starts with no facade at all: the alternative is a nil
// dereference on a request that simply arrived early.
func (p *Proxy) requireFacade(msg *Message, engine string) (*facade, bool) {
	if f := p.facadeFor(engine); f != nil {
		return f, true
	}
	p.codec.RespondError(msg.ID, -32002,
		fmt.Sprintf("no facade is enabled for engine %q on this proxy", engine))
	return nil, false
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
	p.serveCtx = ctx
	defer cancel()

	// Keep the "is this address local?" set fresh as interfaces come and go,
	// so the loopback rewrite in nodeURL stays correct after VPN/dock changes
	// or a sleep/wake IP reassignment.
	startLocalAddrWatch(ctx)

	// No facade is brought up here: the process serves the JSON-RPC channel
	// first and binds only when the broker enables an engine. A bind failure is
	// therefore an enable failure, reported in that call's response, rather
	// than a process exit — which is what lets one engine lose a bind race
	// without taking the others down.
	err := p.readLoop(ctx)

	// The app is going away (stdin closed or ctx cancelled). Stop any
	// inference requests still in flight rather than letting them run to
	// completion: cancelling the proxy's root context propagates to every
	// in-flight request context — and thus the upstream reverse-proxy
	// connection — so the target Ollama sees the client disconnect and stops
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

// Retry bounds for a single inference request.
//
// The two are not redundant, because they bound different things. An attempt is
// consumed by a dispatch, never by a resolution: finding no eligible owner
// sends nothing, so maxDispatchAttempts governs dispatch failures while
// jobDeadline governs the wait for an owner to exist at all — a wait that costs
// no attempts and would otherwise be unbounded.
//
// Read only against dispatch failures the deadline looks unreachable, and the
// arithmetic says so: five attempts at a 120s first-content cap plus 15s of
// cumulative backoff span 615s, so every reschedule check before the last sees
// a clock under 600s. It is not dead — it is the only bound on the no-owner
// wait. Do not remove it as unreachable.
const maxDispatchAttempts = 5

// jobDeadline is a var (not a const) only so a test can shorten it; production
// never reassigns it.
var jobDeadline = 10 * time.Minute

// targetWatchInterval is how often an in-flight attempt rechecks that its
// target is still in discovery. It bounds detection latency only: the effect is
// to replace a full first-content budget spent waiting on a node that is
// already gone with a sub-second abort.
//
// A var (not a const) only so a test can shorten it; production never
// reassigns it.
var targetWatchInterval = 500 * time.Millisecond

// defaultRetryBackoff is the delay before a retry, indexed by the number of
// dispatches already made and clamped to the last entry. It also paces the poll
// while no owner is available.
var defaultRetryBackoff = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}

// retryBackoff is the schedule in force. A var (not a const) only so a test can
// shorten it; production never reassigns it. Naming the default separately lets
// a test that asserts the schedule itself put the real one back.
var retryBackoff = defaultRetryBackoff

// backoffFor returns the delay before the next dispatch, jittered and never
// longer than the time left before the deadline.
func backoffFor(dispatches int, remaining time.Duration) time.Duration {
	if len(retryBackoff) == 0 || remaining <= 0 {
		return 0
	}
	i := dispatches - 1
	if i < 0 {
		i = 0
	}
	if i >= len(retryBackoff) {
		i = len(retryBackoff) - 1
	}
	d := retryBackoff[i]
	// Jitter by up to ±25% so a burst of requests that all failed against the
	// same node does not retry in lockstep and re-collide. The clock's
	// nanosecond low bits are the entropy source: they differ between
	// concurrent handlers, and `rand` here is crypto/rand, whose interface is
	// far heavier than spreading a backoff warrants.
	if spread := int64(d) / 2; spread > 0 {
		d = time.Duration(int64(d) - spread/2 + time.Now().UnixNano()%(spread+1))
	}
	if d > remaining {
		d = remaining
	}
	return d
}

// waitBeforeRetry sleeps d, reporting false if the request was abandoned while
// waiting. A client that has gone away should not keep a retry budget alive:
// nobody is left to receive the answer, so the remaining attempts are better
// spent on work someone is waiting for.
func waitBeforeRetry(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// errFirstBodyTimeout is returned by awaitFirstBody when an upstream sent its
// headers but produced no content within the budget.
var errFirstBodyTimeout = stderrors.New("upstream sent no response body within the first-byte budget")

// errTargetGone is the reason an attempt was abandoned because its target left
// discovery. It replaces the bare "context canceled" the transport reports,
// which in a log is indistinguishable from the client hanging up.
var errTargetGone = stderrors.New("target node left discovery mid-request")

// Attempt outcomes for attemptClaim.
const (
	attemptPending int32 = iota
	attemptCommitted
	attemptAbandoned
)

// attemptClaim resolves the race between committing an attempt and abandoning
// it because its target left discovery.
//
// Both can become true at once: the watcher's discovery check can pass while
// the commit path is already running, and between the first body byte arriving
// and the commit finishing that path does a synchronous notification write and
// takes two mutexes. A pair of channels cannot express which happened first —
// closing one says an event occurred, not that it won — so the outcome is one
// value that each side claims by compare-and-swap.
//
// Exactly one of commit and abandon succeeds, and callers must honor the
// result: a false from commit means this attempt is being cancelled and must
// not be served, and a false from abandon means the response is already
// committed and must not be torn down.
type attemptClaim struct {
	state atomic.Int32
}

// commit claims the attempt for the response. Idempotent, so an already
// committed attempt still reports true.
func (a *attemptClaim) commit() bool {
	return a.state.CompareAndSwap(attemptPending, attemptCommitted) ||
		a.state.Load() == attemptCommitted
}

// abandon claims the attempt for cancellation, reporting false when the
// response has already committed.
//
// A committed first byte wins on purpose. The byte is direct evidence that this
// node is serving this request, whereas an absence from discovery can be a
// transient announcement gap; cancelling then would truncate a working stream.
func (a *attemptClaim) abandon() bool {
	return a.state.CompareAndSwap(attemptPending, attemptAbandoned)
}

// settled reports whether either side has claimed the outcome.
func (a *attemptClaim) settled() bool {
	return a.state.Load() != attemptPending
}

// abandoned reports whether cancellation won.
func (a *attemptClaim) abandoned() bool {
	return a.state.Load() == attemptAbandoned
}

// bodyWithPeek re-attaches an already-read first byte ahead of the rest of an
// upstream body, while keeping Close bound to the original so the connection
// is still released.
type bodyWithPeek struct {
	io.Reader
	io.Closer
}

// awaitFirstBody blocks until the upstream produces the first byte of its
// response body, and returns that byte spliced back onto the front of the
// stream.
//
// It exists because response headers are not a reliable signal that an engine
// has started work, and the difference is engine-specific. LM Studio answers a
// streaming request with 200 and headers ~12ms after accepting it and then
// holds the connection open, silent, for the entire time the request waits
// behind other work — measured at 128s behind a single-slot generation. Ollama
// withholds headers until generation begins. Committing on headers therefore
// meant that on LM Studio the proxy bound itself to a node that had merely
// accepted the request: the header timeout could never fire, failover was
// impossible, and a node dying during the wait took the job with it.
//
// The first content byte is the signal both engines agree on, and it is a real
// one — no SSE keepalive or empty role delta arrives during the wait, so the
// byte means generation actually started.
//
// A read that ends in EOF with no bytes is an empty body, which is a complete
// (if unusual) response and commits. A genuine read error before any content
// is retryable, like any other pre-commit upstream failure.
func awaitFirstBody(body io.ReadCloser, budget time.Duration) (io.ReadCloser, error) {
	type peeked struct {
		b   []byte
		err error
	}
	// Buffered so the reader goroutine can always finish. On the timeout path
	// ReverseProxy closes resp.Body, which unblocks the pending Read.
	ch := make(chan peeked, 1)
	go func() {
		var one [1]byte
		n, err := body.Read(one[:])
		ch <- peeked{b: one[:n], err: err}
	}()

	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case p := <-ch:
		if len(p.b) == 0 && p.err != nil && !stderrors.Is(p.err, io.EOF) {
			return nil, p.err
		}
		return bodyWithPeek{Reader: io.MultiReader(bytes.NewReader(p.b), body), Closer: body}, nil
	case <-timer.C:
		return nil, errFirstBodyTimeout
	}
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

// firstBodyTimeout bounds how long awaitFirstBody waits for an engine to emit
// its first content byte. It matches proxyResponseTimeout, the budget the
// transport applies to headers, because from the caller's point of view the two
// bound the same thing: how long we wait for a node to start the work. Keeping
// it generous is deliberate — it also covers a cold model load, which is real
// work rather than a stall, and which cannot be told apart from a queue wait
// from outside the engine.
//
// It is a var (not a const) only so a test can shorten it; production never
// reassigns it.
var firstBodyTimeout = proxyResponseTimeout

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

// shutdown stops every facade and then releases the process-wide outbound
// transport pool. The pool is closed here rather than in facade.stopServing
// because it is shared: closing it per facade would drop another engine's
// pooled connections to every peer.
func (p *Proxy) shutdown(ctx context.Context) {
	// Every facade, then the shared transports: the pool is process-wide, so
	// closing it before a facade has drained would cut that facade's own
	// in-flight upstream connections.
	for _, f := range p.enabledFacades() {
		f.stopServing(ctx)
	}
	p.closeIdleTransports()
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
	id       string
	url      *url.URL
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

func (retrySignal) Error() string { return "nvpair-proxy: retry next candidate" }

type modelListItem struct {
	key    string
	digest string
	raw    json.RawMessage
}

type modelListResult struct {
	headers http.Header
	denied  bool
	// unavailable marks a candidate that never answered — unreachable or a
	// server error — as opposed to one that answered unusably. Only the former
	// is excluded from the CORS intersection.
	unavailable bool
	items       []modelListItem
	ok          bool
	err         error
}

// serveModelList queries every candidate concurrently and returns the engine's
// model-list envelope with duplicate model records removed. Results are merged
// in candidate order, not completion order, so duplicate metadata is
// deterministic while an unavailable peer cannot hide healthy inventories.
//
// role selects the wire dialect: which array the upstream envelope carries,
// which field identifies a record, and how the federated response is shaped.
func (f *facade) serveModelList(w http.ResponseWriter, r *http.Request, role routeRole, candidates []candidate) (int, error) {
	p := f.host
	openAI := role == roleModelListOpenAIGET
	writeJSON := func(status int, body []byte) {
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
		upstream, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
		if err != nil {
			results[i].err = err
			continue
		}
		upstream.Header = cors.FanoutHeaders(r.Header)
		upstream.Header.Del("Content-Length")
		if upstream.Header.Get("Accept") == "" {
			upstream.Header.Set("Accept", "application/json")
		}

		// A cluster-peer candidate is queried over mTLS to its promoted proxy;
		// self/manual candidates use the shared plain client.
		clientCopy := *modelListClient
		clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client := &clientCopy
		if cand.peerUUID != "" {
			client.Transport = p.candidateTransport(cand)
		}

		wg.Add(1)
		go func(i int, cand candidate, req *http.Request, client *http.Client) {
			defer wg.Done()
			resp, err := client.Do(req)
			if err != nil {
				f.targets.Forget(cand.id)
				results[i] = modelListResult{unavailable: true, err: err}
				return
			}
			defer resp.Body.Close()
			responseHeaders := cors.EndToEndHeaders(resp.Header)
			results[i].headers = responseHeaders
			if r.Header.Get("Origin") != "" && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || (resp.StatusCode == http.StatusOK && !cors.AllowsOrigin(responseHeaders, r.Header.Get("Origin")))) {
				results[i].denied = true
				return
			}
			if resp.StatusCode >= 500 {
				results[i] = modelListResult{unavailable: true, err: fmt.Errorf("upstream returned %s", resp.Status)}
				return
			}
			if resp.StatusCode != http.StatusOK {
				results[i].err = fmt.Errorf("upstream returned %s", resp.Status)
				return
			}
			body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelListBytes+1))
			if err != nil {
				results[i].err = err
				return
			}
			if len(body) > maxModelListBytes {
				results[i].err = fmt.Errorf("model list exceeds %d bytes", maxModelListBytes)
				return
			}
			var envelope struct {
				Models *[]json.RawMessage `json:"models"`
				Data   *[]json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(body, &envelope); err != nil {
				results[i].err = err
				return
			}
			models := envelope.Models
			if openAI {
				models = envelope.Data
			}
			if models == nil {
				results[i].err = fmt.Errorf("upstream response has no model array")
				return
			}
			items := make([]modelListItem, 0, len(*models))
			for _, raw := range *models {
				var identity struct {
					ID     string `json:"id"`
					Model  string `json:"model"`
					Name   string `json:"name"`
					Digest string `json:"digest"`
				}
				if err := json.Unmarshal(raw, &identity); err != nil {
					results[i].err = fmt.Errorf("invalid model record: %w", err)
					return
				}
				key := identity.ID
				if !openAI {
					key = identity.Model
					if key == "" {
						key = identity.Name
					}
					key = f.profile.normalizeModel(key)
				}
				if key == "" {
					results[i].err = fmt.Errorf("model record has no identity")
					return
				}
				items = append(items, modelListItem{key: key, digest: identity.Digest, raw: raw})
			}
			results[i] = modelListResult{items: items, ok: true, headers: responseHeaders}
		}(i, cand, upstream, client)
	}
	wg.Wait()

	var combinedHeaders http.Header
	if r.Header.Get("Origin") != "" {
		headers := make([]http.Header, 0, len(results))
		invalidInventory := false
		for _, result := range results {
			// An engine that answered and refused the origin is a real denial;
			// its peers must not vote it away.
			if result.denied {
				writeJSON(http.StatusForbidden, []byte(`{"error":"an engine denied access to the model list"}`))
				return http.StatusForbidden, fmt.Errorf("engine denied model list")
			}
			// An engine that never answered expressed no opinion. Its models
			// are dropped from the merged list below, so intersecting its
			// absent headers would let one unreachable node deny the browser
			// access to every reachable one.
			if result.unavailable {
				continue
			}
			// An engine that answered unusably is reachable and broken. Do not
			// quietly hand a browser a list that silently omits it.
			if !result.ok {
				invalidInventory = true
			}
			headers = append(headers, result.headers)
		}
		if len(headers) == 0 {
			writeJSON(http.StatusBadGateway, []byte(`{"error":"model inventory unavailable"}`))
			return http.StatusBadGateway, fmt.Errorf("model inventory unavailable")
		}
		var allowed bool
		combinedHeaders, allowed = cors.Combine(r, headers)
		if invalidInventory {
			if allowed {
				for key, values := range combinedHeaders {
					w.Header()[key] = values
				}
			}
			writeJSON(http.StatusBadGateway, []byte(`{"error":"model inventory unavailable"}`))
			return http.StatusBadGateway, fmt.Errorf("model inventory unavailable")
		}
		if !allowed {
			writeJSON(http.StatusForbidden, []byte(`{"error":"engines do not all permit access to the model list"}`))
			return http.StatusForbidden, fmt.Errorf("engines denied model list")
		}
	}

	success := false
	models := make([]json.RawMessage, 0)
	seen := make(map[string]modelListItem)
	for i, result := range results {
		if !result.ok {
			slog.Debug("model list candidate unavailable",
				"node_id", candidates[i].id, "target", candidates[i].url.Host, "err", result.err)
			continue
		}
		success = true
		for _, item := range result.items {
			if previous, duplicate := seen[item.key]; duplicate {
				if previous.digest != "" && item.digest != "" && previous.digest != item.digest {
					slog.Warn("conflicting model digests across candidates",
						"model", item.key, "first_digest", previous.digest,
						"other_digest", item.digest, "node_id", candidates[i].id)
				}
				continue
			}
			seen[item.key] = item
			models = append(models, item.raw)
		}
	}
	if !success {
		err := fmt.Errorf("no valid model list from %d candidate(s)", len(candidates))
		writeJSON(http.StatusServiceUnavailable, []byte(`{"error":"model inventory unavailable"}`))
		return http.StatusServiceUnavailable, err
	}
	var body []byte
	var err error
	if openAI {
		body, err = json.Marshal(struct {
			Object string            `json:"object"`
			Data   []json.RawMessage `json:"data"`
		}{Object: "list", Data: models})
	} else {
		body, err = json.Marshal(struct {
			Models []json.RawMessage `json:"models"`
		}{Models: models})
	}
	if err != nil {
		writeJSON(http.StatusInternalServerError, []byte(`{"error":"failed to encode model inventory"}`))
		return http.StatusInternalServerError, err
	}
	for key, values := range combinedHeaders {
		w.Header()[key] = values
	}
	writeJSON(http.StatusOK, body)
	return http.StatusOK, nil
}

func (f *facade) handleHTTP(w http.ResponseWriter, r *http.Request) {
	p := f.host
	start := time.Now()

	// Allocate the request ID up front so both code paths (rejection
	// and forward) can stamp the same value into their notification.
	// The rejection path never emits a Started event, so its ID won't
	// appear in any orchestrator in-flight map — that's fine; the
	// completion event still bumps the failed counter regardless of
	// whether a matching Started was seen.
	reqID := strconv.FormatUint(f.nextRequestID.Add(1), 10)

	// Parse the request's model before choosing a node. Model eligibility only
	// applies to inference routes; control endpoints retain their existing
	// routing behavior even when their JSON happens to contain a model field.
	bodyBytes, model := bufferBodyAndModel(r)
	isInf := isInferenceRequest(f.profile, r.Method, r.URL.Path)
	routingModel := ""
	if isInf {
		routingModel = model
	}
	candidates := f.resolveCandidates(routingModel)
	if cors.IsPreflight(r) {
		targets := make([]cors.Target, 0, len(candidates))
		for _, cand := range candidates {
			targets = append(targets, cors.Target{URL: cand.url, Transport: p.candidateTransport(cand)})
		}
		sc := &statusCapture{ResponseWriter: w, status: http.StatusOK}
		cors.ServePreflight(sc, r, targets)
		f.notify("proxy/request", RequestEvent{ID: reqID, Method: r.Method, Path: r.URL.Path, Target: "cluster", Status: sc.status, Duration: time.Since(start).Milliseconds()})
		return
	}
	// held is this request's claim on a node's capacity while it is in flight.
	// resMu guards it because failover re-points it from the ReverseProxy's
	// callbacks while the disconnect watcher may be releasing it.
	var (
		resMu sync.Mutex
		held  reservation
	)
	if isInf && model != "" {
		candidates, held = p.reserveCandidate(f, candidates)
	}
	// Released exactly once, whichever way the request ends: normal unwind,
	// client disconnect, or an early return below. Without this a node stays
	// counted as loaded until the next scheduler snapshot, so a steady trickle
	// of short requests makes it look busier than it is and drives dispatch
	// away from it.
	//
	// Both accessors release resMu by defer. The release itself runs from a
	// defer, so a resMu stranded by a panic in the move would deadlock this
	// request against its own cleanup.
	moveHeld := func(nodeID string) {
		resMu.Lock()
		defer resMu.Unlock()
		held = p.moveReservation(held, nodeID)
	}
	defer func() {
		resMu.Lock()
		defer resMu.Unlock()
		p.releaseReservation(held)
		held = reservation{}
	}()
	if role, ok := f.profile.roleFor(r.Method, r.URL.Path); ok && role.isModelList() {
		if len(candidates) > 0 {
			_ = f.notify("proxy/request-started", RequestStartedEvent{
				ID: reqID, Method: r.Method, Path: r.URL.Path, Target: "cluster",
			})
		}
		status, err := f.serveModelList(w, r, role, candidates)
		errText := ""
		if err != nil {
			errText = err.Error()
		}
		_ = f.notify("proxy/request", RequestEvent{
			ID: reqID, Method: r.Method, Path: r.URL.Path, Target: "cluster",
			Status: status, Duration: time.Since(start).Milliseconds(), Error: errText,
		})
		return
	}
	if len(candidates) == 0 {
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
		_ = f.notify("proxy/request", RequestEvent{
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

	// wlSeq numbers this workload's events. Every mutation of wl below happens
	// either before the watcher goroutine exists or under wlMu, so a plain
	// counter is enough; see Workload.Seq for why the sequence is needed.
	var wlSeq int64
	nextWlSeq := func() int64 { wlSeq++; return wlSeq }

	// Emit workload:submitted the moment the request is admitted, before any
	// dispatch. A burst of concurrent inference requests must surface as job
	// cards immediately — the upstream engine serializes work on a single GPU
	// slot, so a card that waited for the upstream response would leave every
	// queued job invisible until the node dequeued it, one at a time (a prior
	// regression). "queued" is what makes that visible without claiming the
	// engine is generating, which only becomes true at the commit point below.
	//
	// scheduledOn is left empty here: nothing has been dispatched yet, and the
	// scheduler counts pending work by scheduledOn, so naming a node we have
	// not tried would inflate its load. Each dispatch and each gap between
	// attempts re-points it.
	if isInf && model != "" {
		createdMs := start.UnixMilli()
		wl = &Workload{
			ID:        reqID,
			Model:     model,
			Engine:    f.profile.Name,
			RunID:     p.runID,
			State:     "queued",
			CreatedAt: createdMs,
			Seq:       nextWlSeq(),
		}
		p.emitWorkload(workloadSubmittedMethod, *wl)
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
			wl.Seq = nextWlSeq()
			snapshot := *wl
			wlMu.Unlock()
			// Only "completed" gets its own method; every other terminal state,
			// including "cancelled", rides workload:errored. The workload
			// manager does not validate method against state and consumers read
			// the state out of the payload, so a new terminal state needs no new
			// method on the wire.
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
				emitTerminal("cancelled", "client disconnected before completion")
			case <-finished:
			}
		}()
	}

	// committedSC is the statusCapture of the candidate we committed to
	// streaming; its wroteErr tells us after the fact whether the client write
	// failed (dead/half-open client) so we can mark the workload failed. It is
	// assigned at the commit point as well as after the loop, because the
	// post-loop assignment does not always run — see finalize below.
	var committedSC *statusCapture

	// finalize reports this request's outcome, and it is deferred because a
	// mid-copy error is never returned to us: ReverseProxy converts one into
	// panic(http.ErrAbortHandler) whenever it detects a real server, and that
	// panic unwinds straight past the end of this function. Reported inline,
	// both the terminal workload event and the proxy/request completion were
	// lost whenever an upstream died mid-stream while the client was healthy,
	// leaving the job "running" for the life of the broker. That is worse than
	// a stuck card: the scheduler counts queued and running by scheduledOn, so
	// the ghost permanently inflated that node's pending count and biased
	// routing away from a node whose only fault was a crashed engine.
	//
	// Client-side failures never had this problem. net/http cancels the
	// request context synchronously before a write error propagates
	// (checkConnErrorWriter), so the watcher above has always emitted by the
	// time the panic arrives. The upstream-died case is the one that had no
	// reporter at all.
	//
	// Registered after the reservation release above, so on unwind this runs
	// first and the node is still counted as loaded while we report.
	defer func() {
		aborted := recover()
		// An aborted copy is operational detail, not a verdict on the job. A
		// stream that already committed a 2xx is a completion whether or not
		// it reached its end, so this never feeds the terminal state below,
		// and proxyErr is left untouched for that reason.
		reportErr := proxyErr
		if aborted != nil && reportErr == "" {
			reportErr = "upstream stream aborted mid-response"
		}
		if committedSC != nil && finalStatus == 0 {
			// The panic skipped the post-loop assignment, but the committed
			// capture still carries the status the upstream sent.
			finalStatus = committedSC.status
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
			"err", reportErr,
		)

		_ = f.notify("proxy/request", RequestEvent{
			ID:       reqID,
			NodeID:   servedNodeID,
			Method:   r.Method,
			Path:     r.URL.Path,
			Target:   servedTarget,
			Status:   finalStatus,
			Duration: time.Since(start).Milliseconds(),
			TTFB:     ttfbMs,
			Error:    reportErr,
		})

		// Terminal workload transition pairs with the workload:started emitted
		// at forward time above. Cancellation, an upstream/transport error, a
		// failed client write (dead/half-open client), or any non-2xx status is
		// a failure; a clean 2xx is a completion. The Workload carries the same
		// id so the broker (and peers) can collapse the start/finish pair.
		// Routed through emitTerminal so the disconnect watcher and this path
		// emit exactly once.
		if wl != nil {
			switch {
			case r.Context().Err() != nil:
				// The request was cancelled before it finished — either the
				// client disconnected or, on shutdown, we cancelled it to stop
				// the in-flight inference. A mid-stream cancel never reaches
				// ErrorHandler (the 200 headers are already sent), so without
				// this branch it would be misreported as completed. (The
				// watcher above usually beats us to it; emitTerminal makes that
				// a no-op.) Cancelled rather than failed: nothing went wrong
				// here, the requester stopped waiting.
				emitTerminal("cancelled", "request cancelled before completion")
			case committedSC != nil && committedSC.wroteErr != nil:
				// The response committed but a write to (or flush toward) the
				// client failed — typically the idle deadline tripping on a
				// dead/half-open client. The same event as the branch above,
				// differing only in how we noticed: a client that vanished
				// without a FIN never cancels the context, so the write
				// deadline is what surfaces it. Classified identically.
				emitTerminal("cancelled", "client connection lost: "+committedSC.wroteErr.Error())
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

		if aborted != nil {
			// Preserve ReverseProxy's contract with http.Server, which recovers
			// ErrAbortHandler silently and closes the connection. Swallowing it
			// here would leave the client waiting on a response that will never
			// be finished or closed.
			panic(aborted)
		}
	}()

	// repointWorkload records where the job is currently placed: a node id while
	// an attempt is in flight, empty between attempts. The scheduler counts
	// pending work by scheduledOn, so clearing it is what stops a node we have
	// given up on from still looking busy. The state stays "queued" throughout,
	// because "running" means the engine is generating and the broker's store
	// would reject a return to "queued" as a backwards transition.
	repointWorkload := func(nodeID string) {
		if wl == nil {
			return
		}
		wlMu.Lock()
		if terminated || wl.ScheduledOn == nodeID {
			wlMu.Unlock()
			return
		}
		wl.ScheduledOn = nodeID
		wl.Seq = nextWlSeq()
		snapshot := *wl
		wlMu.Unlock()
		p.emitWorkload(workloadSubmittedMethod, snapshot)
	}

	// releaseHeld drops the capacity claim. Called before every backoff: the
	// reservation map is process-wide and shared with the other engine's
	// facade, so holding a claim on a node we have stopped using would push
	// that engine's traffic away from a node that is in fact free. It is the
	// same reasoning as clearing scheduledOn, applied to the proxy's own
	// estimate rather than the scheduler's.
	releaseHeld := func() {
		resMu.Lock()
		defer resMu.Unlock()
		p.releaseReservation(held)
		held = reservation{}
	}
	// takeHeld re-reserves for a fresh round, replacing any claim still held.
	// reserveCandidate both orders the round and takes the claim, so a retry
	// that re-resolves has to go back through it.
	takeHeld := func(cands []candidate) []candidate {
		resMu.Lock()
		defer resMu.Unlock()
		p.releaseReservation(held)
		var out []candidate
		out, held = p.reserveCandidate(f, cands)
		return out
	}

	// Retry loop. Each iteration dispatches to one candidate; when a round's
	// candidates are exhausted it backs off and re-resolves, so a node that
	// recovered or a model that finished pulling becomes eligible mid-retry.
	//
	// Bounded by maxDispatchAttempts for dispatch failures and by jobDeadline
	// for the whole pre-commit life. Both bounds are checked before every
	// dispatch — the deadline never truncates an attempt already in flight,
	// because killing one seconds from its first token and then failing the job
	// for being out of time would spend the entire wait and discard the result.
	//
	// Retrying is only possible before the first byte reaches the client; past
	// the commit point we are bound to that node, and a stream truncated from
	// there is the caller's to resume. proxy/request-started fires at the commit
	// so it names the node that actually served. The self-forward guard lives in
	// resolveCandidates.
	// The budget is for inference only. Everything not in the engine's route
	// table is forwarded verbatim, and some of those are state-changing: a
	// POST /api/pull starts a model download. shouldRetry treats any 5xx as
	// retryable regardless of route, so without this split a failed pull would
	// be replayed up to five times — including against the node that just took
	// the work, since re-resolution can pick it again.
	//
	// Non-inference therefore gets one pass over the resolved candidates: no
	// re-resolution, no backoff, no deadline.
	maxDispatches := maxDispatchAttempts
	retryRounds := true
	if !isInf {
		maxDispatches = len(candidates)
		retryRounds = false
	}

	deadline := start.Add(jobDeadline)
	dispatches := 0
	committed := false
	respondedWithError := false
	round := candidates
	next := 0

	for !committed {
		if dispatches >= maxDispatches {
			break
		}
		if retryRounds && !time.Now().Before(deadline) {
			break
		}
		// Abandon-now: the requester has gone, so there is nobody left to
		// receive an answer and the remaining attempts are better spent on work
		// someone is waiting for. The watcher has already emitted the terminal
		// as cancelled; the guard on the exhaustion response below keeps this
		// from also inventing a reason for a client that stopped listening.
		if r.Context().Err() != nil {
			break
		}
		if next >= len(round) {
			if !retryRounds {
				// Single pass: the candidate list is the whole budget.
				break
			}
			// Round exhausted, or no owner was available. Either way the job is
			// on no node right now, so drop both the placement and the capacity
			// claim before waiting.
			repointWorkload("")
			releaseHeld()
			if !waitBeforeRetry(r.Context(), backoffFor(dispatches, time.Until(deadline))) {
				break
			}
			round = f.resolveCandidates(routingModel)
			if isInf && model != "" {
				round = takeHeld(round)
			}
			next = 0
			if len(round) == 0 {
				// Every owner went away mid-flight. Not a rejection: keep
				// waiting for one to come back. This consumes no attempt, which
				// is why jobDeadline is the only bound on it.
				continue
			}
		}
		cand := round[next]
		next++
		dispatches++
		// lastPermitted means no further dispatch can happen, so this attempt's
		// failure is the client's answer. It is not "last candidate in the
		// round" any more: another round may follow.
		lastPermitted := dispatches >= maxDispatches ||
			(retryRounds && !time.Now().Before(deadline))
		last := lastPermitted
		repointWorkload(cand.id)
		// The claim follows the node we are about to try, not the one
		// reserveCandidate happened to pick for the round — an attempt can hold
		// a node for the whole first-content budget, and for that time it is
		// the node under load. A same-node move is a no-op.
		moveHeld(cand.id)

		// Each attempt runs on its own context, a child of the request's, so we
		// can abandon this dispatch without touching the request itself. The
		// distinction is load-bearing: the disconnect watcher and the terminal
		// classification both read r.Context(), the parent, so cancelling a
		// child cannot be mistaken for the client going away.
		attemptCtx, cancelAttempt := context.WithCancel(r.Context())
		// claim decides, once, whether this attempt commits or is abandoned;
		// see attemptClaim for why a pair of channels could not.
		claim := &attemptClaim{}

		// Watch the target while the attempt is pre-commit. A node that has
		// dropped out of discovery is not coming back to answer a request
		// already sent to it, and without this the attempt sat out the whole
		// first-content budget before failing over — while the broker had
		// already stamped the record failed from its own node-loss sweep, so
		// the UI said failed while the client still waited.
		//
		// Polling rather than subscribing keeps this out of Discovery's
		// contract, and the interval only bounds detection latency. Watching
		// stops at the commit point, because cancelling after that would kill a
		// stream that is working.
		//
		// Read the interval once, on this goroutine: the watcher must not
		// depend on a value that could change under it mid-attempt.
		watchEvery := targetWatchInterval
		go func() {
			t := time.NewTicker(watchEvery)
			defer t.Stop()
			for {
				select {
				case <-attemptCtx.Done():
					return
				case <-t.C:
					// The commit path may have claimed the outcome since the
					// last tick, in which case this attempt is no longer ours
					// to cancel.
					if claim.settled() {
						return
					}
					if f.discovery.Has(cand.id) {
						continue
					}
					if claim.abandon() {
						cancelAttempt()
					}
					return
				}
			}
		}()

		// ReverseProxy derives the outbound request from the one it is given,
		// so the attempt context has to travel on a copy. The replayed body
		// goes on the copy for the same reason.
		attemptReq := r.WithContext(attemptCtx)
		if bodyBytes != nil {
			attemptReq.Body = io.NopCloser(bytes.NewReader(bodyBytes))
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
				// Hold the commit until the engine actually produces content.
				// Only for inference: a control endpoint's headers mean what
				// they say, and the model-list fan-out has its own client.
				// Waiting here costs a non-streaming response nothing, since
				// its headers and body arrive together, so the request's own
				// stream flag never has to be consulted — which also avoids
				// guessing per-dialect defaults (Ollama streams by default,
				// the OpenAI routes do not).
				var peeked io.ReadCloser
				if isInf {
					body, err := awaitFirstBody(resp.Body, firstBodyTimeout)
					if err != nil {
						// This response must not commit, whether or not another
						// attempt is permitted. awaitFirstBody's reader is still
						// blocked on resp.Body, so committing would put two
						// readers on one stream — the peek could swallow a byte
						// the client never sees — and against a silent upstream
						// the copy would then block with nothing left to
						// interrupt it, hanging the request past every bound we
						// set.
						//
						// Returning an error is what releases that reader:
						// ReverseProxy closes resp.Body and calls ErrorHandler.
						proxyErr = err.Error()
						if !last {
							retry = true
							slog.Warn("proxy upstream produced no content, failing over",
								"id", reqID, "node_id", cand.id, "target", cand.url.Host,
								"path", r.URL.Path, "err", err)
							return retrySignal{}
						}
						slog.Warn("proxy upstream produced no content, retries exhausted",
							"id", reqID, "node_id", cand.id, "target", cand.url.Host,
							"method", r.Method, "path", r.URL.Path,
							"duration_ms", time.Since(start).Milliseconds(), "err", err)
						return err
					}
					peeked = body
				}
				// Claim the attempt before any commit side effect. Losing the
				// claim means the watcher has already cancelled this attempt,
				// so committing would stream through a context that is about to
				// die and truncate the response we just started.
				if !claim.commit() {
					proxyErr = errTargetGone.Error()
					if !last {
						retry = true
						slog.Warn("proxy target left discovery as the response committed, failing over",
							"id", reqID, "node_id", cand.id, "target", cand.url.Host,
							"path", r.URL.Path)
						return retrySignal{}
					}
					return errTargetGone
				}
				// The peeked byte goes back only once the commit is ours, so a
				// lost claim leaves the body untouched for ReverseProxy to close.
				if peeked != nil {
					resp.Body = peeked
				}
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
				// Preserve the upstream response, including absent CORS permissions.
				if !started {
					started = true
					_ = f.notify("proxy/request-started", RequestStartedEvent{
						ID:     reqID,
						NodeID: cand.id,
						Method: r.Method,
						Path:   r.URL.Path,
						Target: cand.url.Host,
					})
					// The claim already follows each dispatch, so this is a
					// no-op unless something reordered underneath us; it stays
					// as the belt-and-braces guarantee that the node that
					// actually served is the one counted as loaded.
					moveHeld(cand.id)
					// Record the commit here, not just after the loop: a copy
					// error aborts this handler by panic, and finalize needs to
					// know the response had committed (and on which capture) to
					// classify it.
					committedSC = sc
					// queued -> running: the engine is producing content, so
					// this is the first moment "running" is true. It also fixes
					// the placement on the node that actually served, which may
					// differ from the last node the queued updates named.
					// Guarded by wlMu against the disconnect watcher, and
					// skipped once terminated so a late transition cannot
					// resurrect a workload we have already finalized.
					if wl != nil {
						wlMu.Lock()
						if !terminated {
							startedMs := time.Now().UnixMilli()
							wl.State = "running"
							wl.StartedAt = &startedMs
							wl.ScheduledOn = cand.id
							wl.Seq = nextWlSeq()
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
				f.targets.Forget(cand.id)
				if !last {
					// Transport/dial error with candidates left: fail over.
					retry = true
					proxyErr = err.Error()
					slog.Warn("proxy upstream error, failing over",
						"id", reqID, "node_id", cand.id, "target", cand.url.Host,
						"path", r.URL.Path, "err", err)
					return
				}
				// No further dispatch is permitted: terminal, surface it.
				respondedWithError = true
				servedNodeID = cand.id
				servedTarget = cand.url.Host
				proxyErr = err.Error()
				slog.Warn("proxy upstream error, retries exhausted",
					"id", reqID, "node_id", cand.id, "target", cand.url.Host,
					"method", r.Method, "path", r.URL.Path,
					"duration_ms", time.Since(start).Milliseconds(), "err", err)
				body, mErr := json.Marshal(map[string]string{
					"error": "upstream error: " + err.Error(),
				})
				if mErr != nil {
					body = []byte(`{"error":"upstream error"}`)
				}
				// A node that answered but never produced content timed out
				// rather than failed to be reached, and 504 says so.
				status := http.StatusBadGateway
				if stderrors.Is(err, errFirstBodyTimeout) {
					status = http.StatusGatewayTimeout
				}
				ew.Header().Set("Content-Type", "application/json")
				ew.Header().Set("X-Content-Type-Options", "nosniff")
				ew.WriteHeader(status)
				ew.Write(body)
			},
		}

		proxy.ServeHTTP(sc, attemptReq)
		cancelAttempt()
		if claim.abandoned() {
			// Name the real reason rather than the bare "context canceled" the
			// transport reports, which reads identically to a client hangup.
			proxyErr = errTargetGone.Error()
		}
		if !retry {
			finalStatus = sc.status
			committedSC = sc
			committed = true
		}
	}

	// Nothing committed and no attempt wrote the client's answer, so the loop
	// ended between attempts: either the budget ran out or, more usually, every
	// owner went away and none came back before the deadline. The dispatch
	// paths answer for themselves — a final transport failure writes its own
	// 502 and a non-retryable status is passed through — so this is the only
	// outcome left without a response.
	//
	// An abandoned request is excluded: writing to a client that has gone
	// achieves nothing, and attributing its end to a missing node would be a
	// fabricated reason for something that was the requester's own choice.
	if !committed && !respondedWithError && r.Context().Err() == nil {
		// Name what the loop was actually doing when it ran out, which is what
		// the caller needs to know. The three cases are distinguishable: the
		// budget is gone; or the deadline passed while an owner was still there
		// to try, because slow attempts can reach 10 minutes before they reach
		// five dispatches; or the deadline passed with the candidate set empty,
		// which is the wait for an owner to come back.
		reason := "no node advertising the requested model became available before the retry deadline"
		switch {
		case dispatches >= maxDispatches:
			reason = "every dispatch attempt failed"
		case len(round) > 0:
			reason = "the retry deadline passed while dispatch attempts were still failing"
		}
		proxyErr = reason
		finalStatus = http.StatusServiceUnavailable
		slog.Warn("proxy request exhausted its retry budget",
			"id", reqID, "method", r.Method, "path", r.URL.Path,
			"dispatches", dispatches, "duration_ms", time.Since(start).Milliseconds(),
			"reason", reason)
		body, mErr := json.Marshal(map[string]string{"error": reason})
		if mErr != nil {
			body = []byte(`{"error":"retry budget exhausted"}`)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// Advisory only: the caller knows its own patience better than we do.
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write(body)
	}

	// Reporting happens in the deferred finalize above, so it survives the
	// ErrAbortHandler panic that a mid-copy error raises.
}

// resolveCandidates returns the ordered list of nodes to try for the current
// request. A model-bearing request first filters a request-local node copy to
// advertised owners. A user-selected eligible node then leads, followed by
// scheduler priority and stable ID fallback. The failover loop walks the
// resulting owner list until a node returns a usable response.
//
// A node that resolves to this proxy's own listen address is dropped
// (self-forward guard): with the proxy on Ollama's default :11434, a
// local-Ollama advertisement can otherwise point right back at us and loop.
//
// Returns an empty slice when no forwarding target is available; the caller
// treats that as the rejection path.
func (f *facade) resolveCandidates(model string) []candidate {
	p := f.host
	id := f.SelectedID()

	priority := p.PriorityList()

	selfPort, aliasBoundAddresses := f.selfAddresses()

	// Re-derive membership and pins before resolving so a cluster joined or left,
	// and a peer paired or removed, since the last request is reflected without a
	// restart: a removed peer stops being a routable candidate immediately, and a
	// freshly-paired one becomes one.
	p.mesh.Refresh()

	nodes := f.discovery.Nodes()
	known := len(nodes)
	if model != "" {
		owners := make([]Node, 0, len(nodes))
		for _, node := range nodes {
			if nodeAdvertisesModel(f.profile, node, model) {
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
	// routing to the same Ollama twice is wasteful.
	seenHost := make(map[string]bool, len(nodes))
	// placed tracks node IDs already considered so the priority-ordered and
	// fallback passes don't reconsider one (scheduler ordering).
	placed := make(map[string]bool, len(nodes))
	add := func(n Node) {
		placed[n.ID] = true
		// targetURL picks a reachable address for a multi-homed node (cached,
		// TCP-probed), falling back to the first candidate; nil only when the
		// node advertises no usable address.
		u := f.targetURL(n)
		if u == nil {
			return
		}
		peerUUID := ""
		switch {
		case isSelfTarget(u, selfPort) || isAnyAliasSelfTarget(u, aliasBoundAddresses):
			// Our own advertised endpoint (ol now points at this proxy). Serve
			// it from the explicit local backend — the loopback engine — rather
			// than dialing our own mTLS ingress, which would recurse. Ranking
			// still used this node's real (discovered) model list above.
			lb, ok := f.localBackendTarget()
			if !ok {
				slog.Debug("resolveCandidates: no local backend for self", "node_id", n.ID)
				return
			}
			u = lb
		case p.mesh.HasPin(n.ClusterUUID):
			// A pinned cluster peer: reach it only over mTLS to its promoted
			// proxy (the ol port now advertises the proxy, not the engine).
			// The pin is read from the live mesh refreshed above, not from the
			// relayed n.Trusted: that flag is the scanner's answer from whenever
			// it last saw this peer's mDNS record, so a peer discovered before
			// this node's pins were written stays false until its record next
			// changes. It is also strictly weaker than what we hold here — the
			// dial itself is gated on ClientTLSConfig finding the same pin — so
			// the relayed value can only ever disagree by being stale.
			u.Scheme = "https"
			peerUUID = n.ClusterUUID
		case f.discovery.IsManual(n.ID):
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
		// Defensive: the local backend must never resolve back to this proxy.
		if isSelfTarget(u, selfPort) || isAnyAliasSelfTarget(u, aliasBoundAddresses) {
			slog.Debug("resolveCandidates: skipping self-target node",
				"node_id", n.ID, "target", u.Host)
			return
		}
		if seenHost[u.Host] {
			return
		}
		seenHost[u.Host] = true
		out = append(out, candidate{
			id:       n.ID,
			url:      u,
			peerUUID: peerUUID,
		})
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

// reservation is one in-flight dispatch this proxy has made since the last
// scheduler snapshot, held so it can be released when the request ends.
//
// The generation is the snapshot the reservation was counted against. A
// snapshot supersedes every reservation taken before it — its pending counts
// already include that work — so a release carrying an older generation is
// dropped. Without that check a long request finishing after a snapshot would
// decrement a counter that had already been reset, pushing a node's estimated
// load below zero and making it look permanently idle.
type reservation struct {
	nodeID     string
	generation uint64
	// held distinguishes "no reservation was taken" from "a reservation on the
	// zero-value node", so release on the paths that never reserved is a no-op.
	held bool
}

// reserveCandidate atomically moves the least estimated loaded scheduler-listed
// candidate to the front of this request's failover list, and returns the
// reservation it took so the caller can release it when the request ends.
//
// The scheduler's pending count and GPU pressure form the authoritative
// baseline; reservations are local dispatches made since that snapshot arrived
// and have not necessarily completed the proxy→broker→scheduler→proxy feedback
// loop yet.
//
// Model eligibility was enforced before this function receives the list. An
// explicit node/select pin bypasses reservations, and unlisted/manual owners
// retain their existing fallback position.
//
// The reservation map is process-wide while the route pin is per facade, so
// this stays a host method and takes the facade whose request it is serving.
// Process-wide is the point: two facades bursting at once are competing for the
// same GPU, so a dispatch through either has to be visible to the other.
func (p *Proxy) reserveCandidate(f *facade, candidates []candidate) ([]candidate, reservation) {
	if len(candidates) == 0 {
		return candidates, reservation{}
	}
	if selectedID := f.SelectedID(); selectedID != "" {
		for _, cand := range candidates {
			if cand.id == selectedID {
				return candidates, reservation{}
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
		return candidates, reservation{}
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
		return candidates, reservation{}
	}

	chosen := candidates[bestIndex]
	p.priorityReservations[chosen.id]++
	if bestIndex > 0 {
		copy(candidates[1:bestIndex+1], candidates[:bestIndex])
		candidates[0] = chosen
	}
	return candidates, reservation{
		nodeID:     chosen.id,
		generation: p.appliedPriorityGeneration,
		held:       true,
	}
}

// releaseReservation drops an in-flight dispatch's reservation once its request
// has ended, so the node stops counting as loaded before the next snapshot
// arrives. Safe to call on a request that never reserved.
//
// A release from before the current snapshot is ignored: that snapshot already
// accounts for the work, and decrementing here would double-count the
// completion and leave the node looking idle.
func (p *Proxy) releaseReservation(r reservation) {
	if !r.held {
		return
	}
	p.priorityMu.Lock()
	defer p.priorityMu.Unlock()
	if r.generation != p.appliedPriorityGeneration {
		return
	}
	if p.priorityReservations[r.nodeID] <= 1 {
		// Deleted rather than left at zero so the map does not accumulate an
		// entry per node ever dispatched to.
		delete(p.priorityReservations, r.nodeID)
		return
	}
	p.priorityReservations[r.nodeID]--
}

// moveReservation transfers a reservation to the node failover actually landed
// on, so the node that refused the request stops carrying its load and the one
// now serving it starts.
//
// It returns the new reservation; the old one must not be released separately.
// A move across a snapshot boundary takes a fresh reservation rather than
// carrying the stale one forward, because the new snapshot has already
// superseded it.
func (p *Proxy) moveReservation(r reservation, nodeID string) reservation {
	if !r.held || r.nodeID == nodeID {
		return r
	}
	p.priorityMu.Lock()
	defer p.priorityMu.Unlock()
	if r.generation == p.appliedPriorityGeneration {
		if p.priorityReservations[r.nodeID] <= 1 {
			delete(p.priorityReservations, r.nodeID)
		} else {
			p.priorityReservations[r.nodeID]--
		}
	}
	if p.priorityReservations == nil {
		p.priorityReservations = make(map[string]int)
	}
	p.priorityReservations[nodeID]++
	return reservation{
		nodeID:     nodeID,
		generation: p.appliedPriorityGeneration,
		held:       true,
	}
}

// nodeAdvertisesModel reports whether a node's advertised inventory contains
// the requested model, under the engine's naming convention. Both sides are
// normalized so the answer agrees with the federated model list's dedupe key;
// if they disagreed the proxy could advertise a model it then refuses to route.
//
// Kept a free function taking the profile so the match is testable without a
// Proxy.
func nodeAdvertisesModel(p engineProfile, n Node, model string) bool {
	requested := p.normalizeModel(model)
	if requested == "" {
		return false
	}
	for _, available := range n.Models {
		if p.normalizeModel(available) == requested {
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
	return isLoopbackHost(host)
}

// isAliasSelfTarget reports whether u points to the alias listener that this
// process actually owns. The configured alias is not enough: a failed bind
// leaves another process owning that endpoint, and two distinct 127/8
// addresses may legitimately use the same port.
func isAliasSelfTarget(u *url.URL, boundAddress string) bool {
	if boundAddress == "" {
		return false
	}
	targetHost, targetPort, err := net.SplitHostPort(u.Host)
	if err != nil {
		return false
	}
	boundHost, boundPort, err := net.SplitHostPort(boundAddress)
	if err != nil || targetPort != boundPort {
		return false
	}
	return equalLoopbackHosts(targetHost, boundHost)
}

func isAnyAliasSelfTarget(u *url.URL, boundAddresses []string) bool {
	for _, address := range boundAddresses {
		if isAliasSelfTarget(u, address) {
			return true
		}
	}
	return false
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func equalLoopbackHosts(a, b string) bool {
	aLocalhost, bLocalhost := isLocalhostName(a), isLocalhostName(b)
	if aLocalhost || bLocalhost {
		if aLocalhost && bLocalhost {
			return true
		}
		other := a
		if aLocalhost {
			other = b
		}
		ip := net.ParseIP(other)
		return ip != nil && (ip.Equal(net.IPv4(127, 0, 0, 1)) || ip.Equal(net.IPv6loopback))
	}
	aIP, bIP := net.ParseIP(a), net.ParseIP(b)
	return aIP != nil && bIP != nil && aIP.IsLoopback() && bIP.IsLoopback() && aIP.Equal(bIP)
}

func isLocalhostName(host string) bool {
	return strings.EqualFold(strings.TrimSuffix(host, "."), "localhost")
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

// nodeCandidates returns the ordered, de-duplicated host:port targets for a node.
//
// Order comes from the node itself: netpick.Candidates keeps the node's published
// ranking, which it derived from evidence no observer has, and appends anything
// else it advertised. Re-sorting here by address class is what previously put a
// two-host direct-connect link ahead of a peer's real LAN address.
//
// Any local-interface address is rewritten to loopback (Ollama binds loopback
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
		if isLocalAddress(h) && !isLoopbackHost(h) {
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
	// dial loopback instead of our own external IP (Ollama binds loopback
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

// upstreamUnreachableID is the canonical ServiceError id for an upstream the
// proxy no longer sees in discovery. Kept as a single function so the report and
// clear can't drift (nvpair-errors matches by literal id).
func upstreamUnreachableID(p engineProfile, nodeID string) string {
	return p.ComponentName() + ":upstream-unreachable:" + nodeID
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
//
// It stamps its own generation rather than sending zero. An unversioned
// snapshot would clear the reservations without advancing the epoch, so a
// reservation taken before it would still match and release one taken after —
// the undercount the stamp exists to prevent.
//
// The stamp and the apply are one critical section. Reading the epoch, dropping
// the lock, and then applying would let two callers mint the same generation
// and have the second silently ignored by the guard.
func (p *Proxy) SetPriority(nodes []string) int {
	p.priorityMu.Lock()
	defer p.priorityMu.Unlock()
	return p.setPriorityLocked(schedulerwire.Priority{
		Generation: p.appliedPriorityGeneration + 1,
		Nodes:      nodes,
	})
}

// SetPrioritySnapshot replaces the scheduler baseline and clears optimistic
// reservations made against the previous snapshot. Nodes-only callers remain
// valid: a missing rank supplies zero pending and GPU-pressure baselines.
//
// Applying is idempotent per generation, and that is load-bearing rather than
// tidy. Clearing the reservations is the whole point of applying a snapshot —
// the new pending counts already account for those dispatches — so replaying a
// generation the proxy has already applied would discard reservations taken
// since, which are exactly the dispatches the scheduler cannot see yet. The
// broker can and does replay: a node/set-priority that times out may already
// have been applied.
//
// A generation is mandatory: zero is rejected at the RPC boundary and ignored
// here, because a snapshot that cleared the reservations without advancing the
// epoch would let one taken before it release one taken after.
func (p *Proxy) SetPrioritySnapshot(priority schedulerwire.Priority) int {
	p.priorityMu.Lock()
	defer p.priorityMu.Unlock()
	return p.setPriorityLocked(priority)
}

// setPriorityLocked is the body of SetPrioritySnapshot. Split out so a caller
// that has to mint a generation can do so in the same critical section that
// applies it. Caller holds priorityMu.
func (p *Proxy) setPriorityLocked(priority schedulerwire.Priority) int {
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

	// Unconditional, with no escape hatch for an unversioned snapshot. One used
	// to apply while leaving the epoch untouched, which reintroduced exactly
	// the undercount the stamp prevents: clear the map at epoch N, reserve
	// again, and a reservation from before the clear still matches N and
	// releases the new one. A generation of 0 now fails this comparison and is
	// ignored; both callers stamp, so it cannot arrive.
	if priority.Generation <= p.appliedPriorityGeneration {
		// Already applied, or superseded. Leave the baseline and the
		// reservations exactly as they are.
		return len(p.priority)
	}
	p.appliedPriorityGeneration = priority.Generation
	p.priority = cleaned
	p.priorityPending = pending
	p.priorityGPUPressure = gpuPressure
	p.priorityReservations = make(map[string]int)
	return len(cleaned)
}

// subscribedToNode projects a relay DirectoryNode onto the proxy's routable Node
// for the ol service, returning false when the node doesn't advertise ol or has
// no dialable address. The engine port comes from the ol service key (the real
// Ollama port the broker's engine poller registered, not the proxy's listen
// port).
func subscribedToNode(p engineProfile, n noderec.DirectoryNode) (Node, bool) {
	svc, ok := n.Services[p.DiscoveryService]
	if !ok || n.IP == "" {
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
		Port: svc.Port,
		// The node's whole ranked address list, not just its canonical one: a
		// multi-homed peer's best address from its own vantage point may be a
		// direct-connect link this host cannot reach, and routing needs somewhere
		// to fail over to when that happens.
		Addresses:   n.CandidateIPs(),
		TXT:         n.AddressTXT(),
		IP:          n.IP,
		ClusterUUID: n.ClusterUUID,
		// Filter on this node's Ollama models only, not the cross-engine union, so
		// a model that a dual-engine node serves solely via LM Studio isn't
		// accepted as an Ollama owner here (falls back to the union for a peer
		// that sends no attribution — see DirectoryNode.EngineModels).
		Models: append([]string(nil), n.EngineModels(p.Name)...),
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

// handleMessage dispatches one control-plane message.
//
// A panic here is contained rather than fatal. It used to be reasonable to let
// one crash the process: the process was one engine, so it took down only the
// engine whose message was being handled. Now it hosts every facade, so an
// unhandled panic while parsing one engine's request would drop every other
// engine's listener and the inference streaming through it.
//
// Contained, not swallowed: the panic is logged at error level with its stack
// and the method that caused it, and the caller is answered with an internal
// error rather than left waiting.
//
// Every critical section reachable from here releases its lock by defer, which
// unwinding runs. That is a precondition, not a nicety: a mutex stranded by a
// recovered panic would trade a contained crash for a deadlock across every
// facade — strictly worse than the crash it replaced. resolveCandidates takes
// httpMu on every request and stopServing takes it during teardown, so the
// facade bring-up path in facade.go is held to the same rule.
func (p *Proxy) handleMessage(msg *Message) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		slog.Error("recovered from a panic while handling a proxy request",
			"method", msg.Method, "panic", fmt.Sprint(recovered),
			"stack", string(debug.Stack()))
		if msg.IsRequest() {
			p.codec.RespondError(msg.ID, -32603, "internal error handling "+msg.Method)
		}
	}()

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

	// Facade-scoped traffic arrives addressed to its engine, because the broker
	// cannot know which facade a bare method is for. Split once here so every
	// case below dispatches on the bare method.
	engine, method := engines.SplitAddressedMethod(msg.Method)

	switch method {
	case noderec.NotifyNodes:
		// Dropped rather than queued when the addressed facade is not enabled:
		// the relay re-sends the full set on every change, and enabling
		// subscribes, so the next snapshot supersedes anything missed here.
		if f := p.facadeFor(engine); f != nil {
			f.replaceSubscribed(msg.Params)
		}
		return
	}

	if !msg.IsRequest() {
		if msg.IsNotification() {
			log.Printf("ignoring incoming notification: %s", msg.Method)
		}
		return
	}

	switch method {
	case "facade/enable":
		var params enableFacadeParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			p.codec.RespondError(msg.ID, -32602, "invalid params: expected {\"engine\",\"port\",\"aliasAddresses\",\"ignorePersistedPort\"}")
			return
		}
		result, err := p.enableFacade(params)
		if err != nil {
			// A bind failure is a normal outcome the broker acts on, not a
			// process-fatal one: it picks another port and asks again. Nothing
			// else on this process is disturbed, which is the whole reason
			// bring-up moved off argv.
			code := -32000
			if stderrors.Is(err, errFacadeBindFailed) {
				code = codeFacadeBindFailed
			}
			slog.Warn("facade/enable failed", "engine", params.Engine, "port", params.Port, "err", err)
			p.codec.RespondError(msg.ID, code, err.Error())
			return
		}
		slog.Info("facade enabled", "engine", result.Engine, "port", result.Port)
		if err := p.codec.Respond(msg.ID, result); err != nil {
			log.Printf("failed to respond to facade/enable: %v", err)
		}

	case "nodes/list":
		f, ok := p.requireFacade(msg, engine)
		if !ok {
			return
		}
		nodes := f.discovery.Nodes()
		if err := p.codec.Respond(msg.ID, NodesResult{Nodes: nodes}); err != nil {
			log.Printf("failed to respond to nodes/list: %v", err)
		}

	case "node/select":
		f, ok := p.requireFacade(msg, engine)
		if !ok {
			return
		}
		var params SelectParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			p.codec.RespondError(msg.ID, -32602, "invalid params: expected {\"id\": \"...\"}")
			return
		}
		if params.ID != "" {
			found := false
			for _, n := range f.discovery.Nodes() {
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
		f.SetSelected(params.ID)
		log.Printf("node selection changed to %q", params.ID)
		if err := p.codec.Respond(msg.ID, SelectedResult{ID: params.ID}); err != nil {
			log.Printf("failed to respond to node/select: %v", err)
		}
		_ = f.notify("node/selection-changed", SelectedResult{ID: params.ID})

	case "node/selected":
		f, ok := p.requireFacade(msg, engine)
		if !ok {
			return
		}
		if err := p.codec.Respond(msg.ID, SelectedResult{ID: f.SelectedID()}); err != nil {
			log.Printf("failed to respond to node/selected: %v", err)
		}

	case "node/set-priority":
		var params schedulerwire.Priority
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			p.codec.RespondError(msg.ID, -32602, "invalid params: expected {\"generation\": <uint>, \"nodes\": [\"id\", ...], \"ranks\": [...]}")
			return
		}
		// A generation is required, not optional. This method is relayed
		// verbatim from any client, and an unversioned snapshot would clear the
		// reservations without advancing the epoch — letting a reservation
		// taken before it release one taken after, which reads as an idle node
		// and attracts every later dispatch.
		if params.Generation == 0 {
			p.codec.RespondError(msg.ID, -32602, "generation is required and must be greater than 0")
			return
		}
		count := p.SetPrioritySnapshot(params)
		log.Printf("priority snapshot set (%d nodes, %d ranks): %v", count, len(params.Ranks), params.Nodes)
		if err := p.codec.Respond(msg.ID, map[string]int{"count": count}); err != nil {
			log.Printf("failed to respond to node/set-priority: %v", err)
		}

	case "set-port":
		f, ok := p.requireFacade(msg, engine)
		if !ok {
			return
		}
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
		if err := f.setPort(params.Port); err != nil {
			p.codec.RespondError(msg.ID, -32000, err.Error())
			return
		}
		if err := p.codec.Respond(msg.ID, ReadyParams{Version: Version, Port: params.Port}); err != nil {
			log.Printf("failed to respond to set-port: %v", err)
		}

	case "node/add-manual":
		f, ok := p.requireFacade(msg, engine)
		if !ok {
			return
		}
		var node Node
		if err := json.Unmarshal(msg.Params, &node); err != nil {
			p.codec.RespondError(msg.ID, -32602, "invalid params: expected {\"id\",\"host\",\"port\",\"addresses\"}")
			return
		}
		if node.ID == "" || node.Port == 0 || len(node.Addresses) == 0 {
			p.codec.RespondError(msg.ID, -32602, "id, port, and at least one address are required")
			return
		}
		added := f.discovery.AddManual(node)
		if err := p.codec.Respond(msg.ID, map[string]bool{"added": added}); err != nil {
			log.Printf("failed to respond to node/add-manual: %v", err)
		}
		if added {
			log.Printf("manual node added: %s (%s:%d)", node.ID, node.Addresses[0], node.Port)
			_ = f.notify("node/discovered", node.withPrimaryIP())
		} else {
			log.Printf("manual node updated: %s (%s:%d)", node.ID, node.Addresses[0], node.Port)
			_ = f.notify("node/updated", node.withPrimaryIP())
		}

	case "node/remove-manual":
		f, ok := p.requireFacade(msg, engine)
		if !ok {
			return
		}
		var params SelectParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			p.codec.RespondError(msg.ID, -32602, "invalid params: expected {\"id\": \"...\"}")
			return
		}
		removed := f.discovery.RemoveManual(params.ID)
		if err := p.codec.Respond(msg.ID, map[string]bool{"removed": removed}); err != nil {
			log.Printf("failed to respond to node/remove-manual: %v", err)
		}
		if removed {
			log.Printf("manual node removed: %s", params.ID)
			if f.clearSelectionOf(params.ID) {
				_ = f.notify("node/selection-changed", SelectedResult{ID: ""})
			}
			_ = f.notify("node/removed", Node{ID: params.ID})
		}

	case "node/set-local-backend":
		f, ok := p.requireFacade(msg, engine)
		if !ok {
			return
		}
		var b localBackend
		if err := json.Unmarshal(msg.Params, &b); err != nil {
			p.codec.RespondError(msg.ID, -32602, "invalid params: expected {\"engine\",\"host\",\"port\",\"healthy\"}")
			return
		}
		// The address decides which facade this applies to, so a payload naming
		// a different engine is a caller bug, not a preference. Accepting it
		// would point one engine's ingress and self-candidate at the other
		// engine's port — and the old log line, which echoed the payload,
		// would have named the wrong engine and hidden the cross-wire.
		if b.Engine != "" && b.Engine != engine {
			p.codec.RespondError(msg.ID, -32602, fmt.Sprintf(
				"params name engine %q but the request is addressed to %q", b.Engine, engine))
			return
		}
		if err := f.setLocalBackend(b); err != nil {
			p.codec.RespondError(msg.ID, -32602, err.Error())
			return
		}
		slog.Info("local backend updated",
			"engine", f.profile.Name, "host", b.Host, "port", b.Port, "healthy", b.Healthy)
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
