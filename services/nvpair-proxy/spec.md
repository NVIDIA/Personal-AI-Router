<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Microservice: Engine Proxy (`nvpair-proxy`)

> **Status: implemented.** House spec style (`nvpair-cluster-manager`,
> `nvpair-workload-manager`, `nvpair-engine-manager`, `nvpair-job-scheduler`).
> Where this file and `README.md` disagree, this file wins.

## 1. Purpose

Carries inference **to** the node that should serve it. One process hosts a
**facade** per enabled engine: each facade is an HTTP reverse proxy speaking its
engine's own dialect, filtering to nodes that advertise the requested model,
choosing among them, and failing over when one refuses.

The same process can also host the **cluster routing endpoint**: one loopback
listener speaking the OpenAI-compatible surface, routing a requested model name
to *any* (node, engine) pair that provides it and rewriting the body's `model` to
the identifier that engine expects. It is a second routing personality, not a
second proxy — it shares this process's transports, trust fabric, reservation
ledger, and workload stream, and adds one addressing class (§5.12).

It is a data plane, not a policy component. Ranking belongs to
`nvpair-job-scheduler`, and cluster membership and trust to
`nvpair-cluster-manager`. `nvpair-engine-manager` owns an engine's *install and
process* lifecycle; the per-request *workload* lifecycle events in §5.5 are this
process's own. What it owns beyond those events is the choice of *which
advertised owner receives this request*, and the local accounting that makes
that choice good under bursts.

### 1.1 What a caller is promised

The proxy aims to get every inference request **started** on some node that can
serve it, and spends a bounded number of dispatches doing so. Stated precisely:

> While budget remains (§5.1), a failure that happens *before* the engine begins
> producing output is retried rather than returned. Once output begins, the
> request is committed to that node and is the caller's to finish with.

Failures absorbed that way include a retryable status, a transport error, an
engine that answers its headers and then produces nothing, and a target that
leaves discovery mid-request. Waiting for an owner to *exist* costs no attempt
at all, so a node blinking out does not spend the budget.

Four things bound that aim, and a caller should design for them.

**Dispatches are bounded, elapsed time is not, and which bound ran out changes
what the caller gets.** Absorption holds only while budget remains. Past that
there are two shapes, both detailed in §5.4, and a third where no bound is
reached:

- **The dispatch budget ran out.** The final permitted attempt is never
  retried, so its own failure is the answer: the upstream's status and body
  passed through unchanged, or a `502` if it failed at the transport and a
  `504` if it answered and produced nothing.
- **The deadline passed between attempts, or no owner came back.** Nothing is
  in flight to answer, so the proxy generates a `503` with `Retry-After` that
  names which of those it was.
- **An attempt never ends.** A node that withholds its headers holds the
  attempt until it answers, the connection fails, the node leaves discovery, or
  the caller gives up (§5.1, §5.6). Nothing is returned until one of those
  happens.

In every case this is a persistent attempt to start, bounded in dispatches but
not in time — not an assurance that every submission eventually runs.

**"Started" is the guarantee, not "finished".** Commitment ends at the first
byte of response body (§5.2), so `completed` means the engine began generating,
not that a whole answer arrived.

**A truncated stream still reports `completed`**, for two reasons that compound.
To the caller it is the same situation either way — it holds partial output and
is the only party that can decide what to do with it, which is what the
retrying harness above this layer is for. And the proxy genuinely cannot tell a
truncated stream from a complete one: a streaming response carries no
`Content-Length`, and the end-of-turn marker is engine-specific, so recognising
it would mean parsing response bodies, which §9 forbids.

**A non-streaming request is the weakest case.** Its headers arrive only when
the response is complete, so "queued inside the engine" and "generating right
now" look identical from outside. The proxy puts no cap on that wait, so a long
generation is never restarted, but a node that stalls before responding is never
failed over for being slow either: the attempt lasts until the engine answers,
the connection fails, the node leaves discovery (for a manual node, only when
the user removes it), or the caller gives up. That is a genuine limitation, not
a safety margin (§5.6).

## 2. Scope

**In scope**

- One listener per enabled engine, serving loopback plaintext for local clients
  and pin-gated cluster mTLS for peers on the same port.
- Model-owner filtering, target precedence, and retryable failover.
- Aggregating the engine's model list across every routable candidate.
- Per-request workload lifecycle events.
- The process-wide reservation map that makes concurrent dispatch spread.
- The persisted per-engine port a user chose through `set-port`.
- The cluster routing endpoint: one loopback listener, any engine's owner, the
  body's `model` rewritten to what that engine calls it, and a generated
  inventory built from the same resolution that routes (§5.12).

**Out of scope**

- Deciding node rank (scheduler), membership or pinning (cluster-manager),
  engine install/start/stop (engine-manager).
- Any cryptography of its own: it consumes the trust fabric, never builds it.
- Model search or catalog browsing.
- Exposing the cluster routing endpoint to a network. It binds loopback only; a
  peer that needs to route across nodes runs its own router on its own loopback
  and hops over the dedicated engine endpoints, which are already pin-gated.
- Control-plane fan-out (`/api/tags`, `/api/show`, `/api/pull` and friends) on
  the router, and a cluster-scoped engine telemetry stream: each listed with its
  reason in §5.12.

## 3. Process model

The process starts with **no engine and no listener**. The broker then sends one
`facade/enable` per engine, carrying that engine's port and any alias addresses.

A flag cannot express this. The broker plans a different port for each engine —
Ollama's managed facade wants `:11434`, LM Studio's wants `:1234`, and
llama.cpp's managed facade wants `:8080`, and vLLM's wants `:8000`; any may be absent so the child keeps
its own persisted port — and a single-valued flag carries only one plan.

### 3.1 Why one process

Between scheduler snapshots a facade takes short-lived **reservations** for work
it has dispatched but that the scheduler has not yet observed. Those live in the
process, and every facade shares them. If each engine had its own proxy
process, none could see the others' reservations, so simultaneous bursts across
Ollama, LM Studio, llama.cpp, and vLLM could select the same node, each incorrectly
believing it was idle.
Sharing the map is the reason the engines share a process.

### 3.2 What sharing costs

**Shared fate on the process.** A crash takes every facade down; the supervisor
restarts them together and the crash is reported once, against the process.

