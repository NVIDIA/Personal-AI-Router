<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Microservice: Node Scanner `nvpair-node-scanner`

## 1. Purpose

The Node Scanner (Scanner for short) discovers computers on the local network and maintains a directory of their
identities, services, hardware, and models. It announces this computer, the host,
as one node, even when several services run on it. The broker distributes node
records and verified telemetry (hardware measurements) to services such as
proxies and scheduler. Desktop and the text interface (TUI) receive hardware
through broker snapshots and pushes, including display-only public previews
collected by workers for unverified records.

## 2. Scope

**In scope**

- Using multicast DNS (mDNS), a protocol for local-network discovery, to announce
  a maintained list of local services (`_nvpair-node._tcp`).
- Discovery, address selection, a node directory that clients can query, and
  notifications when records change.
- Public hardware previews collected over HTTP for unverified display records,
  and authenticated details for services using approved peer nodes in the cluster.
- GPU, CPU, memory, and model enrichment: gathering extra details to add to a
  discovered node's directory record, including models loaded by each engine.
  These details are sent to broker clients, not broadcast in mDNS announcements.
- Regular telemetry reads, sample-age tracking, retries, and anti-flap behavior:
  delaying removal to avoid treating short discovery gaps as disconnects.
- Identity and trust updates while running, using saved peer certificates
  (certificate pins) to check that each peer presents the expected certificate.

**Out of scope**

- Pairing, certificate generation, membership decisions, and saving pins,
  owned by `nvpair-cluster-manager`.
- Hardware collection, owned by `nvpair-node-info`, and engine startup/model
  operations, owned by `nvpair-engine-manager`.
- Sending inference requests and choosing where to run them, owned by proxies
  and scheduler.
- Frontend rendering and interaction, owned by desktop and TUI.

## 3. Key Use Cases

- **Publish this host**: the broker registers local service ports, and Scanner
  announces the host's identity, addresses in preferred order, and service ports
  as one node.
- **Discover an unpaired node**: Scanner records its mDNS announcement as an
  unverified discovery and separately collects public hardware for that row.
  Broker delivers the preview to that unverified display record. Verified
  directory records and scheduling use owned or authenticated measurements.
- **Discover a cluster peer**: Scanner verifies its node-info server against the
  saved certificate, then updates its directory record and fetches its model list
  using mTLS (mutual TLS), an encrypted connection where both nodes present
  certificates. Each checks the other's certificate against its saved pin.
- **Show live hardware usage**: desktop and TUI use broker hardware snapshots
  and live updates. Owned IPC supplies local measurements, pinned mTLS supplies
  approved peer measurements, and public previews remain unverified display
  data. Only accepted local or pinned GPU usage reaches scheduler.
- **Recover after a network change**: Scanner tries other known addresses and
  refreshes inventory without depending on receiving every network announcement.
- **Follow cluster changes**: Scanner reloads identity and pins when nodes join
  or leave, updating announcements and which peers can be used without a restart.

## 4. Open Questions / Risks

- **Public inventory for frontend display**: public HTTP supports worker-collected
  previews of unverified records in every membership state. Desktop and TUI
  receive those previews through the broker. HTTP does not
  verify who supplied the measurements, so those values are display data only.
  Scheduling, trust, routing, and other service decisions must use authenticated
  remote data or measurements received from a locally owned worker.
- **Unreachable does not mean removed**: a failed authenticated read may mean
  an overloaded or unreachable peer. It does not prove that the peer left the
  cluster, and an unauthenticated response cannot prove that either.
- **Service coordination**: node-info connections, shared records, broker
  caches, and service consumers must be updated together. A paired node that
  does not support authenticated inventory reads cannot supply verified details
  until upgraded. Unauthenticated data cannot be used in their place.

## 5. Requirements

**Functional**

- Scanner owns the host's network announcement. Other workers register their
  service ports with Scanner through the broker.
- The public HTTP node-info endpoint serves unpaired hardware
  previews, including while clustered. Scanner collects these on a separate
  bounded two-second loop with at most four public HTTP requests in flight
  across all nodes and their addresses. Each address sweep has a 1.5-second
  total deadline. Reads have a 64 KiB response limit, refuse redirects, support
  address failover, and back off after failures for at most 30 seconds.
  Previews cannot enter scheduling caches, verified peer records, routing inputs,
  trust decisions, or authenticated liveness evidence.
  In-flight results are discarded if the discovery record or endpoint changes.
- Scanner uses mDNS identity, name, addresses, and ports to decide where to try
  connecting. Finding a pin for an advertised identity selects the certificate
  to check against, but does not prove who sent the announcement.
- Scanner marks a remote peer verified only after pinned mTLS and checks on the
  identity in its response. It uses the name, addresses, ports, and hardware from
  that authenticated response to update its record.
- The local host, verified peers, unverified discoveries, and manually configured
  servers have separate records. A claimed node identifier, a UUID, cannot cause
  unverified data to be merged into a local or verified peer record.
- Scanner refreshes models every 15 seconds and reads healthy node-info
  servers every two seconds with stable per-node jitter, a small fixed timing
  offset that spreads requests across nodes. Failed remote reads use backoff,
  increasing the wait: 4, 8, 16, then 30 seconds between retries, returning to the
  normal interval on success. Each accepted peer response updates metadata,
  hardware inventory, and scheduling telemetry together. Local hardware arrives
  over owned worker IPC at the normal interval.
- Scanner retains the last successfully read inventory during temporary failures.
  Samples retain their validity and age, including time spent in requests, queues,
  and caches. Forwarding or resending old data must not make it look new.
- Scanner sends the highest GPU utilization on each node for scheduling, using
  pinned mTLS for remote samples and private interprocess communication (IPC)
  with the local worker for local samples. Public HTTP node-info never supplies
  scheduling data, including for manually configured nodes.
- A discovery observation expires after 12 consecutive missed five-second
  scans, clearing its candidate state and remembered address independently of
  verified peer availability. If more than one node is known and a scan returns
  none, Scanner ignores at most six consecutive empty scans before counting
  misses normally.
  If a verified peer remains after its last discovery expires, Scanner continues
  checking availability through the same canonical sampling loop. Once no
  discovery remains associated with the peer, a failed pinned mTLS read removes
  its record and caches only after a minute without authenticated inventory or
  inference activity. Membership and saved certificates remain. Announcement
  changes and removals cannot directly remove verified peers or close their
  connections. Open TCP ports and unverified network claims cannot retain a
  verified peer.
- Scanner applies trust changes while running. Removed or replaced pins clear
  verification status and scheduling samples, close affected idle connections, and
  prevent pending responses from restoring the old state.

**Non-functional**

- Scanner runs on Windows, Linux, and macOS on x64 and arm64, without administrator
  privileges.
- Node-info requests have deadlines and a 64 KiB complete-body response limit.
  Model requests have deadlines and a 16 MiB JSON decoding limit. Concurrent
  authentication, telemetry, preview, and model reads have separate limits.
  A slow discovered node must not block telemetry reads from verified peers.
- Scanner has few dependencies and reuses shared discovery and cluster-trust packages.
- Tests are repeatable with simulated discovery and local HTTP/TLS test servers,
  without requiring real engines, multicast networks, or cluster membership.

## 6. Inputs and Outputs

**Inputs**: broker JSON-RPC requests over stdio (standard input/output) or `--ipc`,
mDNS announcements, local identity and pins (read only), authenticated peer
node-info responses, public display-only previews, local hardware snapshots from owned worker IPC, model lists,
and notifications of inference responses. The broker supplies local service
ports and addresses that peers have successfully used, resending registrations
after Scanner restarts.

**Outputs**: the local mDNS record, `DirectoryNode` query results and change
notifications, node-info snapshots (measurements available at the time of a read),
smaller scheduling messages, and a startup `ready` notification. stdout carries
JSON-RPC messages written one at a time, and operational logs go to stderr.

Directory records carry `discoveryRecordId`, `identityVerification`, identity,
address, service, inventory, model, and `lastSeen` fields.
`identityVerification` is `local`, `pinned`, or `unverified`. These fields record
provenance: where the data came from and how the node's identity was verified.
Scanner owns and assigns these provenance fields.

`discoveryRecordId` identifies the record being tracked; `hostUuid` identifies
the host that record describes. For an unverified discovery, `hostUuid` is only
an advertised claim. Two announcements can claim the same `hostUuid` while
receiving different `discoveryRecordId` values because their service instances
or connection endpoints differ. Those distinct record IDs let Scanner track
the candidates separately while checking their claims. Neither identifier proves
ownership of a host; `identityVerification` records how identity was established.

## 7. API / Interface Contract

### 7.0 Methods and notifications