Inside the process the boundaries are finer, and deliberately so:

| Failure | Blast radius |
| --- | --- |
| Bind race lost on enable | That engine only; reported so the broker can retry elsewhere |
| Port cannot be re-listened | That engine only; its ownership gate is released |
| Panic handling a request | That request only; logged with stack, answered as an internal error |
| Process crash or exit | Every facade |

The first three failures are contained within their listed boundaries. Only a
process crash or exit takes down every facade. Containment matters here in a way
it would not for a single-engine process: one engine's bind race must not take
another engine's working listener with it.

## 4. Addressing

Every facade-scoped message carries the engine it concerns, in both directions.
Without it the engine would be implied by which process a message travelled
through, which is precisely the property one process destroys.

The address is a bare engine id (`ollama:ready`), deliberately **not** the
component name (`ollama-proxy:ready`). The component form is how a client
addresses the component through the broker; the two meet in the broker's relay,
which takes one off and puts the other on.

| Class | Addressed? | Examples |
| --- | --- | --- |
| Facade-scoped | yes | `ready`, `error`, `node/*`, `errors:*`, `set-port`, `discovery:subscribe`, `discovery:nodes` |
| Process-scoped | no | `log/set-level`, `node/set-priority`, `workload:*`, `discovery:node-activity`, `cluster/set-aliases`, `router/set-enabled`, `router/status`, `router/get-aliases` |
| Bootstrap | names its engine in the payload | `facade/enable` |

`facade/enable` is unaddressed because it runs *before* the facade it names
exists. Only a known engine id counts as an address: plenty of unaddressed
methods contain a colon, so treating `errors:report` as engine `errors` would
strip a real method down to `report`.

An unaddressed facade-scoped message resolves to **no** facade. Falling back to
"the only facade" would work with one engine enabled and misroute silently with
two.

The cluster routing endpoint adds a class with no engine in it at all. Its
JSON-RPC surface (`cluster/set-aliases`, `router/set-enabled`,
`router/status`, `router/get-aliases`) is process-scoped, because the router is
not an engine and prefixing those names would make a caller guess an engine for
something that routes across all of them. Its HTTP surface addresses by **the model name in the request body**,
which the catalog resolves to a set of (node, engine, native identifier) triples
— so the address is not known until the name is resolved, and the same name can
address one machine today and three tomorrow (§5.12).

## 5. Request path

For a model-bearing inference request:

1. Filter a request-local discovery snapshot to nodes whose per-engine inventory
   advertises the requested model. Ollama normalizes the implicit `:latest` tag;
   LM Studio, llama.cpp, and vLLM ids match exactly. An empty owner set returns a local
   `502` without contacting an engine.
2. Order the eligible owners: explicit `node/select` pin, then the scheduler's
   priority list, then deterministic default ordering.
3. Reserve the least estimated-loaded scheduler-listed candidate and move it to
   the front (§6).
4. Forward, failing over through the remaining candidates on a retryable status
   or transport error. An upstream model `404` counts as retryable, because
   positive inventory can be stale.
5. When a round's candidates are exhausted, back off and re-resolve (§5.1),
   until the dispatch budget or the deadline runs out.

An ineligible manual selection cannot override the capability gate, and failover
never broadens to an excluded node.

The llama.cpp facade exposes `GET /models` and `GET /v1/models` as aliases for
the fleet's llama.cpp inventory. Both query every candidate's `GET /models`
concurrently and merge duplicate model ids into an OpenAI list envelope
(`{"object":"list","data":[...]}`). A selected node affects candidate ordering,
not inventory scope. Its inference routes remain `/v1/*`.

### 5.1 Retry bounds

| Bound | Value | Governs |
| --- | --- | --- |
| `maxDispatchAttempts` | 5 | how many dispatches may fail |
| `jobDeadline` | 10 minutes from request creation | how long new attempts may keep being started, including while waiting for an owner to exist |
| `firstBodyTimeout` | 180s | one attempt's wait for first content, measured from the arrival of headers |

The retry budget applies only to inference requests. Routes not classified in
the engine's route table are still forwarded verbatim, and some perform
state-changing operations; for example, `POST /api/pull` starts a model
download. Since any `5xx` is retryable regardless of route, re-resolving and
redispatching such a request could start the operation several times, which is
both non-obvious and wasteful. For a non-inference request the proxy therefore
resolves candidates once and tries each at most once. It does not re-resolve
candidates, back off between rounds, or apply the inference deadline.

**An attempt is consumed by a dispatch, never by a resolution.** Finding no
eligible owner sends nothing, so a job whose only owner briefly drops out does
not burn its budget while the node comes back.

**Elapsed time per attempt is not capped.** The dial is bounded
(`proxyDialTimeout`, 10s), and so is the wait for first content once headers
arrive (`firstBodyTimeout`, 180s). The wait for headers is not: the transport
sets no response-header timeout, so an engine that accepts a request and
withholds its headers holds the attempt until it answers, the connection fails,
the target leaves discovery (for a manual node, only when the user removes it),
or the client disconnects.

Both bounds are checked before every dispatch and **never truncate an attempt
already in flight**. Killing one seconds from its first token and then failing
the job for being out of time would spend the entire wait and discard the
result. `jobDeadline` is therefore a "no new attempts after" line rather than a
ceiling on elapsed time, and the worst case is the deadline plus one whole
attempt. Since an attempt's wait for headers has no cap, neither does that sum.

vLLM model preparation has a separate ten-minute limit, including queueing
behind active responses. It runs before inference, whose response-header wait
remains uncapped as described above. The preparation budget is not an overall
inference deadline; streaming after first content has its own idle deadlines.

Which bound ends a job depends on how its attempts fail, and all three cases
are real:

- **Fast failures** — a refused connection, a prompt `503` — cost about a
  second, so all five attempts fit well inside the deadline and the dispatch
  budget is what ends the job. This is the common case.
- **Slow failures** — an engine that sends headers and then produces nothing —
  cost about 180s each, so the deadline arrives first. With backoff between
  them the fifth dispatch would start past 600s, so the job ends after four
  attempts with budget unspent.
- **No owner at all** costs no attempt, so the deadline is the only thing that
  can end the wait.

The last case is why the deadline cannot be dropped as redundant with the
attempt count, and the middle one is why five attempts is a ceiling rather than
a promise.

An engine that withholds its headers never fails for being slow, so neither
bound ends that job; only the attempt ending does.

#### Waiting for an owner

There is no memory of nodes seen in the past: every resolution runs against the
live discovery snapshot, so "eligible owner" always means "advertising this
model right now". What differs is the position in the request's life.

- **No owner at admission** — nothing has been dispatched, so there is nothing
  to wait for being interrupted. Return `502` immediately (§5.4). A model
  nobody advertises will not become servable by waiting.
- **No owner after at least one dispatch** — an owner existed a moment ago and
  went away, so the proxy waits for one to reappear, re-resolving on the backoff
  schedule. This costs no attempt, so `jobDeadline` is the only thing that ends
  it: the deadline *is* the grace period for a node coming back.

The asymmetry is deliberate. Waiting is justified by evidence that a capable
owner existed within this request's lifetime, and nothing weaker.

Candidates are re-resolved every round, so a node that recovered or a model that
finished pulling becomes eligible mid-retry. The same node may be retried — with
one owner, all five attempts go to it.

A requester that has gone away ends the retry immediately: nobody is left to
receive the answer, so the remaining attempts belong to work someone is waiting
for. Its end is reported as `cancelled` and is never attributed to a missing
node.

### 5.2 The commit point

**Commit is the first byte of response body, not the response headers.**

Headers are not a valid commit signal: they may show only that the engine
accepted the request. Treating them as commitment would prevent failover while
the request remains queued inside the engine. The first content byte is the first engine-independent evidence that
generation has begun.

The difference is engine-specific and large. Measured against LM Studio 0.4.x
with a model loaded at `--parallel 1`, probing while a long generation held the
single slot:

| Request | Headers | First body byte |
| --- | --- | --- |
| streaming | 12ms, on accept | 35.5s, when the slot freed |
| non-streaming | 38.9s, withheld until complete | with the headers |

Nothing arrives during that wait — no SSE keepalive, no empty role delta — so a
byte is real content rather than a heartbeat.

**Only the existence of the byte matters, never its value.** The proxy does not
look at it: inspecting content would make the gate engine-specific, and it
would mean reading response data §9 forbids. Whatever the byte is, it is
spliced back onto the front of the stream so the client receives the response
intact.

`EOF` with no bytes counts as a commit. This is defensive rather than expected
— an inference route answering `200` with an empty body is an engine
misbehaving — but treating it as a stall would spend the whole budget waiting
for content that is never coming and then fail a request the engine considered
answered.

A first-content timeout **never** commits, including on the final permitted
attempt, where it is terminal with `504`. Committing there would leave the peek
reader blocked on the same body, racing two readers on one stream, and against
a silent upstream the copy would block with nothing left to interrupt it.

The gate is scoped to inference, and costs a non-streaming response nothing,
since its headers and body arrive together. That is why the request's own
`stream` flag never has to be consulted and no per-dialect default has to be
guessed, and it is what makes the boundary engine-independent — correct whether
an engine withholds headers or sends them on accept.

**After commit there is no retry.** A stream truncated from there is the
caller's to finish with (§1.1).

#### Known gap: a silent upstream after commit

There is no upstream read or idle deadline. If a committed engine keeps the
connection open and stops producing, nothing in this process ends the request:
the client write deadline needs a write to trip, and a silent upstream produces
nothing to write. A *crashed* engine is handled — the copy fails and §5.4's
deferred reporter emits the terminal — but a *frozen* one holds the request
until the client gives up.

Closing it needs a read deadline refreshed on every chunk, and a value chosen
so a slow-but-healthy generation is not cut off mid-answer. That judgement is
not made here.

### 5.3 Per-attempt cancellation

A watcher cancels an attempt promptly when its target leaves discovery.
Otherwise the attempt could stay blocked until the first-content timeout while
the broker's node-loss sweep had already marked the workload failed, so the UI
would report failure while the client was still waiting.

Each attempt runs on a context derived from the request's, and that child
relationship is load-bearing: the disconnect watcher and the outcome
classification both read the *parent*, so cancelling a child cannot be mistaken
for the requester leaving. Confusing the two would either report a job that went
on to succeed as `cancelled`, or let a real disconnect keep burning attempts.

**Commit and cancellation are mutually exclusive, decided by one atomic
claim.** Both can become true at once — the watcher's discovery check can pass
while the commit path is already running, and the work between the arrival of
the first byte and the end of the commit includes a synchronous notification
write and two mutex acquisitions. A pair of signals cannot express which
happened first, so the attempt has a single state that each side claims by
compare-and-swap; the loser becomes a no-op.

**A committed first byte wins that claim.** A delivered byte is direct evidence
that this node is serving this request, whereas an absence from discovery can be
a transient announcement gap. If the watcher claims first the attempt is
abandoned without committing, so the request retries rather than being served a
stream the proxy then truncates itself.

### 5.4 Status handling and outcome classification

Whether a status is retried depends on *when* it arrives, not only on its class.
Before commit the proxy may try elsewhere; at commit the status becomes the
client's answer.

| Upstream status | Pre-commit, budget remaining | Workload state if it is the answer |
| --- | --- | --- |
| `2xx` | commits (§5.2) | `completed` |
| `3xx` | not retried; not expected from an engine, and forwarded verbatim if one sends it | `failed` |
| `4xx` except `404`/`408`/`429` | not retried — the same request fails identically everywhere, so retrying only spends budget | `failed` |
| `404` | retried on an inference route only: positive model inventory can be stale | `failed` |
| `408`, `429` | retried | `failed` |
| `5xx` | retried | `failed` |

Anything not retried commits and becomes the client's answer immediately, with
no further dispatch, whatever budget is left.

These are the statuses the proxy itself returns:

| Condition | Status |
| --- | --- |
| Request body exceeds 32 MiB, nothing dispatched | `413` |
| No owner advertises the model, nothing dispatched | `502` |
| Final permitted attempt returned a status | that status and body, unchanged |
| Final permitted attempt failed at the transport | `502` |
| Final permitted attempt answered but produced no content | `504` |
| The loop ended between attempts, nothing in flight | `503` with `Retry-After` |