Newline-delimited JSON-RPC 2.0 over stdio by default. `--ipc <path>` selects a
Unix domain socket or Windows named pipe.

Requests (caller → Scanner):

| Method | Params | Result |
|---|---|---|
| `discovery:register` | `{service, port, txt?}` | `{ok:true}` |
| `discovery:unregister` | `{service}` | `{ok:true}` |
| `discovery:update-txt` | `{service, port, txt?}` | `{ok:true}` |
| `discovery:get-nodes` | `{service?}` | `{nodes:[DirectoryNode]}` |
| `discovery:reload-identity` | — | `{ok:true}` |
| `discovery:reload-trust` | — | `{ok:true}` |
| `discovery:set-observed-addresses` | `{addresses}` | `{ok:true}` |
| `discovery:node-activity` | `{hostUuid, msSince}` | `{ok:true}` |
| `log/set-level` | `{level}` | `{level}`, the accepted log level |
| `shutdown` | — | `null` |

Registration, identity/trust reloads, observed-address updates, and activity
reports may also arrive as notifications, messages that have no request ID and
receive no reply. `shutdown` requires a request. The broker normally sends
`discovery:node-activity` as a notification when an authenticated peer has
returned inference response data.

`discovery:update-txt` has the same handling as `discovery:register`. An empty
service name or zero port leaves the registry unchanged. Repeating a registration
or removing an absent service is also harmless: requests still return `{ok:true}`,
and unchanged registrations do not cause another network announcement.

Malformed registration, unregistration, observed-address, and activity requests
return JSON-RPC error `-32602` (invalid parameters), without changing state.
Unknown requests return `-32601` (method not found). Notifications receive no
error reply. `log/set-level` accepts `debug`, `info`, `warn`, or `error`, and
rejects an invalid level with `-32602` when sent as a request.

`discovery:get-nodes` filters by service when one is supplied. An absent, empty,
or unparseable filter returns all records. Results use a stable order.

The broker also sends `discovery:local-node-info {info, capturedAt}` as a notification,
carrying measurements received from its own node-info worker over private
stdio. Scanner attaches these samples to its locally owned registry record,
independently of any identity or address advertised over mDNS. The private
`capturedAt` time is Unix milliseconds on the shared local host clock. Scanner
includes elapsed pipe delivery in GPU age; a missing or future capture time makes
GPU telemetry unavailable while retaining display inventory.

Notifications (Scanner → caller):

| Method | Payload and meaning |
|---|---|
| `ready` | `{version}`, once at startup |
| `discovery:node-discovered` | `{node: DirectoryNode}`, a new record |
| `discovery:node-updated` | `{node: DirectoryNode}`, changed identity or inventory |
| `discovery:node-removed` | `{node: DirectoryNode}`, a record removed or replaced |
| `discovery:node-info` | `{discoveryRecordId, hostUuid, clusterUuid?, identityVerification, info}`, full hardware from pinned mTLS or owned IPC, or an unverified display-only preview |
| `discovery:node-telemetry` | `{hostUuid, gpuUtilizationPercent, telemetryValid, msSince}`, GPU-usage data for scheduling |

`discovery:node-info` sends a complete hardware snapshot through the broker.
`info` includes GPU usage and video memory (VRAM), CPU, system memory, sample
validity, and age. Its `identityVerification` is `pinned` for an authenticated
remote source or `local` for an owned worker. Public previews use `unverified`
and the current discovery record ID. Their `info.hostUuid`, `info.clusterUuid`,
and `info.peer` claims are stripped. The broker retains observations separately
by source and record, ages them on snapshot reads and replay, and exposes them
as optional `AvailableNode.info`. Only local and pinned measurements enter
service and scheduling inputs.
For client display, the broker exposes a stable `info.observedAt` receipt time for
the whole display observation. Cached snapshots and live updates preserve it
while GPU sample age increases. It is separate from scanner identity and
scheduling evidence.
Directory events report changes to identity, services, models, and hardware
identity or capacity. Utilization and used-memory changes do not emit directory
updates or advance `lastSeen`. Local and authenticated peer samples still produce
full hardware updates and compact scheduling telemetry. `lastSeen` records the
time the directory record last changed.

`ready` is sent before network discovery starts. It reports worker startup,
not successful advertising, peer authentication, or availability of other
services. Closing the control connection or receiving an operating-system
termination signal stops the worker. A `shutdown` request is answered before
cancellation. Malformed JSON-RPC frames are logged without ending the read loop.