The final permitted attempt is never retried, so it answers for itself. That is
why exhausting the dispatch budget does not produce a `503`: the fifth attempt
is the answer. The `503` is for a loop that ended with no attempt to answer,
which means time ran out.

Its body names which, because the two call for different responses from the
caller:

- **the deadline passed while attempts were still failing** — an owner was
  present throughout, and slow attempts reached 10 minutes before they reached
  five dispatches (§5.1);
- **no owner became available** — the candidate set was empty and the wait for
  a node to come back timed out.

A third reason, **every dispatch attempt failed**, guards the case where the
loop ends on the count with nothing having answered. The paths above make that
unreachable today; it is cheap to keep and wrong to rely on.

The resulting workload state:

| Outcome | State |
| --- | --- |
| committed 2xx, streamed to the end | `completed` |
| committed 2xx, truncated by the node dying | `completed` |
| committed non-2xx | `failed` |
| client disconnected, or dead-client write deadline tripped | `cancelled` |
| cancelled by our own shutdown | `cancelled` |
| budget or deadline exhausted pre-commit | `failed` |

`cancelled` distinguishes requester-initiated termination from an actionable
engine, node, or routing failure. Client disconnects and shutdown cancellation
therefore do not appear in the failed bucket.

**Reporting is deferred.** A mid-copy error is never returned to the handler:
`httputil.ReverseProxy` converts it into `panic(http.ErrAbortHandler)` when
running under a real server. That panic bypasses ordinary code following the
proxy call, so terminal workload and request reporting must run in a `defer`.
The deferred reporter records the abort as operational detail, emits the
terminal event, and re-panics so `http.Server` still closes the connection.
Reservation release is also deferred so it survives the same unwind.

That panic is not the failure mode §3.2 describes, despite the shared word.
`http.ErrAbortHandler` is a sentinel `net/http` defines to mean "abandon this
response without logging a stack", and `http.Server` recovers it silently by
contract; re-panicking is how the connection gets closed, and swallowing it
would leave the client waiting on a response that will never be finished.
§3.2's entry covers an *unexpected* panic — a bug — which is logged with its
stack and answered as an internal error.

### 5.5 Workload lifecycle

| Event | When | State | `scheduledOn` |
| --- | --- | --- | --- |
| `workload:submitted` | admitted | `queued` | empty |
| `workload:submitted` | each dispatch | `queued` | the target |
| `workload:submitted` | between attempts | `queued` | empty |
| `workload:started` | commit | `running` | the node that served |
| `workload:completed` / `workload:errored` | terminal | per §5.4 | unchanged |

`submitted` covers the whole pre-commit life; `started` means the engine is
producing content. That split is forced as well as honest: the broker's store
merges by state rank and rejects a lower one, so a job that claimed `running` at
dispatch time could not return to `queued` for its next attempt and the retry
would be invisible. Keeping the loop inside one state, with `scheduledOn`
carrying the detail, is the only shape that store can represent.

Every event carries `seq`, the producer's event counter from 1. The
workload-manager's inter-node dedup is a permanent set, so a retry returning to
a placement it already used — `queued` on A, cleared, `queued` on A again, which
this loop produces routinely — would otherwise be indistinguishable from a
redelivery and dropped by every peer.

A job admitted but not yet dispatched has no execution node. Consumers must
treat an absent `scheduledOn` as "not placed" rather than assuming a node.

### 5.6 Coverage gap: non-streaming requests

A non-streaming request is the case the retry policy serves worst: once
dispatched, it is never retried for being slow.

Its headers arrive only when the response is complete, so from outside the
engine "queued behind other work", "generating right now" and "stalled" are
indistinguishable. The transport sets no response-header timeout, and the
first-content wait adds nothing here, since the body arrives with the headers.
A long generation therefore runs to completion rather than being restarted
elsewhere, but a node that stalls before responding holds the attempt until it
answers, the connection fails, the target leaves discovery (for a manual node,
only when the user removes it), or the client disconnects, and it is never
failed over for being slow. An engine that withholds headers on a streaming
request too, as Ollama does until generation begins, shares this gap.

Nothing in the current engine surface fixes this: there is no progress signal
before the response is complete. A caller that cares should prefer a streaming
request on an engine that sends headers before generating, such as LM Studio,
where the first content byte gives the proxy the evidence it lacks here. On
Ollama streaming does not help, because its headers are withheld as well.

### 5.7 Accepted behavior

**An abandoned attempt may still run inside the engine.** None of the engines
PAIR supports today exposes a way to withdraw a request it has accepted, and all
of them queue internally, so a retried job can have more than one copy generating while
the abandoned copy keeps its node busy.

Whether closing the connection cancels anything upstream is **unverified** —
plausible, engine-specific, and never measured. The behavior above is stated as
the pessimistic case for that reason.

This is accepted rather than overlooked: those internal queues are invisible and
un-retargetable, which is the problem the retry policy works around rather than
a mechanism it can build on. Do not reach for engine environment bounds or
occupancy probes to "fix" it — LM Studio exposes no such signal at all, and an
adopted engine keeps its own environment regardless. An engine that did offer a
real cancellation API would change this calculus, and the claim should be
revisited per engine rather than assumed to hold forever.

### 5.8 Browser policy

Each facade preserves its engine's CORS policy, including missing headers and
permission denials. Ingress gates apply before preflight forwarding. Proxy errors
grant no CORS permissions. Multi-target preflights intersect permissions from all
responding engines without forwarding caller credentials; unavailable engines
are skipped, and no responders yields 502. Preflights take no reservations.

Model inventories with an Origin require agreement from every responding engine:
a denial yields 403, an invalid inventory yields 502 without a partial list. An
invalid-inventory error retains CORS headers only when all responding engines
approve. See README.md for the complete forwarding and intersection rules.

### 5.9 Candidate resolution

Resolution turns the eligible owners into dial targets. Relay-fed owners first
pass the identity gate in §5.11. Resolution then refreshes membership and pins
from the cluster directory, so each relay-fed remote dial requires a current pin in
addition to the admitted discovery record. Each owner is classified by where
its address points:

| Owner | Dialed as | Why |
| --- | --- | --- |
| This facade — its own listen port, or an alias address it bound | the local engine from `node/set-local-backend`, over loopback; dropped when none is set or it is unhealthy | this node advertises the facade under the engine's own key (e.g. `ol`, `lm`, `lc`), so dialing it would enter this node's own cluster ingress and recurse |
| An admitted peer this node holds a current pin for, read from the live mesh | `https` to that peer's facade, through the pooled client pinned to its UUID | discovery supplies the verified identity and `trusted: true`; the live mesh authorizes the outbound connection against the current pin |
| A manual node from `node/add-manual` | plain HTTP to the address the user supplied | a deliberate, separately labelled bypass |
| Anything else — an unpinned peer, or any peer while this node is unclustered | dropped | a peer's facade refuses plaintext from the LAN, so it is not reachable |
| The same owner reached from the cluster router (§5.12) | the same dial, through the same pooled client, at that peer's port **for that engine** | the hop is terminal by construction: the peer's cluster ingress forwards to its own local engine and never consults resolution, so a cluster-routed request is dialled exactly once |

A router hop carries the facade whose address chooser selected it, so a transport
error forgets the confirmed address on the same chooser that confirmed it rather
than on a second one that never saw it.

A defensive check then drops any target that still resolves to this facade, and
targets are de-duplicated by resolved host, so one engine known under two ids —
a manual entry and its discovered record — is tried once. Order is §5 step 2's:
an eligible `node/select` pin, then the scheduler's list, then every remaining
owner by stable id, so an owner the scheduler has not ranked stays routable as a
last resort.

### 5.10 Model-list aggregation

The model-list routes are the one exception among forwarded requests to
sending a request to a single node; CORS preflights also fan out (§5.8). They
resolve with no model, so no owner filter applies, and fan out concurrently to
every resolved candidate, dialed as §5.9 describes, with
redirects not followed. Each candidate is queried at the request's own path,
except on the llama.cpp facade, where `GET /models` and `GET /v1/models` both
query the candidate's `/models`.

A candidate's answer counts only if it is `200`, at most 16 MiB
(`maxModelListBytes`), carries its dialect's array (`models` native, `data`
OpenAI), and gives every record an identity. A `5xx` or transport failure marks
it unavailable, and a transport failure also forgets its confirmed address.

Results merge in **candidate order, not completion order**, so which duplicate
is kept is deterministic and a slow peer cannot hide a healthy one. The identity
is `id` in the OpenAI dialect, and `model` falling back to `name` in Ollama's
native list, normalized by the engine's naming rule so `llama3` and
`llama3:latest` are one model. The first record for an identity wins; a later
one whose `digest` differs is logged as a conflict and dropped.

Without an `Origin` header, no usable list is a `503` `{"error":"model
inventory unavailable"}`; a candidate answering with an empty list is a
success. With one, the browser-origin gate in §5.8 runs first and answers `502`
with the same body when no candidate answered — every candidate unreachable or
`5xx`, or none resolved — or when any candidate answered unusably, such as a
`404` or a `200` without a valid list, even if another returned a good one
(§5.8). So the `503` arises only for a request without `Origin`. The
aggregate is reported as one request with `target: "cluster"` and no node id.

The cluster routing endpoint's `GET /v1/models` does not fan out at all: its list
is **generated** from the catalog that routes the same request, so a name cannot
appear in the listing without resolving, and cannot stop resolving while it is
still listed (§5.12). Its dedupe key is the **name alone**, not the `(name,
digest)` key above — two engines holding the same weights advertise different
digests (GGUF file hashes against Ollama manifest digests), so digest dedupe
would delete a real second provider and make the failover set depend on which
host answered a list first. Every name with at least one dialable provider is
listed, and its `owned_by` is the routing class (`cluster`) rather than an
engine, because at list time the answering engine is not decided and a guess
would read as a routing promise the moment load preferred somebody else.

### 5.11 Discovery input

Each facade subscribes to the broker's discovery relay for its engine's own
service key (e.g. `ol`, `lm`, `lc`) and receives `discovery:nodes`, the full
filtered set, on every change. Each snapshot **replaces** the facade's relay-fed
overlay, so a departed node is simply absent from the next one and there is no
state to drift.

A directory node enters the routing overlay only when `identityVerification`
is `local`, or it is `pinned` with `trusted: true`, and it advertises this
engine's service key with a nonempty canonical `ip`. Owned local records need
no peer pin. Unverified records remain outside the overlay regardless of their
UUID, address, model, or trust claims. For an admitted record:

- its id is its `hostUuid`, so routing, `scheduledOn`, and the scheduler's list
  agree on one identity that survives a rename;
- its port is the one it advertises under the key, which is that peer's facade;
- its addresses are its whole ranked list, so routing can fail over past a
  direct-connect address this host cannot reach;
- its models are its inventory for this engine alone, so a model a multi-engine
  peer serves only through another engine does not make it an owner here. A
  peer sending no per-engine attribution falls back to its flat list.

The diff against the previous snapshot is emitted as `node/discovered`,
`node/updated` — a model change counts, because inventory is routing data — and
`node/removed`. A node dropping out also reports
`<engine>-proxy:upstream-unreachable:<nodeId>` into the errors pipeline, cleared
when it returns. Manual nodes are a separate overlay the snapshots never touch,
and a manual node is shadowed by a relay node with the same id. A `node/select`
pin on a node that has left both overlays is cleared with
`node/selection-changed`.

### 5.12 The cluster routing endpoint

A dedicated facade answers one engine's dialect and can only route to owners of
that engine. The cluster routing endpoint answers the OpenAI-compatible surface
and routes to whoever provides the named model — any (node, engine) pair — after
rewriting the body's `model` to the identifier that provider expects. It is
brought up by `--cluster-router` on `--cluster-router-port` at process start,
and a live process can also switch it through `router/set-enabled` — up on a
named port, an omitted port taking the default, or down by closing the
listener — so the endpoint's presence is runtime state, not only a launch flag
(default 9999, which is outside every engine profile's range for the same
reason the facades' standalone ports are: a router sitting on an engine's port
would be mistaken for that engine, by clients and by engine-manager's identity
probe alike). The broker threads both flags only when the user's Cluster
Settings switch asks for the endpoint.

**The listener binds loopback only, and that is the posture, not a placeholder.**
This endpoint routes to any node in the cluster, so handing it to a network in
plaintext would give whoever shares that network a routing oracle for the whole
cluster. A peer that needs to route across nodes runs its own router on its own
loopback and hops over the dedicated engine endpoints, which are already
pin-gated. The listener address is therefore the whole gate and no per-request
`RemoteAddr` check is added on top of it.