Hardware sizes are in bytes and utilization is in percent. `telemetryValid`
and `msSince` describe the source GPU sample, not the age of the directory
record. When validity is false, clients ignore the age. Optional CPU, memory,
and usage fields may be absent when measurements are unavailable.

Remote scheduling measurements are sent only for verified peers. The broker
checks that the sample still belongs to the approved peer before caching or
forwarding it to scheduler. Local samples must come from owned worker IPC.
Manually configured remote nodes meet the same pin and response-identity checks;
explicit engine configuration does not authorize using public hardware data.
Measurements without an accepted source sample are unavailable; cached samples
retain their elapsed age under the scheduler's freshness rules.

### 7.1 Discovery records and trust

The version 1 mDNS TXT record requires a nonempty `uuid` (host UUID), with optional
`cluster-uuid` (the certificate principal, the node identity named in its
certificate), addresses in preferred order, and registered service ports.
Unclustered announcements omit `cluster-uuid`. Different advertised UUIDs are
retained as unverified discovery data and may receive public hardware previews;
neither claim establishes a verified identity.
Node-info reports the same certificate identity as `clusterUuid`. This is the
individual node's identity, separate from the shared cluster identifier.
Hardware and model lists are read from the services rather than included in TXT.

The DNS SRV (service-location) record uses the fixed port `14318`; it does not
select a service's connection port. TXT service ports identify where to try
connecting. Verified peer ports come from authenticated node-info metadata.
Unknown service keys with valid ports are retained. The TXT record lists at most
four addresses, in preferred order.

The service instance name comes from the hostname. It can help distinguish
discovery records, but cannot establish a verified host's identity. Different
hosts may use the same name.

Scanner assigns each unverified record an independent discovery record ID.
The canonical peer record for an approved node links its identity to its exact
saved certificate. That saved pin selects the certificate used to authenticate
a known host, even when an announcement omits or changes its `cluster-uuid`
claim. An announcement cannot select a different certificate or replace verified
service addresses before authentication succeeds.
Once an observation authenticates, its subsequent announcements stay hidden
while that exact proof remains current and its host UUID and advertised cluster
identity are unchanged. Changing either UUID claim, including adding or clearing
the cluster identity, clears the observation's previous association. New claims
require authentication before updating a verified peer.

Scanner rejects announcements with a missing host UUID or a claim to the local
host. It removes only the cached discovery observation and its previous peer
association. Announcement changes and removals cannot directly remove a verified
peer, change its saved pins, or close its connections. The peer's own
authenticated refresh and availability checks determine when its record expires.

`trusted` is true for a verified peer whose pin is still approved. For the local
record it follows actual local membership. Adding a pin schedules authentication
of matching discoveries, rather than immediately trusting their announcements.
Unverified rows cannot be merged into member rows by matching UUID, name, or
address in the broker or either frontend.

When the user removes a peer or leaves the cluster, the affected verified peer
records disappear from the broker's desktop and TUI views. Scanner restores
their known observations as separate unverified discoveries without waiting for
another announcement change. Replacing a certificate or admission also
invalidates the prior association. An unverified discovery or a saved manual
configuration cannot preserve the removed peer's verification.

### 7.2 Inventory and model transport

Node-info serves `GET /v1/node-info` on its registered `ni` port (normally
`14318`) with a shared split listener, which accepts both HTTP and mTLS on the
same port:

| Reader | Transport | Use of the response |
|---|---|---|
| Scanner or manual worker collecting an unpaired preview | Public, unencrypted HTTP, including while clustered | Unverified hardware details for broker-delivered display only |
| Service reading an approved remote peer | Certificate-pinned mTLS | Authoritative peer metadata and telemetry |
| This host's services | Private broker/worker IPC | Locally owned inventory and scheduling measurements |

Workers collect public hardware previews for unverified records. Desktop and
TUI receive hardware through broker snapshots and pushes. Discovery and pairing
use the addresses and service ports supplied by mDNS.
The split listener keeps public HTTP available while live membership gates the
peer node inventory handler. Neither plaintext HTTP nor TLS without a
matching peer pin can supply authenticated remote data to services.

Local hardware comes from the node-info process started by the broker through
private worker IPC. Approved remote measurements require pinned mTLS. These
channels establish the source of each service observation.

Node-info publishes `nodeinfo:info-updated {info, capturedAt}` over its owned
stdio channel at startup and every two seconds. The broker forwards those
snapshots as `discovery:local-node-info`, adding elapsed time to sample age when
forwarding or replaying cached data. Each hop captures its timestamp before
serialization and pipe writes, and the receiver includes that delivery time in
sample age. This private clock is separate from display `info.observedAt` and
never uses a remote response timestamp. Scanner emits full local hardware back to
the broker for client delivery. Local hardware and model inventory updates share
serialization through directory mutation and full-node publication, so consumers
receive those snapshots in the order they were applied. The public listener serves
unverified previews. A discovered record must not be treated as local merely because
its advertised address points to this computer, including a loopback address such
as `127.0.0.1` or `::1`.

The mTLS request handler checks current membership and requires the client's
certificate to match its saved pin exactly on every request, including reused
connections. Scanner checks the server's certificate the same way, and checks
that response `hostUuid` and `clusterUuid` match the node UUID in that certificate
and the approved node identity. Keeping a certificate and private key after
leaving a cluster does not keep the authenticated endpoint available.

Scanner uses the record's current address list for each read. It remembers a
successful address separately for node-info and engine-manager. If that address
stops answering or is removed from the list, Scanner tries the other addresses.
Among successful alternatives, it chooses the first in the record's order.
Unverified candidates cannot replace a verified peer's remembered address or
supply addresses used as evidence to choose this host's own address.

The peer node inventory response includes hardware fields and a `peer`
object containing the serving host's name, addresses in preferred order, and
registered service ports. These details come from its own local Scanner registry,
the list of services registered on that host:

```json
{
  "peer": {
    "name": "workstation-b",
    "addresses": ["192.0.2.20"],
    "services": {"ni": 14318, "ol": 11434, "em": 14322}
  }
}
```

The broker sends the local snapshot to node-info over stdio using
`nodeinfo:set-peer-metadata {hostUuid, clusterUuid, peer}`, with result
`{ok:true}`. It refreshes the snapshot after local identity, name, address, or
registration changes and resends it on restart. Peer node inventory is served
only when the snapshot agrees with the current cluster identity. Until
setup is complete, or while identity is changing, it returns `503`, while public
hardware inventory is available independently.

Peer node-info responses must fit within 65,536 bytes; Scanner rejects larger
responses. This limit applies to the authenticated service interface.

For an approved peer, a failed pin check, TLS exchange, or identity validation
never triggers a retry over unencrypted HTTP. For public responses, absent
`clusterUuid` means unknown and an empty value is an unclustered claim, neither
of which changes an established peer's membership or verified data.

Scanner reads a peer's models using the verified engine-manager (`em`) service
details and `GET /v1/models` over pinned mTLS. For the local host, Scanner uses
its own registry and unencrypted HTTP over loopback. It adds the combined model
list, `modelsByEngine`, `loadedByEngine`, and `downloadedByEngine` to the directory,
retaining the last
successfully read data during temporary failures at the same authenticated
source. A change to the peer's proof, address list, or engine-manager port clears
all four inventories. Removing engine-manager also clears them. A new or changed
source schedules an immediate model sweep while engine-manager is present.
Local publication uses retained models without waiting for HTTP while holding
the local identity lock. A local registration also schedules the model sweep
without waiting for its next periodic run.

`downloadedByEngine` carries downloaded inventory for engines such as vLLM that
advertise only their served model in `modelsByEngine`. It remains available while
the engine is stopped, for example `{"downloadedByEngine":{"vllm":["Qwen/Qwen3.5-4B"]}}`.

Scanner compares model lists without depending on their order. A present engine entry
with an empty list reports no models for that engine; a missing entry means
no inventory was supplied for it. Omitted per-engine maps leave the combined
model list available without supplying per-engine details.

Hardware and model updates preserve each other's fields. Scanner accepts a result
into records or caches only while the target record, the service's address and port,
and its approved identity are still current. A result for a removed or changed
target cannot restore old data, even if the network request succeeded.

### 7.3 Frontend display and service decisions

Desktop and TUI obtain GPU, VRAM, CPU, and memory through optional `info` in
broker `discovery:get-nodes` / `discovery:nodes-changed` snapshots and live
`discovery:node-info` updates. Workers own collection and authentication for
local, paired, unpaired, and manual records. Scheduling snapshots carry service
state, and hardware displays use the full client payload.