**A port nobody can listen on is a reason to have no cluster endpoint, not a
reason to have no proxy.** The router shares this process with every facade, so
exiting over a bind failure would turn off inference on every engine of the
machine for a number the user typed. At process start the proxy logs once and
stays up with `clusterRouter` nil; `router/status` then answers "there is none"
as a normal result rather than an error, and `cluster/set-aliases` stores.
The alias document is process configuration: the courier delivers it once per
consumer bring-up and again on change, so refusing a router-less push would
drop it and leave an endpoint started later routing only exact native names
until somebody re-saved the list — broken-looking, not unconfigured. No
listener means no routing, so storing costs nothing, and a bring-up installs
the stored document before serving. At runtime the same fact comes
back as a refusal — error code `-32011` (`codeRouterBindFailed`), naming the
port — and the refusal changes
nothing: the endpoint that was serving still serves, because moving a working
endpoint off its port over a lost bind race would turn one wrong number into
downtime on the path that was fine.

**A runtime turn-off closes the listener and answers at once.** No drain: a
generation mid-flight keeps streaming on its already-accepted connection — the
no-abort invariant — while new connections to the port get nothing.
`router/status` follows the socket: a closed listener still reports its
address, so status reads the endpoint's stopped mark rather than the listener,
and a re-enable of the port that already answers keeps that listener (the
broker can resend after a reconnect). The desktop reads bound state from
`router/status` — never from the port it asked for — and lists the endpoint
only when a live listener answers.

**What it serves:** `GET /v1/models`, and `POST` on any path the engine route
tables declare inference on. Dialect comes before distance: a path every engine
declares (the shared OpenAI/Anthropic surface) makes every engine hosting the
requested model a candidate, while a path only Ollama declares (`/api/chat` and
friends) makes Ollama providers the *only* candidates — sending it elsewhere
would be a request to an endpoint that does not exist there. Anything else is
`404 not-routable`: the facades forward unlisted paths verbatim because they
front exactly one engine and the router has no such default. A request carrying
the engine identity probe header gets `409 router-not-engine`, because answering
a probe with a forwarded response would let engine-manager conclude an engine is
present where nothing but a router is listening.

**Resolution is D1's order, and it is one expansion.** The requested name is
matched as a native identifier across every engine first — that pass
short-circuits, so a model literally named after an alias keeps routing to
itself — then under each engine's naming convention, then as a configured alias.
An alias is a synonym for a model, never a host list, so the alias pass expands a
name into whoever this catalog provides under the name it stands for. The target
is resolved once, at catalog build: a synonym whose target is itself a configured
name (including its own) yields nothing, because a two-hop synonym cannot be
predicted from any one machine's catalog. Several machines may define one name,
each standing for its own engine's spelling of the same weights, and **every
definition that resolves contributes its hosts to that name's provider set** —
which is what lets one client-side name reach any machine currently able to serve
it. This node's own definitions lead and peer definitions follow in node-id
order, so a merge never depends on which directory record sorted first.

**The catalog is built whole and replaced whole**, on a 250 ms TTL, from three
inputs: the per-engine routing overlays the facades in this process already
maintain (so identity gating, ranked addresses and manual entries are shared, not
re-derived); this node's configured alias list — the copy held on the process,
installed into a fresh endpoint at bring-up and replaced whole by every
`cluster/set-aliases`, whether or not an endpoint is up; and the relay-fed
peer alias feed — a
`cluster:subscribe-aliases` registration followed by `cluster:peer-aliases`
snapshots, each replacing the feed. The peer feed deliberately does not travel
through the per-engine overlays: an alias names a model, not the machines fronting
it, so a node advertising no engine service announces aliases just as validly as
one that does. This node's own record is filtered out of the feed, since its
aliases are configuration and arrive immediately rather than at discovery pace. A
routing decision therefore sees either the old cluster or the new one, never half
of each.

**Dialability is decided per attempt, from live trust.** The provider set names
who could serve the name; each provider is then admitted by §5.9's rules — local
backend for this node's own endpoint, cluster mTLS only while the live mesh still
pins that peer, plain HTTP for an explicitly manual entry, everything else
dropped — and the membership refresh runs again for each walk, so a peer unpaired
since the catalog was built stops being routable immediately. Address selection
uses the facade's own reach chooser for that engine, so a confirmed address is the
same fact the facades hold and a transport error forgets it on the chooser that
recorded it. Two refusals come back as local answers with nothing dialled: `502
no-provider` when no node serves the name, and `502 no-provider` when providers
exist but none can be dialled, which logs the providers considered because "the
cluster says a host serves this and this process cannot dial it" is a different
fix. A body that names no model is `400 no-model`.

**One pass over the provider set is the whole walk.** The reservation is taken
after the set is known and before anything is dialled, from the same
process-wide ledger the facades use — keyed by node id at the same
host-level granularity — so a burst through the router sees, and is seen by, the
load the facades already put on that host. No user pin exempts a provider here:
`node/select` is a per-engine selection and a cluster route has no engine to
inherit one from. The claim moves to whichever provider is actually being tried
and is released on every exit path, including the mid-copy upstream panic.
Retryable statuses mirror the facade path (`404` included, because every name
here arrived through an inventory that can go stale; genuine client errors are
not replayed), commitment is the first byte of body (§5.2), and the final
provider's answer is the client's answer. What differs is that a cluster provider
set can be every host in the cluster, so the dispatch budget can end the walk
with providers still untried; that is a local `503 exhausted`, never the fifth
provider's `503` read back as the cluster's verdict, and never a widened set
(D9).

**Only `model` is rewritten.** When the chosen provider spells the model
differently, the body's `model` is replaced with that native identifier through a
JSON object round trip; a body that is not an object carrying a string `model`
travels as it arrived, and the corrected `Content-Length` is sent with the body
that was actually written. Nothing else about the request is touched, so a
friendly name never reaches the wire: the provider sees only its own identifier,
and a client's `Authorization` travels through as on the facade path.