Scanner sends its directory changes directly to the broker. `discovery:subscribe`
is a broker method, not a Scanner method. Subscribed workers receive all records
matching their requested services in `discovery:nodes`; subscribed desktop and
TUI clients receive full snapshots in `discovery:nodes-changed`. Each snapshot
replaces the consumer's complete set of records.

Both clients receive node identity, trust state, service addresses, and model
records from the broker. Hardware updates must match the current record ID and
verification. Local and pinned records use owned IPC or authenticated peer
measurements; unverified records may carry public previews. A preview updates
hardware display fields only and cannot promote identity or supply service input.
The broker retains each source's receipt time, adds elapsed age on query/replay,
and withdraws hardware when its source, proof, or endpoint is replaced.

Removing an unverified discovery, including when it is replaced by a verified
record, does not mean the verified peer departed. Removing a manual configuration
cannot remove an independently verified peer record. Membership or pin removal
still clears the affected verification, regardless of other discovery or manual
records that remain.

Scanner, manual-node workers, broker, scheduler, proxies, and other services
must not use public node-info responses for authoritative decisions. Remote
node-info reads use pinned mTLS; local measurements use owned worker IPC. A
manually entered address can be a connection candidate, but hardware data must
still authenticate before it is used by services. Public HTTP measurements
cannot be used to select engines, change routing, prove peer availability or
membership, or populate scheduling caches.

An authenticated read failure leaves service data unavailable or aging according
to its freshness rules. Subsequent accepted service measurements require current
pinned mTLS. Public previews serve unverified display records separately.

### 7.4 Contract ownership

Changes to snapshot notifications and identity-verification fields are coordinated
across shared Go types, broker forwarding and caches, proxies that read the
directory, desktop bridge, and TUI. Operator TLS settings cannot replace exact
certificate-pin checks for an approved peer.

Discovery record IDs and verification fields belong to the JSON-RPC contract.
The version 1 TXT schema describes advertised identity claims, address candidates,
and service ports (§7.1).

## 8. Dependencies

- **Broker**: `nvpair-ui-broker` starts and supervises Scanner and registers local
  services.
- **Cluster manager**: `nvpair-cluster-manager` maintains node identity and trusted
  certificate pins.
- **Reads data from**: node-info and engine-manager. The broker distributes its
  results to frontend clients, proxies, and scheduler.
- **Shared packages**: `nvpair-shared/noderec`, discovery/address helpers,
  `clustertrust`, JSON-RPC, and `applog`. No additional trust service or keys.

## 9. Data Ownership

- **Owned**: the local service list, discovery directory, unverified records,
  information used to choose addresses, request/retry state, and cached inventory
  and measurements. These are kept in memory for the current session.
- **Source of truth**: local registry for this host's advertised services,
  cluster-manager for membership/pins, node-info for hardware samples, and
  engine-manager for model lists. Scanner builds its directory from these sources.
- **Storage**: Scanner reads identity/trust state and keeps discoveries in memory.
  Node-ID resolution may create `node-id.json` before cluster-manager
  supplies an identity. When the local UUID changes, Scanner retires the old local
  record so clients do not retain both identities.

## 10. Design Constraints

- **Performance**: Scanner keeps discovery and two-second telemetry responsive by
  limiting simultaneous reads. Nodes awaiting authentication have separate
  identity and retry state from verified peers. Node-info reads have request
  deadlines and a 64 KiB complete-body limit; model reads have deadlines and a
  16 MiB JSON decoding limit.
  Waiting behind another node-info read must not consume a request's own timeout
  budget. Model-request deadlines allow engine-manager's bounded inventory
  collection to finish.
- **Reliability**: Scanner refreshes without new mDNS announcements, tries addresses
  in preferred order, and retains previously read data with its correct age. Failed
  discoveries cannot replace a peer's last verified address or interrupt its reads.
  Scanner reconsiders local addresses periodically and after network changes. The
  broker's observed-address list, addresses that peers have reached on this host,
  replaces the previous list so withdrawn observations stop influencing selection.
- **Security**: services use pin-based mTLS in Go. Verifying one
  connection does not verify unrelated unencrypted data or every address in an
  announcement. Scanner rechecks the approved node identity and exact pin before
  accepting a response.
- **Transport**: one registered node-info port supports public worker previews
  and peer node inventory over pinned mTLS. Public HTTP data is for display only;
  service logic uses pinned remote reads or private local IPC.

## 11. Assumptions