**Every hop is terminal.** A peer's advertised engine endpoint is that peer's
proxy, and a request arriving there over the cluster ingress is forwarded to that
peer's local engine without consulting candidate resolution (§7). Nothing
re-routes a second time, so a cluster-routed request is dialled exactly once and
a request loop between two routers is unreachable by construction; the same rule
is applied on this node by the self-forward guard that stops a hop resolving to
this process's own listener.

**Jobs are the router's own.** A workload carries `engine: "cluster"` — a routing
class, not a fifth engine, and a true statement about the moment of submission:
which engine would run it was not yet known — and its ids count on a
router-private counter, since the workload key carries the engine and a router job
and an Ollama-facade job may both be id `1`. One id spans the whole walk, so a
request that fails over twice is one job card, and re-placement between attempts
re-publishes `scheduledOn` so the scheduler counts the host actually being held.
Rejections before admission create no workload record, and terminal classification
follows §5.5 — including a committed stream whose copy was cut short, which stays
`completed` because the truncation is the caller's to resume.

**Deliberately absent, each with its reason:**

- *The dropped-target watcher.* The facades poll a dispatched node's discovery
  record during an attempt and abandon early when it disappears. This path finds
  out when the dispatch fails or the first-byte budget expires, so the bound is
  failover latency, not correctness: the hop still counts once and nothing
  re-routes.
- *Re-resolution between attempts.* This is one pass over what the resolver
  produced; a facade's multi-round wait is not a state this walk reaches.
- *Per-engine user pins,* for the reason given with the ledger above.
- *The paired-addressed telemetry pair* (`request-started` / `request`). It
  travels through a facade because the broker attributes those events to an
  engine; a cluster-scoped stream needs a broker-side decision about what engine
  "cluster" means there. Workload events stay on the process-scoped channel,
  which already accepts any engine tag.
- *Control-plane fan-out* (`/api/tags`, `/api/show`, `/api/pull` and friends).
  The router is inference plus a generated inventory and never fetches a list
  from upstream, so the credential-stripping question the model-list fan-out
  carries does not arise here.
- *A LAN personality,* per the listener posture above. A clustered mTLS
  personality for the router is deferred, not declined.
- *Finer capacity granularity.* The open question is the ledger key's
  granularity, not the claim: any (host, engine) split waits for a mixed-host
  burst run that shows starvation.

## 6. Reservations

A reservation is one in-flight dispatch this process has made since the last
scheduler snapshot. Estimated load for a node is
`pending + gpuPressure + reservations`, the first two from the scheduler.

- **Taken** when a candidate is chosen, unless an eligible manual pin applies,
  and re-taken through the same path on each retry round (§5.1).
- **Moved** to the node an attempt actually targets, because the node that
  refused the request is not doing the work. An attempt can hold a node for the
  whole first-content budget, so the claim follows each dispatch rather than
  waiting for the commit.
- **Released** when the request ends, so a node stops counting as loaded as soon
  as it stops working. Without release a trickle of short requests makes an idle
  node read as busy for the rest of the snapshot interval.
- **Released before every retry backoff**, for the same reason applied to a
  waiting job: between attempts nothing is running, and because the map is
  process-wide a stale claim would push the *other* engine's traffic away from a
  node that is in fact free. This mirrors clearing `scheduledOn`, one being the
  proxy's own estimate and the other the scheduler's.

Each is stamped with the snapshot **generation** it was counted against. A
snapshot supersedes every reservation taken before it — its pending counts
already include that work — so a release naming an older generation is dropped.
Applying it would double-count the completion and drive the node's estimated
load below zero, which reads as permanently idle and attracts every subsequent
dispatch.

`node/set-priority` therefore carries a generation and is applied at most once;
a redelivered snapshot must not clear live reservations. A generation of zero is
rejected with `-32602`.

A snapshot's contents are applied as follows. `nodes` is stored verbatim: an id
discovery does not know is kept, since the node may appear later, and counts for
nothing until it does. `ranks` supplies each node's `pending` and `gpuPressure`;
an entry with no id is skipped, a negative value is floored at zero, and
pressure is capped at `schedulerwire.MaxGPUPressure`. A nodes-only payload is
valid and gives every node a zero baseline. An empty list clears the
scheduler's influence and leaves stable-id ordering.

## 7. Listener personalities

One port per facade, demultiplexed on the connection's first byte:

- **Loopback plaintext** for local clients. A LAN caller is refused.
- **Cluster mTLS** when `--cluster-dir` shows this node is a member: a peer
  whose client certificate matches a local pin is forwarded straight to the
  local engine reported by `node/set-local-backend`, never re-routed onward.

Membership and pins are re-derived per request and on a watch, so joining or
leaving a cluster needs no restart.

Each personality answers its own refusals with a small JSON body
(`{"error", "code"}`) that never echoes the request or any generated output:

| Personality | Condition | Status and code |
| --- | --- | --- |
| Plaintext | caller is not loopback; an unparseable address fails closed | `403 loopback-only` |
| Plaintext | request carries `X-NVPAIR-Engine-Identity-Probe: 1` | `409 proxy-facade` |
| Cluster mTLS | client certificate is not a pinned member | `403 cluster-auth` |
| Cluster mTLS | no healthy local engine from `node/set-local-backend` | `503 no-local-backend` |
| Cluster mTLS | the local engine fails the forward | `502 backend-error` |

The `409` is a contract with `nvpair-engine-manager`, which marks its identity
and action probes with that header so a facade on the engine's usual port can
never be mistaken for the engine itself. The cluster ingress applies no route
filtering — a pinned peer is treated like a local client toward the engine —
and never consults candidate resolution (§5.9), so it is terminal: a peer's
request cannot be routed onward or amplified.

An engine with an inherited host variable (today Ollama alone) may also be given
loopback-only **alias** addresses, so clients already using that variable enter
the same routing path. An occupied alias is non-fatal: the existing owner is
untouched, the primary listener stays up, and a warning is reported.

The cluster routing endpoint is a **third personality shape**: its own port, with
no demultiplexing, because it has exactly one — loopback plaintext, always. There
is no cluster mTLS personality on it, so a peer never arrives here; a peer's
routing starts on its own loopback and enters this process through one of the
engine ports above (§5.12). It answers its own refusals in the same small JSON
body:

| Condition | Status and code |
| --- | --- |
| Request carries `X-NVPAIR-Engine-Identity-Probe: 1` | `409 router-not-engine` |
| Path is not an inference route or `GET /v1/models` | `404 not-routable` |
| Body names no model | `400 no-model` |
| No node provides the requested model, or none of its providers is dialable | `502 no-provider` |
| The dispatch budget ended with providers untried | `503 exhausted` |

Unlike the facade personalities, it never forwards to a local engine: everything
it accepts is resolved against the catalog and dialled as a candidate, which is
why the identity probe is refused rather than answered.

## 8. Ports

| | Ollama | LM Studio | llama.cpp | vLLM |
| --- | --- | --- | --- | --- |
| Engine's own client-facing port | 11434 | 1234 | 8080 | 8000 |
| Where PAIR relocates the engine | 11435 | 1235 | 8081 | 8001 |
| Standalone port, when `port` is omitted | 11435 | 1234 | 8080 | 8000 |
| Persisted-port file | `proxy-port.json` | `lmstudio-proxy-port.json` | `llamacpp-proxy-port.json` | `vllm-proxy-port.json` |

The cluster routing endpoint is not in that table because it belongs to no
engine: `--cluster-router-port` (default 9999) is its own number, chosen by the
user in Cluster Settings and deliberately outside every engine profile's range.
It is **not** persisted per-engine and not announced to peers — the endpoint is
loopback-only, so there is nothing for a peer to dial here, and each node runs
its own (§5.12).

A port chosen at runtime via `set-port` is persisted per engine and restored
when that facade is enabled, taking precedence over the requested port, so the
facade returns where the user left it. `ignorePersistedPort` bypasses that for a
broker-coordinated start.

`port` 0 means "the engine's standalone default". Any other out-of-range value
is rejected rather than honoured as ephemeral: the facade announces the
requested port in `ready`, so binding an ephemeral one would leave the broker
unable to locate it.

## 9. Logging constraints

Never log prompts, chat messages, request bodies, stream chunks, full responses,
credentials, or pairing data. Operational metadata only — engine, model, job id,
node id, path, stream flag, normalized error text — and keep per-request routing
detail below info level.

The log component is the **process** (`nvpair-proxy`), because at process init
there is no engine to name. Facade-scoped records carry an `engine` field, which
is what keeps a line attributable when several facades share the sink.

## 10. Adding an engine

Everything engine-specific is one entry in `engines.go` plus the shared identity
in `nvpair-shared/engines`. A new engine needs: its identity and discovery
service key, its ports and persisted-port filename, its route table and model
normalization, and its empty model-list envelope. It needs no new process, no
new supervisor, and no new relay wiring.

## 11. Failure modes

| Mode | Behavior |
| --- | --- |
| Requested port taken at enable | Tagged bind failure; broker retries on a fallback port |
| Request body exceeds 32 MiB | Local `413`, rejected before candidate selection, no engine contacted |
| No advertised owner for the model | Local `502`, no engine contacted |
| All owners refuse retryably | Last upstream status surfaced |
| Transport error with candidates left | Forget the node's confirmed address, fail over |
| Client disconnects mid-stream | Terminal workload event emitted at once; upstream cancelled |
| Broker link closed | Facades stop serving, then the shared transport pool closes |

## Request-triggered model preparation

The vLLM profile supports preparing an advertised cached model before inference.
The broker's `node/set-local-backend` payload includes optional
`loadOnRequest:true` while this engine is enabled, including awaiting-model and
starting states. This permits a loopback routing target while the serving process
is not yet healthy. It never permits a non-loopback local target.

Only inference requests with an `application/json` content type and no `Origin`
header may trigger preparation. Other requests retain the native engine's
behavior and CORS decision without starting or switching a model.
Their model-bearing inference responses still hold admission until completion,
so an automatic switch cannot interrupt a response accepted by the native engine.

The proxy sends operational `{id, engine, model}` metadata in `model:prepare` to
its broker, receives `model:prepared {id, engine, port?, error?, status?}`, and
sends `model:cancel` if that preparation is abandoned. Engine-manager rechecks
cached inventory and enabled intent under the launch lock. Request content is
never sent over this control channel. Non-retryable preparation failures retain
an actionable error response rather than repeatedly restarting a failing model.
Model preparation returns HTTP 503 if vLLM is OFF or starting from OFF after
an explicit enable or model **Load**. It also returns 503 if **Stop** or
**Eject** interrupts a request-triggered load and the manager restores any launch
settings it changed. Within its retry budget, the proxy can then try another node
that advertises the requested model.
Requests for the last successfully prepared model skip this relay until a
lifecycle change, engine-manager restart, or upstream failure invalidates it.
Concurrent same-model requests share one preparation and its result. Each request
can stop waiting independently; the shared load is cancelled only when its last
waiting request leaves. Preparation retains its own 10-minute budget rather than
inheriting the initiating request's cancellation or deadline. An already-serving
model can also be confirmed by engine-manager without taking its launch operation
lock; an overlapping lifecycle change falls back to the locked admission path.

A per-facade gate allows concurrent same-model local inference and FIFO groups
for different models. Admission spans the complete response, including streams;
only after existing responses finish can another model be prepared. Both local
requests and terminal pinned-mTLS ingress use this gate. The terminal ingress
still never routes onward. Preparation has a bounded 10-minute allowance before
ordinary inference begins. Local and peer requests share the standard transport
pools, with no response-header timeout. Pin revocation, cancellation and
connection cleanup remain enforced.

The broker acknowledges `node/set-local-models {engine, id, enginePort, enabled,
models}` before exposing initial model snapshots or model-change events to the
UI. The proxy projects this authoritative inventory onto this node's stable ID,
superseding only its stale self-discovery entry. This makes an immediate request
after download routable and removes deleted local models immediately. The port
comes from engine-manager lifecycle state, including custom server ports. Cached
model listings through PAIR do not load a model. Browser-origin requests retain
the native engine's CORS decision.

A manager restart settles pending proxy preparations with a retryable failure;
a proxy restart cancels preparations orphaned by the old process.

For vLLM, the aggregated `/v1/models` response is limited to each peer's
advertised model-owner inventory. A peer's fresher HTTP list cannot expose a
new model before this facade can route an inference request to it. The model
appears after discovery convergence; removed models disappear when the peer no
longer reports them. Other engines retain their existing aggregation behavior.