- The broker owns the Scanner control connection and starts/stops local workers.
- Unapproved nodes can announce themselves. They can read public hardware
  inventory for display, including while the serving node is clustered.
- Authenticated peers can report their own details. Authentication proves which
  node sent them, but cannot prove that a compromised approved node reports
  truthful measurements.
- Local cluster state controls membership. Network reachability alone does not.

## 12. Failure Modes and Mitigations

| Failure | Behavior |
|---|---|
| Malformed service or address fields | Unusable fields are ignored; usable unverified data is retained within discovery limits |
| Missing host UUID or local-host claim | The announcement is rejected and its cached discovery observation is removed; verified peers keep their independent authenticated availability checks |
| Peer authentication or identity mismatch | The discovery is not marked verified, the existing peer record is unchanged, and no unencrypted retry occurs |
| Inventory/model timeout or unreachable peer | Previously read inventory is retained, measurements age, and retries use increasing intervals up to the limit |
| Pin removal/replacement or local departure | Verification is cleared, affected idle connections close, routing stops using the peer, and scheduling samples are discarded; in-flight results are checked against current trust before acceptance |
| Response arriving after a trust change | The response is discarded before records are updated or telemetry is sent |
| Unverified UUID/name/address collision | Discovery and configuration records remain separate and cannot overwrite local or verified data |
| Worker restart | The broker resends local registrations/details, remote records are authenticated again, and cached sample age is preserved |
| Peer node inventory or local worker unavailable | Service measurements become unavailable or retain their elapsed age until the next accepted source sample |
| Public HTTP read fails | The last accepted preview retains its original sample age, or hardware stays unavailable if no preview exists; authenticated service reads remain independent |
| Public HTTP data enters a verified or scheduling cache | The data is rejected based on source, including public data attributed to manual nodes |
| Local network advertising unavailable | Scanner logs the failure and continues browsing and maintaining the local record |
| Temporary failure to read saved peer pins | A failed read does not count as removal of every peer pin |

The local announcement follows current identity and membership. A remote peer
does not lose its approved membership merely because its authenticated endpoint
stops answering. After a removal is processed, an old response or still-open
connection cannot restore its former trust.

## 13. Observability

- **Logging**: structured stderr logs cover advertising failures, address changes,
  node-info outage and recovery, model-request failures, invalid local snapshots,
  and notification failures. Depending on the event, fields identify the host,
  address, endpoint, or method. Discovery changes are emitted as JSON-RPC events.
  Key material, certificates, pairing PINs, prompts, and response bodies are never logged.
- **Freshness**: snapshots report whether samples are usable and how old they are.
  Resending a sample never reduces its age. Failed reads must not make a node
  appear freshly measured with zero usage.
- **Events**: clients receive notifications when directory data changes. Seeing
  the same announcement or reloading unchanged trust data does not send another update.

## 14. Example runtime flow

The broker starts Scanner, registers the local service ports, and receives its
directory and telemetry notifications. Scanner announces this host and listens
for other nodes on the local network. Unpaired nodes appear in discovery rows.
Approved peers are authenticated before their details enter verified records.

```mermaid
sequenceDiagram
    participant B as Broker
    participant S as Node Scanner
    participant P as Network node
    participant U as Desktop and TUI
    participant Q as Scheduler
    B->>S: Register local services
    B->>S: Local worker measurements over private IPC
    S-->>P: Advertise local node over mDNS
    P-->>S: Node announcement over mDNS
    alt Approved cluster peer
        S->>P: GET node-info with pinned mTLS
        P-->>S: Authenticated identity, services and hardware
        S->>P: GET models with pinned mTLS
        P-->>S: Available and loaded models
        S-->>B: Verified node details and measurements
        B-->>Q: Verified GPU usage
    else Unpaired node
        S-->>B: Unverified discovery record
        S->>P: GET public hardware preview over HTTP
        P-->>S: Unauthenticated hardware
        S-->>B: Sanitized preview under discovery record ID
    end
    U->>B: Subscribe to discovery
    B-->>U: Initial node snapshot with hardware
    B-->>U: Full hardware updates matched to current record
```

The diagram shows worker-owned collection and broker-delivered client hardware.
Public previews never flow into scheduler or verified peer records. Models refresh every 15 seconds;
peer metadata, hardware, and measurements share the two-second sampling loop,
independently of mDNS changes. The broker forwards trust changes to Scanner and clears affected
verification and service samples when membership or pins change.
