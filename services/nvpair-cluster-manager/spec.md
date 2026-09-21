<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Microservice: Cluster Manager

## 1. Purpose
Owns cluster membership — which nodes belong to the same logical cluster — and the interactive join/leave handshake that establishes it. The Cluster Manager invites nodes, receives and responds to invites, removes nodes, and maintains the authoritative local view of cluster members, giving the customer the data to render "who is in my cluster" and the controls to change it. `cluster_id` is the membership identifier this service negotiates around, while the join/leave protocol itself lives here.

Because membership **is** trust, this service is also the **trust fabric** for the cluster: it owns each node's cryptographic identity (a stable keypair), bootstraps mutual-TLS trust during the join handshake, and maintains the **trusted-node store** that backs mTLS on every inter-node interface in the system. `nvpair-workload-manager` and the other inter-node services consume the certs and trust roots this service provisions rather than each rolling their own.

## 2. Scope
**In scope**
- Maintain the authoritative local membership set for this node's cluster (who's a member, who has a pending invite).
- Drive the interactive invite handshake end to end: send an invite to a peer, surface an incoming invite to the local Broker, relay the user's accept/decline, and reconcile both sides' membership on the outcome.
- Remove a node from the cluster and notify the affected peer so membership stays symmetric.
- Resolve peer reachability from the broker-relayed consolidated discovery directory, with an explicitly supplied address taking precedence.
- **Own this node's cryptographic identity**: generate and persist a stable per-node keypair and self-signed certificate (keyed to a stable node UUID, §7.4) on first run.
- **Bootstrap mTLS trust via an OOB-authenticated pairing**: run an **EAP-NOOB** (RFC 9140) key exchange over the invite handshake, authenticated by a **six-digit PIN** the user carries out of band, to exchange and pin each node's self-signed certificate (§7.4). Persist the resulting **trusted-node store** (UUID → pinned cert) and revoke a pin when a node is removed.
- **Enforce mTLS on its own inter-node interface** once paired: serve HTTPS with `RequireAnyClientCert`, verify the presented cert against the trusted-node store, and reject untrusted callers (`403`). (The pairing exchange itself runs before any pin exists — see §7.2/§7.4.)
- **Expose the trust fabric** (local keypair + trusted-node store) so other inter-node services can authenticate with mTLS without rolling their own.

**Out of scope**
- Owning the user-facing `cluster_id` setting — that remains `nvpair-node-settings`. Cluster Manager does persist its active admission `{clusterId, epoch}` as crash-consistent security state; this is authoritative for authorization/recovery, while the Broker-facing setting remains the UI configuration mirror.
- Being a full PKI / certificate authority. There is no central CA: each node self-signs its own leaf and trust is established pairwise at join time, authenticated by the EAP-NOOB pairing PIN (§7.4). No CRL/OCSP infrastructure; removal-from-cluster is the revocation mechanism.
- Workload and error propagation across the cluster — those are `nvpair-workload-manager` and `nvpair-errors`. Membership defined here is the set those services should eventually scope their fan-out to, and the trusted-node store this service owns is what authenticates that traffic, but that wiring is separate work.
- The pre-existing **bring-your-own** operator TLS for the `nvpair-node-info` HTTP surface (`tls-settings.json`, `--cert`/`--client-ca`, etc.). That is a separate, manually-configured mechanism; the cluster trust fabric here is self-managed and auto-provisioned. Converging the two is a possible follow-up (§4), not part of this contract.
- Auto-accepting invites. Invites are always interactive by design; there is no auto-join path because the user-entered PIN is the out-of-band step that authenticates the pairing (§7.4).

## 3. Key Use Cases
- **First run — mint identity**: on first start the Cluster Manager generates this node's stable UUID + keypair and a self-signed certificate, and persists them (§7.4). This identity is reused across restarts and is what every peer pins to authenticate this node.
- **Create a cluster (founder)**: a node with no cluster (`cluster:get-node-id` returns `clusterId: ""`) calls `cluster:create` to initialize a brand-new cluster of one — minting a fresh, globally-unique `clusterId` (UUID v4) plus a `clusterFriendlyName`, recording itself as the founding member, and reporting the new identity via `cluster:identity-changed` so the Broker persists it to `nvpair-node-settings`. An unclustered node has three ways into a cluster: call `cluster:create` **explicitly** (e.g. to set a `clusterFriendlyName` up front), **just invite a peer** — the first `cluster:invite-node` auto-founds a cluster of one as a side effect (§7.0/§7.2), so no separate create step is required — or **skip both and wait to be invited**, adopting the inviter's cluster on accept. `cluster:create` is therefore optional: it is the explicit form of the same founding the first invite performs implicitly, so a UI does not have to orchestrate "check membership, then create, then invite" itself.
- **Invite a node (outbound)**: the Broker emits `cluster:invite-node` (`params.address`, optional `params.port`/`params.nodeId`) on `stdin`. If this node isn't clustered yet, the invite **first auto-founds a cluster of one** (the same in-process mint-and-record founding as `cluster:create`, emitting `cluster:identity-changed`) and then proceeds. The Cluster Manager (acting as the EAP-NOOB **Server**) opens a pairing session with the target and runs the EAP-NOOB **Initial Exchange** in-band — both sides carry their full certificate PEM in the EAP-NOOB `ServerInfo`/`PeerInfo` objects (§7.4) — then derives a **six-digit PIN** from the OOB secret and returns `{inviteId, state: "pending", pin}` so the inviter's UI can display the PIN. No trust is established yet. If the target refuses because it is **already clustered**, the Initial Exchange comes back with an explicit rejection and the result is `{inviteId, state: "rejected", reason: "already-clustered"}` — no PIN (and a solo cluster this invite just auto-founded is dropped again). (A well-behaved UI also skips the invite when it can already see the target's `cluster-uuid=` over discovery; this is the authoritative backstop.)
- **Receive an invite (inbound)**: the pairing's Initial Exchange arrives over the inter-node interface; the Cluster Manager (EAP-NOOB **Peer**) runs it automatically (machine-to-machine, no human yet), stashes the inviter's cert from the transcript (not yet trusted), records a pending-inbound invite, and emits a `cluster:invite-received` notification on `stdout` so the Broker can prompt the local user to enter the PIN shown on the inviting node. A new invite from the same authenticated `fromNodeUuid` **supersedes that sender's older pending inbound invite**: the older session is canceled locally, `cluster:invite-canceled` dismisses its prompt, and a best-effort decline tells the inviter to invalidate the old PIN. Pending invites from different sender UUIDs remain independent. **If this node is already clustered it refuses the pairing up front** — before creating a session, minting a PIN, or emitting `cluster:invite-received` — replying on the pairing channel with `HTTP 409` and `{rejected: true, reason: "already-clustered"}` so the inviter reports `state: "rejected"`. An already-clustered node must **leave** (or be removed) before it can pair again.
- **Respond to an invite (enter the PIN)**: the user reads the PIN from the inviting node (out of band — in person, phone, chat) and accepts; the Broker sends `cluster:respond-to-invite` (`params.inviteId`, `params.accept`, `params.pin`). The Cluster Manager feeds the PIN to EAP-NOOB (`OOBInputNoob`, §7.4) and then drives the **Completion Exchange** back to the inviter (§7.2); on a verified PIN both sides reach the EAP-NOOB `Registered` state. Each side then reads the peer's now-authenticated cert from the pairing transcript and **pins it into the trusted-node store** (keyed by the peer's UUID), records the peer as a member, and (on the joiner) adopts the cluster's identity. A **wrong PIN** fails the pairing (no pin, no membership) and, mirroring the decline path, the joiner **best-effort notifies the inviter** over the pairing channel (`phase:"fail"`, carrying `reason:"incorrect-pin"`) so the inviter's invite flips to `failed`, its EAP session is torn down (PIN invalidated), and it emits `cluster:invite-failed` — the joiner (EAP Peer) detects the bad MAC first and never drives the Completion round that would otherwise surface the failure on the inviter, so without this signal the inviter would stay `pending` until its TTL expired. A decline works the same way over `phase:"decline"` so the inviter's invite flips to `declined`, its EAP session is torn down (PIN invalidated), and it emits `cluster:invite-declined` — without that signal the inviter would stay `pending` forever and UIs that lazily auto-create a solo cluster for the invite could not abandon it.
- **Poll invite status**: the inviting Broker calls `cluster:invite-status` (`params.inviteId`) to read the current state (`pending` → `paired` / `declined` / `expired` / `failed`) without waiting on a push. The joiner drives the Completion Exchange back to the inviter when the user enters the PIN (§7.2), so the inviter observes `paired` via this read or the `nodes:changed` push; a decline or a wrong-PIN failure is observable the same way (or via the `cluster:invite-declined` / `cluster:invite-failed` push). A wrong-PIN `failed` also carries `reason:"incorrect-pin"` so the UI can show a specific message.
- **List cluster members**: the Broker calls `nodes:get-initial` to seed its view with the current membership set (confirmed members plus any pending invites).
- **Remove a node (= revoke one admission)**: the Broker emits `nodes:remove` (`params.nodeId`); the Cluster Manager first persists signed proof targeting the peer's exact authenticated admission, notifies it while its pin can still authenticate mTLS, then revalidates that the admission did not change before deleting the pin/member. A concurrent newer readmission is preserved. Removal proof, not elapsed time or a bare `403`, is the revocation mechanism.
- **Edge case — duplicate / retried pairing or removal messages**: a re-delivered message correlated by the same `inviteId` / `nodeUuid` is idempotent — it does not create a second pending entry, re-pin a cert, or double-apply a membership change. A deliberate retry that creates a new `inviteId` is distinct, but on the joiner it supersedes any older pending invite from the same authenticated sender UUID.
- **Edge case — peer unreachable**: a target that is down or partitioned can't be paired or cleanly removed. The Initial Exchange uses bounded timeouts/retries; an undeliverable invite ends `failed`, and a removal still takes effect locally — the pin is dropped immediately, so the removed node is denied even before it learns it was removed.

## 4. Open Questions / Risks
- **Membership persistence (closed)**: confirmed members, pins, active admission, removal proofs, and teardown intent survive restart; in-flight pairing sessions remain transient.
- **`cluster_id` semantics on accept (closed)**: a joiner adopts the inviter's cluster id after the authenticated pairing commits; a founder mints a UUID v4.
- **Who writes `cluster_id` (closed)**: Cluster Manager emits `cluster:identity-changed`; the Broker persists the UI setting. Cluster Manager separately persists admission state required for authorization and crash recovery.
- **Authentication — decided**: the inter-node interface is **mTLS over HTTPS**, authenticated against the trusted-node store this service owns (§7.4). The trust model is **self-signed leaf certs pinned at the join handshake**, keyed by a **stable per-node UUID** rather than hostname — see §7.4 for the rationale and the on-disk layout. The pinning is bootstrapped by an **EAP-NOOB (RFC 9140) pairing authenticated by a user-carried PIN** (the in-tree `eap-noob` library). Residual decisions:
  - **⚠️ Six-digit PIN is temporary security debt — MUST UPGRADE before production cross-node trust**: EAP-NOOB's security depends on the out-of-band secret (`Noob`) being high-entropy, because the in-band Completion Exchange exposes `NoobId = H("NoobId", Noob)` and the MACs over the transcript. A six-digit PIN (~20 bits) is therefore **offline-brute-forceable by an active man-in-the-middle** on the LAN: an attacker who relays the in-band exchange can recover the PIN from `NoobId`, derive the keys, and substitute its own cert. The PIN as specified defends against **passive eavesdroppers and accidental/unauthorized joins**, not a determined active MITM. This is accepted temporarily to ship the flow; it must be replaced with a high-entropy OOB value (a copy/paste pairing code or QR encoding a high-entropy `Noob`, ~16–32 bytes) before the cluster trust is relied on.
  - **Library delta the PIN needs**: today `eap-noob` random-generates the 16-byte `Noob` and its OOB API exchanges a full `OOBMessage{PeerId, Noob, Hoob}`. The six-digit design needs **one** small additive change — a caller-injected `Noob` on each role (`Server.OOBOutputWith` / `Peer.OOBInputNoob`) — so the human-carried digits are the only OOB input; nothing else is relayed (the joiner already holds `PeerId` and recomputes `Hoob` locally). Concrete API and the exact PIN→16-byte-`Noob` encoding are in §7.4. The delta disappears when the OOB upgrades to a high-entropy pairing code.
  - **CA-less pinning vs a cluster CA (leaning CA-less)**: pinning each peer's self-signed leaf needs no central authority and matches the no-founder, peer-to-peer model — but it's O(n²) pins and re-pins on rotation. A shared cluster CA (the accepting node, or the cluster founder, signs joiners) would shrink trust to "verify against the cluster CA" but centralizes issuance. Starting CA-less; because certs ride in the EAP-NOOB transcript, swapping the pinned-leaf check for a CA-chain check later needs no wire change.
  - **Cert lifetime & rotation (open)**: proposed long-lived leaves (1–2 years) with a re-pin path when a node rotates its key (a node whose pin no longer matches is treated as untrusted until re-invited or an explicit re-pin flow runs). Exact lifetime, rotation trigger, and whether rotation can be done without a re-invite are TBD.
  - **Sharing the trust fabric with other services (open)**: `nvpair-workload-manager` and `nvpair-errors` peer-sync also need mTLS. Intended: they consume the same keypair + trusted-node store this service owns (via a shared on-disk location or a small local query API). The exact handoff (file paths the Broker passes, vs. an IPC call) is unspecified here.
  - **Convergence with BYO node-info TLS (open)**: whether the auto-provisioned cluster identity should eventually also serve the `nvpair-node-info` HTTPS surface (replacing the operator-supplied `tls-settings.json` certs) or stay separate.
- **Invite expiry (closed)**: pending invites expire after a default TTL of **5 minutes** with no accept/decline, on **both** sides. Inbound (receiver) expiry drops the tentative pending-inbound member + session, emits `cluster:invite-expired` so the UI dismisses the PIN prompt, and best-effort signals the inviter (`phase:"expire"`) so it tears down immediately rather than waiting for its own TTL. Outbound (inviter) expiry tears down the EAP session, emits `cluster:invite-expired`, and runs provenance-safe invite-created solo cleanup; it is a purely local fallback (it does **not** signal the joiner — the receiver expires its own inbound invite, so both converge on `expired` rather than a timing-dependent `expired`/`canceled`). A newer invite from the same sender still *immediately* supersedes an older pending inbound invite as `canceled` (that is deliberate, not a TTL event). Exact TTL remains tunable later.
- **Membership-change push (proposed, needs confirmation)**: a `nodes:changed` notification (full member snapshot on every change) would let the Broker keep its view live without polling `nodes:get-initial`. Included below as a proposed addition, not in the user's original list.
- **Split membership convergence**: periodic roster reconciliation, durable removal-proof replay, and proof-bearing `403` responses converge nodes that were offline or restarted during a removal.
- **Risk — discovery snapshot staleness**: the resolver map is only as current as the last `discovery:nodes` snapshot, so a departed node may linger as a resolvable target and a freshly added node may appear slowly. A Broker-supplied address bypasses the resolver entirely, so this bounds transitive resolution rather than the invite path.

## 5. Requirements

**Functional**
- Accept `cluster:get-node-id`, `cluster:set-identity`, `cluster:create`, `nodes:get-initial`, `cluster:invite-node`, `cluster:invite-status`, `cluster:respond-to-invite`, `cluster:cancel-invite`, `nodes:remove`, `cluster:leave`, and `log/set-level` requests from the parent over `stdin` (or a named pipe) and return JSON-RPC results.
- Self-own the node identity: on `cluster:get-node-id`, return the persisted `nodeUuid` plus the hostname-derived `nodeId`/`name`, cert fingerprint, and current `clusterId` (§7.4). Accept the *cluster* identity (`clusterId`/`clusterFriendlyName`) only via `cluster:set-identity`, and emit `cluster:identity-changed` when a pairing causes the local `clusterId` to change.
- On `cluster:create` (valid only when unclustered), mint a fresh, globally-unique `clusterId` (random UUID v4 — never caller-supplied, never reused), set `clusterFriendlyName`, record this node as the founding member, emit `cluster:identity-changed`, and return the new identity; reject with `-32004` if the node is already clustered.
- On `cluster:invite-node`, **first auto-found a cluster of one if this node is unclustered** (the same mint-and-record path as `cluster:create`, emitting `cluster:identity-changed`), then create a pending outbound invite with a unique `inviteId`, run the pairing Initial Exchange (§7.2), and return the `inviteId`, `state` (`pending`), and the six-digit `pin` to display.
- Drive the pairing as the EAP-NOOB **Server** on `cluster:invite-node`: run the Initial Exchange in-band, embed the local cert PEM in `ServerInfo`, derive the six-digit PIN, and return it for display.
- On an inbound pairing (EAP-NOOB **Peer**), run the Initial Exchange automatically (embedding the local cert PEM in `PeerInfo`), record a pending inbound invite, and emit `cluster:invite-received` to the Broker; never pin or change membership until the user enters the PIN.
- On `cluster:respond-to-invite` with the PIN, feed it to EAP-NOOB (`OOBInputNoob`, §7.4), drive the Completion Exchange (§7.2), and on success pin the peer's transcript cert and record membership (joiner also adopts cluster identity). A wrong/expired PIN or a decline establishes no pin and no membership, and clears the pending-inbound member row it recorded for the inviter. On decline, also best-effort POST `phase:"decline"` to the inviter so its outbound invite reaches `declined` and its pairing session is dropped; on a wrong-PIN failure, best-effort POST `phase:"fail"` (with `reason:"incorrect-pin"`) so its outbound invite reaches `failed` the same way.
- On `nodes:remove`, persist proof for the exact pinned admission, notify the peer, then revalidate the target admission before de-pinning; preserve a concurrently admitted newer incarnation.
- Maintain a queryable membership set returned by `nodes:get-initial` (confirmed members and pending invites, distinguished by a `state` field).
- Resolve peer Cluster Managers from the consolidated discovery directory: subscribe to the Broker's discovery relay for `cl` nodes and rebuild the resolver map from each snapshot, so a node can be invited whether it was discovered on the LAN or entered manually. A Broker-supplied address always takes precedence over the resolver.
- Deduplicate inbound invites and responses by `inviteId` and removals by `nodeUuid` so retries are idempotent (no duplicate pending entries, no re-pinned certs, no double-applied membership changes).
- Validate inbound payloads; reject malformed envelopes or unknown `method` / endpoint (`400 Bad Request`).
- Generate and persist a stable per-node UUID + keypair + self-signed certificate on first run; reuse them across restarts (§7.4).
- Persist the trusted-node store (UUID → pinned cert + metadata); pin a peer's cert on pairing success, drop it on removal, and reload it on startup.

**Non-functional**
- All inter-node traffic is **mTLS over HTTPS**: the server requires a client certificate and verifies the presented cert against the trusted-node store; an absent or unpinned cert is rejected (`403 Forbidden`). The one exception is the **pairing channel** (`POST /v1/cluster/pairing`), which by definition runs before a pin exists and is instead authenticated by the EAP-NOOB PIN (§7.2/§7.4).
- The pairing PIN is six digits **for now** — accepted, documented security debt (§4). The design must keep the OOB value swappable for a high-entropy pairing code without changing the local Broker contract.
- Private key material is stored with owner-only permissions (`0600` file / `0700` dir), matching the per-user config-dir convention `nvpair-node-settings` uses.
- Membership and trust changes must be idempotent and reconcilable — re-delivery converges to the same state (and same pin) on both nodes.
- A failed delivery to one peer (invite, response, or removal) must not block local state changes or other peers.
- Serialize `stdout` writes so frames never interleave.

## 6. Inputs and Outputs

**Inputs** — from the parent over `stdin` / named pipe (JSON-RPC 2.0 requests with `id`, including the local cluster identity supplied via `cluster:set-identity` — see §7.0/§8; the *node* identity is self-generated, §7.4), or from peer nodes via HTTP REST (pairing messages on the pairing channel, removals over mTLS). See §7 for transport details.

`ClusterNode` object (a cluster member or pending invitee):
```json
{
  id: string                 // logical node id (mDNS instance name / hostname); the membership key. Display/logical identity, NOT the trust principal.
  nodeUuid: string           // stable cryptographic identity; the trusted-node-store key and cert subject (§7.4). Independent of hostname.
  name: string               // human-readable display name (falls back to id)
  ipAddress: string          // reachable address for the inter-node interface
  port: number               // inter-node port (default 14321)
  clusterId: string          // the cluster this membership belongs to
  state: MembershipState
  joinedAt: number | null    // epoch ms when membership was confirmed; null while pending
  lastSeen: number | null    // epoch ms, last successful contact; null if never
}
```

`MembershipState` (enum): `"member" | "pending-outbound" | "pending-inbound"`
- `member` — join confirmed (both sides agreed).
- `pending-outbound` — we invited them, awaiting their response.
- `pending-inbound` — they invited us, awaiting the local user's response.

`Invite` object — the Broker-facing view of a pairing session. The certificates themselves are **not** in this envelope; they travel inside the EAP-NOOB transcript (`ServerInfo`/`PeerInfo`, §7.4) and are only surfaced once authenticated.
```json
{
  inviteId: string           // unique per pairing session; the dedup / correlation key
  fromNodeId: string         // inviter logical node id (hostname)
  fromNodeUuid: string       // inviter cryptographic identity (cert subject); what the invitee pins on success
  fromNodeName: string       // inviter display name
  toNodeId: string | null    // invitee logical node id, if known to the inviter
  clusterId: string          // the cluster the invitee would join
  clusterFriendlyName: string
  pin: string | null         // six-digit PIN — present in the cluster:invite-node RESULT (inviter displays it); never sent to the invitee and never logged
  state: InviteState
  createdAt: number          // epoch ms
  respondedAt: number | null // epoch ms when the PIN was entered / declined; null while pending
}
```

The `pin` is returned only to the **inviting** node and carried to the joiner **by the user, out of band** — never on the wire, never logged; the joiner supplies it back via `cluster:respond-to-invite`.

`InviteState` (enum): `"pending" | "paired" | "declined" | "canceled" | "expired" | "failed" | "rejected"`
- `pending` — Initial Exchange done; awaiting the user's PIN entry.
- `paired` — Completion Exchange succeeded; certs pinned and membership recorded (the success terminal state).
- `declined` — the invitee's user declined.
- `canceled` — the **inviter** aborted the invite before it completed (`cluster:cancel-invite`); the pairing session is torn down and the PIN invalidated. On the joiner, set when the inviter's cancel signal arrives.
- `expired` — TTL elapsed with no PIN entry (see §4).
- `failed` — pairing could not complete (peer unreachable, wrong PIN → MAC / `NoobId` mismatch, malformed).
- `rejected` — the target **explicitly refused** the pairing during the Initial Exchange because it is already clustered (it must leave / be removed before it can pair again). No PIN is minted. The `invite-node` result also carries a `reason` (`"already-clustered"`). Distinct from `failed` (a transport/protocol failure) so a UI can show "already paired / node is in a cluster" rather than "invite failed".

Validation: check every inbound envelope before processing. Reject malformed / unknown-`method` payloads on the inter-node interface (`400`); on the local interface, reply with a JSON-RPC `error` for any request carrying an `id`, and drop-and-log only lines that can't be correlated to an `id` — the full result-vs-error contract and code table are in §7.6. Assume the Broker supplies well-formed requests with epoch-millisecond timestamps.

**Outputs** — to the Broker over `stdout` / named pipe (JSON-RPC results to its requests, plus the `cluster:invite-received` notification and the proposed `nodes:changed` notification), or to peers via HTTP REST (pairing messages on the pairing channel, and removals over mTLS). `stdout` writes are serialized so frames never interleave; inter-node delivery is best-effort (bounded timeouts and retries).

## 7. API / Interface Contract

Two interfaces: a **local interface** toward the same-node UI Broker, and an **inter-node interface** toward Cluster Manager instances on other nodes.

### 7.0 Methods and notifications

All traffic is JSON-RPC 2.0. The local interface uses `stdin`/`stdout` (or a named pipe) and — unlike workload-manager — is **request/response** (the Broker needs results: member lists, invite IDs, invite status). Inter-node uses HTTP REST (see §7.2).

| Name | Direction | Kind | Summary |
|------|-----------|------|---------|
| `cluster:get-node-id` | Broker → CM | request | Return this node's own identity (`nodeUuid`, `nodeId`, `name`, cert fingerprint, current `clusterId`). |
| `cluster:set-identity` | Broker → CM | request | Supply/refresh the local *cluster* identity (`clusterId`, `clusterFriendlyName`) from `nvpair-node-settings`. |
| `cluster:create` | Broker → CM | request | Found a brand-new cluster of one (mints a unique `clusterId`, UUID v4); valid only when unclustered. Returns `{clusterId, clusterFriendlyName}`. |
| `nodes:get-initial` | Broker → CM | request | Returns the current membership set (`{nodes: ClusterNode[]}`). |
| `cluster:invite-node` | Broker → CM | request | Start a pairing; runs the Initial Exchange and returns `{inviteId, state, pin}` (PIN to display). |
| `cluster:invite-status` | Broker → CM | request | Read a pairing's current state by `inviteId`; returns the `Invite`. |
| `cluster:respond-to-invite` | Broker → CM | request | Submit the PIN (or decline) for a `pending-inbound` invite; returns the updated `Invite`. |
| `cluster:cancel-invite` | Broker → CM | request | Abort a `pending`-outbound invite (the inviter's counterpart to a joiner decline); tears down the pairing session, invalidates the PIN, and best-effort notifies the joiner. Returns the updated `Invite`. |
| `nodes:remove` | Broker → CM | request | Remove a *peer* from the cluster; returns `{nodeId, removed}`. Rejects removing self (use `cluster:leave`). |
| `cluster:leave` | Broker → CM | request | This node leaves its cluster: announces departure, drops all pins/members, resets to unclustered. Returns `{left}`. |
| `log/set-level` | Broker → CM | request | Standard `nvpair-shared/applog` level control (every subprocess accepts it). |
| `cluster:invite-received` | CM → Broker | notification | A pairing arrived; carries the `Invite` so the UI can prompt for the PIN. |
| `cluster:invite-canceled` | CM → Broker | notification | A still-pending inbound invite was canceled by its inviter or superseded by a newer invite from the same sender; carries the `Invite` (`state: "canceled"`) so the joiner's UI can dismiss its PIN prompt. |
| `cluster:invite-declined` | CM → Broker | notification | Inviter-side: the joiner declined; carries the `Invite` with `state:"declined"` so the UI can clear the PIN / abandon a throwaway solo cluster. |
| `cluster:invite-failed` | CM → Broker | notification | Inviter-side: the Completion Exchange failed (e.g. a wrong PIN); carries the `Invite` with `state:"failed"` (and `reason:"incorrect-pin"` on a wrong PIN) so the UI can show an "Incorrect PIN" error / abandon a throwaway solo cluster. |
| `cluster:identity-changed` | CM → Broker | notification | The local `clusterId` changed (e.g. the joiner adopted the inviter's on pairing); payload `{clusterId, clusterFriendlyName}` so the Broker can persist it to `nvpair-node-settings`. |
| `nodes:changed` _(proposed, §4)_ | CM → Broker | notification | Full membership snapshot pushed on every change. |

- **`cluster:get-node-id` (Broker → CM)**: pure read of *this* node's own identity. Params: none. Result `{nodeUuid, nodeId, name, certFingerprint, clusterId}`. The `nodeUuid` is the self-generated, persisted identity (§7.4); `nodeId`/`name` default to the OS hostname; `clusterId` is `""` until set, created, or adopted — a `""` here is the unclustered signal a UI can surface (e.g. render "not in a cluster" or offer an explicit `cluster:create`). The caller no longer needs to create before inviting: `cluster:invite-node` auto-founds when unclustered (§7.0/§7.2), so this read is informational rather than a required gate on the invite path. Always available (the identity is minted at startup), so the UI can render "this is me" immediately.
- **`cluster:set-identity` (Broker → CM)**: params `{clusterId?: string, clusterFriendlyName?: string}`. Supplies an **existing** cluster identity that `nvpair-node-settings` already holds (§8) so invites/pairings carry the right `clusterId`. The parent calls this at startup and whenever `nvpair-node-settings` reports a change. Result `{clusterId, clusterFriendlyName}` (the values now in effect). Does **not** touch `nodeUuid` or membership. An active durable admission is authoritative over an empty/stale settings reflection; clearing membership requires `cluster:leave` or authenticated removal. A normal startup restore reuses the durable admission epoch (migrating pre-v2 epoch-zero state to epoch 1), while a retired admission rejects stale non-empty settings even after process restart. Re-clustering must come from `cluster:create` or successful pairing.
- **`cluster:create` (Broker → CM)**: params `{clusterFriendlyName?: string}` (optional). Valid **only when this node is currently unclustered** (`clusterId == ""`); otherwise `-32004` with `data:{reason:"already-clustered"}` (§7.6). **Always mints a fresh, globally-unique `clusterId` — a random UUID v4** (`crypto/rand`), so every cluster gets a distinct id by construction (collision is effectively impossible; never reused). The caller does **not** supply the `clusterId` here — to *replay* a previously-known id (restore / normal startup) use `cluster:set-identity`, which is the path that takes an existing value from `nvpair-node-settings`. Sets `clusterFriendlyName` (defaulting to the OS hostname if omitted) and records this node as the founding member of a cluster of one. Result `{clusterId, clusterFriendlyName}`. Because the new cluster identity **originates here**, it also emits **`cluster:identity-changed`** so the Broker persists it to `nvpair-node-settings` — the same report-and-persist path as adopt-on-join (§7.0). `cluster:create` is the **explicit** founding path; a node may also found **implicitly** by simply inviting a peer while unclustered (`cluster:invite-node` auto-founds — §7.0/§7.2), so an explicit `create` is now only needed to name the cluster (`clusterFriendlyName`) before the first invite, not as a precondition for inviting.
- **`nodes:get-initial` (Broker → CM)**: pure read of the local membership set. Params: none. Result: `{nodes: ClusterNode[]}` — wrapped in an object (not a bare array) so summary fields can be added later without breaking clients. An empty list is a normal early state, not an error.
- **`cluster:invite-node` (Broker → CM)**: params `{address: string, port?: number, nodeId?: string}`. **If this node is unclustered, it first auto-founds a cluster of one** — the same mint-and-record founding as `cluster:create`, run in-process (so there is no check-then-create race across the wire), emitting `cluster:identity-changed` for the Broker to persist — so a caller can invite without a separate create step. It then records a `pending-outbound` invite and runs the EAP-NOOB Initial Exchange with the target (a few in-band round trips). Initial invite publication and session-generation capture are one commit under the teardown boundary: teardown either sees and clears the invite or wins before it is published; an in-flight Initial Exchange cannot reinsert it afterward. On success it derives the six-digit PIN and returns `{inviteId, state: "pending", pin}` — the inviter's UI displays the PIN. If the Initial Exchange can't complete (peer unreachable/incompatible), returns `state: "failed"` and no PIN. If the target **refuses because it is already clustered**, returns `{inviteId, state: "rejected", reason: "already-clustered"}` and no PIN (the target must leave / be removed before it can pair again). After a `pending` return, the inviter holds its EAP-NOOB Server alive (keyed by `inviteId`) and **serves** the joiner-driven Completion Exchange on its pairing endpoint once the remote user enters the PIN (§7.2); the invite flips to `paired` at that point, observable via `cluster:invite-status` or the `nodes:changed` push.
- **`cluster:invite-status` (Broker → CM)**: params `{inviteId: string}`. Returns the current `Invite` (`pin` omitted/null unless this is the inviter's still-pending session). Unknown `inviteId` → JSON-RPC error `-32001` (§7.6).
- **`cluster:respond-to-invite` (Broker → CM)**: params `{inviteId: string, accept: boolean, pin?: string}`. Valid only for a `pending-inbound` invite (else `-32001`/`-32002`, §7.6). On `accept` with a well-formed `pin`, feeds the PIN to EAP-NOOB and runs the Completion Exchange; on success pins the inviter's cert, records it as a `member`, and adopts the cluster identity (see §4 open question on who writes `cluster_id`). A wrong/expired PIN returns `state: "failed"` (with `reason: "incorrect-pin"` on a wrong PIN); a decline returns `state: "declined"`; a malformed (non-six-digit) `pin` is rejected with `-32602` before any attempt (§7.6). Returns the updated `Invite`.
- **`cluster:cancel-invite` (Broker → CM)**: params `{inviteId: string}`. The inviter's counterpart to a joiner decline: aborts a still-`pending` **outbound** invite that this node originated (from `cluster:invite-node`). Valid only for a `pending` outbound invite — an inbound invite (one we received) or a non-`pending` (already terminal) one returns `-32002`, and an unknown/evicted `inviteId` returns `-32001` (§7.6). It **evicts the inviter's EAP-NOOB Server session** (keyed by `inviteId`), which invalidates the PIN: any later joiner-driven Completion POST now hits the `409` unknown-invite branch (§7.2), so a remote user entering the correct PIN can no longer complete the join. It flips the invite to `canceled` and, best-effort, POSTs a `phase:"cancel"` envelope to the joiner's pairing endpoint so the joiner drops its pending-inbound invite and emits `cluster:invite-canceled` (below). The joiner notification is best-effort (bounded timeout); if the joiner is unreachable the inviter-side teardown still prevents the join. Returns the updated `Invite` (`pin` omitted).
- **`cluster:respond-to-invite` (Broker → CM)**: params `{inviteId: string, accept: boolean, pin?: string}`. Valid only for a `pending-inbound` invite (else `-32001`/`-32002`, §7.6). On `accept` with a well-formed `pin`, feeds the PIN to EAP-NOOB and runs the Completion Exchange; on success pins the inviter's cert, records it as a `member`, and adopts the cluster identity (see §4 open question on who writes `cluster_id`). A wrong PIN returns `state: "failed"` (with `reason: "incorrect-pin"`), clears the pending-inbound member row, and best-effort notifies the inviter (`phase:"fail"`) so the inviter's invite also reaches `failed` and emits `cluster:invite-failed`; a decline returns `state: "declined"` and best-effort notifies the inviter (`phase:"decline"`) so the inviter's invite also reaches `declined` and emits `cluster:invite-declined`; a malformed (non-six-digit) `pin` is rejected with `-32602` before any attempt (§7.6). Returns the updated `Invite`.
- **`cluster:invite-declined` (CM → Broker)**: emitted on the **inviter** when a joiner decline arrives over the pairing channel. Params: the `Invite` (with `state: "declined"`, no `pin`). If the cluster was invite-created and no confirmed peer / sibling pending outbound invite remains, the CM also leaves (empty `cluster:identity-changed` + empty `nodes:changed`). An intentional solo cluster is left intact.
- **`cluster:invite-failed` (CM → Broker)**: emitted on the **inviter** when a joiner-signaled completion failure (`phase:"fail"`) arrives over the pairing channel — today only a wrong PIN. Params: the `Invite` (with `state: "failed"`, no `pin`, and `reason: "incorrect-pin"` on a wrong PIN; empty reason for other causes). Tears down the EAP session (invalidating the PIN) and runs the same provenance-safe cleanup as decline (a throwaway invite-created solo cluster is left; an intentional solo cluster is left intact).
- **`cluster:invite-expired` (CM → Broker)**: emitted when a pending invite exceeds the invite TTL without accept/decline. On the **inviter** (outbound) it is the lost-decline / offline-joiner fallback and runs the same provenance-safe cleanup as decline; on the **receiver** (inbound) it fires when the local user never entered the PIN, dropping the tentative pending-inbound member + session so the UI can dismiss the prompt, and best-effort signaling the inviter over `phase:"expire"`. Params: the `Invite` with `state: "expired"`.
- **`nodes:remove` (Broker → CM)**: params `{nodeId: string}` (the logical id the UI holds; `nodeUuid` is also accepted). Resolves the member, durably writes an admission-targeted removal proof, then drops it locally, deletes its pin, and notifies the peer (keyed by `nodeUuid` on the wire). If authoritative proof cannot be persisted, the request fails and membership is left intact. Returns `{nodeId, removed: boolean}` (`removed: false` if the node wasn't a member — a no-op). Removing **self** is rejected with `-32602` (`data:{field:"nodeUuid"}`) — self-departure is `cluster:leave`, since a self-targeted peer removal would evict this node everywhere while leaving it locally still clustered.
- **`cluster:leave` (Broker → CM)**: params: none. The self-initiated counterpart to `nodes:remove`: this node unjoins its own cluster. It announces departure as a signed, admission-targeted self-removal proof pushed over roster reconcile (§7.7) to every reachable member — so they de-pin it and gossip the removal onward to members that are offline — then tears down all local cluster state: drops every pinned peer, clears the member/proof set and current admission, and resets the cluster identity to `""`. The node-global admission counter is retained, so a later join (including to the same cluster) has a strictly newer incarnation. Emits `cluster:identity-changed` (empty `clusterId`, so the Broker persists "unclustered" to `nvpair-node-settings`) and `nodes:changed` (empty set). Returns `{left: boolean}` (`left: false` when already unclustered — idempotent no-op). Departure is best-effort like removal: it takes effect locally even if some peers are unreachable, and the proof reconciles them when they return.
- **`cluster:invite-received` (CM → Broker)**: emitted once the inbound Initial Exchange completes. Params: the `Invite` (with `state: "pending"`, no `pin`). The Broker prompts the user to enter the PIN shown on the inviting node, then drives `cluster:respond-to-invite`.
- **`cluster:invite-canceled` (CM → Broker)**: emitted on the **joiner** when the inviter cancels a still-pending inbound invite (a `phase:"cancel"` arrives on the pairing channel, §7.2), or when a newer invite from the same authenticated sender supersedes it. Params: the `Invite` (now `state: "canceled"`, no `pin`). The joiner drops the old pending-inbound invite/session; on supersession it retains the sender's single pending member row for the replacement invite. The Broker uses this event to dismiss the old PIN prompt before handling the replacement `cluster:invite-received`. Idempotent — a duplicate or late cancel for an already-resolved invite is a no-op and emits nothing.
- **`cluster:identity-changed` (CM → Broker)**: emitted when the local `clusterId` originates **here** rather than from a `cluster:set-identity` call — i.e. in the two cases where this service creates or adopts a cluster identity: (a) `cluster:create` founding a new cluster, and (b) the joiner adopting the inviter's cluster on a successful pair (§4 working model). Params `{clusterId, clusterFriendlyName}`; the Broker persists it to `nvpair-node-settings`. Not emitted for changes the Broker itself drove via `cluster:set-identity` (those already came from node-settings).
- **`nodes:changed` (CM → Broker, proposed)**: full `{nodes: ClusterNode[]}` snapshot pushed whenever membership changes (accept, decline, removal, peer-initiated removal). Lets the Broker stay live without re-polling `nodes:get-initial`. Marked proposed in §4.

Example `cluster:invite-node` request and result (`stdin` → `stdout`):
```json
{
  "jsonrpc": "2.0",
  "id": 7,
  "method": "cluster:invite-node",
  "params": { "address": "192.168.1.22", "port": 14321, "nodeId": "NODE-B" }
}
```
```json
{
  "jsonrpc": "2.0",
  "id": 7,
  "result": { "inviteId": "inv-9f3a1c", "state": "pending", "pin": "402199" }
}
```
The inviter's UI shows `402199`; the user reads it to whoever is at node B.

Example `cluster:invite-received` notification (`stdout`) — no PIN and no cert; the UI uses this to prompt "enter the PIN shown on Lab desk A":
```json
{
  "jsonrpc": "2.0",
  "method": "cluster:invite-received",
  "params": {
    "inviteId": "inv-9f3a1c",
    "fromNodeId": "NODE-A",
    "fromNodeUuid": "b3d4f8a0-1c2e-4f6a-9b8c-0d1e2f3a4b5c",
    "fromNodeName": "Lab desk A",
    "toNodeId": "NODE-B",
    "clusterId": "cluster-xyz",
    "clusterFriendlyName": "Lab 3 desks",
    "pin": null,
    "state": "pending",
    "createdAt": 1716998400000,
    "respondedAt": null
  }
}
```

Example `cluster:respond-to-invite` request (joiner submits the PIN) and result:
```json
{
  "jsonrpc": "2.0",
  "id": 8,
  "method": "cluster:respond-to-invite",
  "params": { "inviteId": "inv-9f3a1c", "accept": true, "pin": "402199" }
}
```
```json
{
  "jsonrpc": "2.0",
  "id": 8,
  "result": {
    "inviteId": "inv-9f3a1c",
    "fromNodeId": "NODE-A",
    "fromNodeUuid": "b3d4f8a0-1c2e-4f6a-9b8c-0d1e2f3a4b5c",
    "fromNodeName": "Lab desk A",
    "toNodeId": "NODE-B",
    "clusterId": "cluster-xyz",
    "clusterFriendlyName": "Lab 3 desks",
    "pin": null,
    "state": "paired",
    "createdAt": 1716998400000,
    "respondedAt": 1716998460000
  }
}
```

### 7.1 Local Interface (UI Broker ↔ Cluster Manager)
- Transport: `stdin` / `stdout` (or a named pipe); JSON-RPC 2.0 **request/response** plus CM → Broker notifications.
- **Inbound** (`stdin`): the requests in §7.0 (`cluster:get-node-id`, `cluster:set-identity`, `cluster:create`, `nodes:get-initial`, `cluster:invite-node`, `cluster:invite-status`, `cluster:respond-to-invite`, `cluster:cancel-invite`, `nodes:remove`, `log/set-level`).
- **Outbound** (`stdout`): JSON-RPC results for each request; `cluster:invite-received` on each inbound invite; `cluster:invite-canceled` when the inviter cancels a pending inbound invite; `cluster:invite-declined` / `cluster:invite-failed` / `cluster:invite-expired` on the inviter when an outbound invite is declined / fails (e.g. wrong PIN) / times out, and `cluster:invite-expired` on the receiver when an unanswered inbound invite times out; `cluster:identity-changed` when a pairing changes the local `clusterId`; `nodes:changed` on each membership change (proposed).
- Local *cluster* identity (`cluster_id`, `cluster_friendly_name`) is supplied by the parent via the `cluster:set-identity` request, since `nvpair-node-settings` owns it (see §8); `nodeId`/`name` default to the OS hostname. The local *cryptographic* identity (node UUID + keypair + cert, §7.4) is **self-owned** — generated and persisted on first run, read back via `cluster:get-node-id`, never supplied by the parent.

### 7.2 Inter-node Interface (Cluster Manager ↔ Cluster Manager)
- Port `14321` (TCP), fixed on every node — all peers, whether resolved from discovery or Broker-supplied, are reached here. (`14318` node-info, `14319` errors, `14320` workload-manager are already taken.)
- Two transport modes on this port, distinguished by endpoint:
  - **Pairing channel** (`POST /v1/cluster/pairing`) — **plain HTTP**, used only during a pairing (before any pin exists). It carries opaque EAP-NOOB message blobs; authentication comes from the EAP-NOOB exchange + the user-entered PIN, not from TLS. (Even a confidential transport here wouldn't rescue the low-entropy PIN against an active MITM — see §4 debt.)
  - **Trusted endpoints** (everything else, e.g. `POST /v1/cluster/members/remove`) — **HTTPS over mTLS**: the server presents this node's leaf cert (§7.4) and is configured `RequireAnyClientCert`; a `VerifyPeerCertificate` hook reads the **claimed UUID from the presented client cert's subject `CN` (and URI SAN, §7.4)**, looks up that UUID in the trusted-node store, and requires a **byte-for-byte DER match** against the pinned cert (TLS min 1.2). An absent, unpinned, or mismatched cert → `403 Forbidden` (logged with the presented subject/fingerprint). These endpoints are only reachable by already-paired peers.

**Pairing channel — `POST /v1/cluster/pairing`** (JSON, `/v1` prefix). Each request/response body wraps exactly one EAP-NOOB message:
```json
{ "inviteId": "inv-9f3a1c", "phase": "initial|completion", "msg": "<base64 EAP-NOOB blob>" }
```
- `msg` is the opaque `Outcome.Send` blob from the `eap-noob` library, relayed verbatim; the receiver feeds it to its `Server`/`Peer` and returns that side's next `Outcome.Send` in the HTTP response. `msg` may be the **empty string** — that is the Completion *kickoff* (see "two HTTP drivers" below), the only case where a request carries no EAP blob.
- **No out-of-band material is ever transmitted on this channel.** There is no `noob`/`peerId`/`hoob` field: the only OOB secret is the six-digit PIN, and it is human-carried. Each side already holds everything else — the `PeerId` is assigned during the Initial Exchange (the joiner records it from the negotiation message), and `Hoob` is recomputed locally from the transcript, so neither needs to be relayed. The joiner builds its EAP-NOOB OOB input entirely from local state + the typed PIN (§7.4).
- **`PairingInfo` — the object each side puts in EAP-NOOB `ServerInfo` (inviter) / `PeerInfo` (joiner)** (identical schema both directions). It is folded into the `Hoob` fingerprint and the Completion `MACs`/`MACp` (`eap-noob/mac.go::noobArray`), so the cert and identity are cryptographically bound to the pairing (§7.4). Exact, fixed schema:
```json
{
  "v": 2,
  "nodeUuid": "b3d4f8a0-1c2e-4f6a-9b8c-0d1e2f3a4b5c",
  "nodeId": "NODE-A",
  "name": "Lab desk A",
  "clusterId": "cluster-xyz",
  "admissionEpoch": 4,
  "clusterFriendlyName": "Lab 3 desks",
  "addr": "192.168.1.10:14321",
  "cert": "-----BEGIN CERTIFICATE-----\nMIIB...AB\n-----END CERTIFICATE-----\n"
}
```
  - `cert` is the full self-signed leaf PEM (§7.4). `v` is the PairingInfo schema version (`2`); `admissionEpoch` is the durable incarnation being authenticated and later stored with the pin. `addr` is **required on the inviter's `ServerInfo`** (the `host:port` the joiner POSTs back to for the Completion Exchange) and optional on the joiner's `PeerInfo`. The joiner sends `clusterId: ""`/`clusterFriendlyName: ""` while pairing but carries the admission epoch it reserved for this attempt. A `PairingInfo` whose `cert` subject CN ≠ `nodeUuid` is rejected (`400`).
- **Two fixed roles, two HTTP drivers.** EAP-NOOB is a strictly server-initiated request/response protocol: the **Server always sends the request, the Peer always responds** (`eap-noob/server.go`, `peer.go`). The roles are fixed for the whole pairing — the **inviter is always the EAP-NOOB Server**, the **joiner is always the EAP-NOOB Peer**. But *which side makes the HTTP requests* deliberately differs by phase, so each phase is driven by the side that just had a reason to act and neither side polls. In **both** phases the receiver of an HTTP request feeds `msg` to its `Server`/`Peer` via `Receive(...)`, returns the resulting `Outcome.Send` as the HTTP response body, and the HTTP client loops, POSTing each new local `Outcome.Send`, **until an `Outcome` reports `Done` (terminal: `Success` ⇒ paired, or an error/`EAP-Failure` ⇒ failed).** Each HTTP request/response carries exactly one EAP message.
  - **Initial Exchange — the inviter (HTTP client) drives, and this lines up naturally with EAP** because the HTTP client is also the side that speaks first (the Server). Triggered by `cluster:invite-node`: the inviter calls `Server.Start()` (Type 1 Discovery) and **POSTs** it to the joiner's `POST /v1/cluster/pairing` with `phase:"initial"`; the joiner's handler validates that first message as Type 1 before reserving an admission epoch, runs `Peer.Receive(msg)`, and returns the peer response. The loop runs Discovery → Negotiation → KeyExchange until both `Outcome.State` reach `Waiting` (the library ends the Initial Exchange with `EAP-Failure` + `Done`, which is the *normal* Initial outcome, **not** an error — both sides are now awaiting OOB). The inviter then calls `Server.OOBOutputWith(noob)` with the PIN-derived `Noob` (§7.4) and **displays the six-digit PIN**; the joiner, on seeing its Peer reach `Waiting`, emits `cluster:invite-received`. Failed first messages delete their session, unpublished sessions expire on the normal invite TTL, and at most 64 joiner sessions may exist at once, bounding unauthenticated memory and durable-counter writes.
  - **Already-clustered reject.** Before creating a joiner session for the first `phase:"initial"` POST, the joiner checks whether it is already clustered (non-empty `clusterId`). If so it **refuses** rather than run the exchange: it replies `HTTP 409` with the pairing envelope `{rejected: true, reason: "already-clustered"}` (no EAP-NOOB message, no session, no PIN, no `cluster:invite-received`). The inviter recognizes this envelope (distinct from a plain `409` phase/session error) and returns `state: "rejected"` from `cluster:invite-node`. This is the authoritative guard against re-pairing a node that is already in a cluster; a node must **leave** (or be removed) first.
  - **Completion Exchange — the joiner (HTTP client) drives, which inverts the EAP request direction, so it needs an explicit kickoff.** Triggered by `cluster:respond-to-invite`: the joiner first feeds the PIN to its Peer (§7.4, moving it to `OOBReceived`), then runs this loop against the inviter's `POST /v1/cluster/pairing` with `phase:"completion"`:
    1. **Kickoff:** the joiner POSTs with **empty `msg`**. The inviter's handler looks up its still-alive `Server` for this `inviteId` and calls `Server.Start()` (a fresh Type 1 Discovery — the EAP reconnect), returning that blob.
    2. The joiner runs `Peer.Receive(blob)` → its Discovery response reports `PeerState = OOBReceived`, which it POSTs. The inviter's `Server.Receive` sees `peerState == OOBReceived` and advances to the Completion sub-flow (NoobID request → the joiner returns the `NoobId` it derived from the PIN → `MACs` → the joiner returns `MACp`).
    3. The inviter verifies `MACp` and EAP-NOOB computes `EAP-Success` (`Outcome.Done && Success`), but that frame is not returned yet. The inviter first commits the joiner's pin + admission + membership under the cluster/session teardown boundary. Only a successful local commit releases `EAP-Success`; a stale/abandoned session or changed cluster returns `409` and discards the success frame, so the joiner never reaches `Registered` when the inviter committed nothing. The inviter retains the successful session and cached EAP-Success until the joiner acknowledges its own durable commit, so a lost final HTTP response is retried idempotently. The joiner's `Peer.Receive` of an actually returned `EAP-Success` reaches `Registered`; it persists its provisional self/member/pin set, activates admission as the final durable write, and then posts `phase:"ack"` over the newly established mTLS trust. A post-success local failure posts the same authenticated `phase:"fail"` instead, causing the inviter to roll its provisional peer back; plain unauthenticated signals cannot alter an already-committed pairing.

    This is why `cluster:respond-to-invite` can complete **synchronously** with `state:"paired"` (§7.0): the joiner is the HTTP client, so it observes `Registered` in-line without waiting on any inviter-initiated request.
  - **Both sides keep their EAP-NOOB instance alive, keyed by `inviteId`, for the whole pairing** — across the Initial round-trips, the (possibly minutes-long) human PIN step, and the Completion round-trips. Each side needs a per-`inviteId` session map because the messages arrive as *separate* HTTP requests; the entry holds the `Server`/`Peer` with its ECDHE state, transcript (`s.in`/`p.in`), and (after the OOB step) the injected `Noob`. The only thing that flips per phase is **who is the HTTP client vs. server**: during Initial the joiner is the HTTP *server*, feeding its session-mapped `Peer` the three inviter-driven POSTs (Discovery → Negotiation → KeyExchange); during Completion the joiner is the HTTP *client*, driving that **same** session entry from the synchronous `respond-to-invite` handler. Entries are evicted on protocol failure, terminal state, or TTL expiry (§4); a process restart mid-pairing drops the session and the invite fails (the user re-invites).
  - **Decline signal — the joiner (HTTP client) notifies the inviter.** On `cluster:respond-to-invite` with `accept:false`, after local teardown the joiner POSTs `phase:"decline"` (empty `msg`) to the inviter's pairing endpoint, with a few short retries. The inviter flips its outbound invite to `declined`, deletes the Server session (so a late Completion hits 409), and emits `cluster:invite-declined`. Cleanup of the cluster itself uses provenance-safe cleanup: only a solo cluster **auto-founded by `cluster:invite-node` while unclustered** is dissolved, and only when **no sibling pending outbound invite** remains — an intentional solo cluster founded by explicit `cluster:create` is preserved, and a second still-pending invite keeps the cluster id stable for that sibling Completion. If the decline POST is lost, the inviter's invite TTL (default 5 minutes) expires the pending invite and runs the same provenance-safe cleanup. Invite-created provenance is persisted by cluster id; if the inviter restarts, its in-memory pairing sessions are gone and restoring that marked solo identity immediately cleans up the orphan.
  - **Fail signal — the joiner (HTTP client) notifies the inviter of a wrong PIN.** A wrong PIN is caught by the joiner (EAP Peer) first: it verifies the inviter's `MACs` in the Completion sub-flow and, on the mismatch, ends its side with an EAP-NOOB HMAC-verification failure **before** sending its own `MACp` — so the inviter's Completion handler is never re-entered and would otherwise be left on a stale `pending` invite. To mirror decline, after local teardown (flip to `failed` with `reason:"incorrect-pin"`, drop the pending-inbound member, evict the session) the joiner POSTs `phase:"fail"` (empty `msg`, carrying `reason`) to the inviter's pairing endpoint with a few short retries. The inviter flips its outbound invite to `failed` (stamping the reason), deletes the Server session (so a late Completion hits 409), emits `cluster:invite-failed`, and runs the same provenance-safe solo-cluster cleanup as decline. If the fail POST is lost, the inviter's invite TTL is the backstop.
  - **Expire signal — the receiver (HTTP client) notifies the inviter of an unanswered invite.** When the receiver's invite TTL (default 5 minutes) elapses with no local accept/decline, the receiver's reaper tears down its own inbound state (flip to `expired`, drop the pending-inbound member, evict the joiner session, emit `cluster:invite-expired` so the UI clears the PIN prompt) and then POSTs `phase:"expire"` (empty `msg`, correlated by `inviteId`) to the inviter's pairing endpoint with a few short retries. The inviter flips its outbound invite to `expired`, deletes the Server session (so a late Completion hits 409), emits `cluster:invite-expired`, and runs the same provenance-safe solo-cluster cleanup as decline. It reuses the shared decline/fail teardown path, so it is idempotent and answered `200` unconditionally. If the expire POST is lost — or the receiver is offline and never sends one — the inviter's own outbound invite TTL is the backstop, so both sides always return to a usable state.
  - On `Registered`, each side reads the peer's cert and admission epoch from the now-authenticated transcript (`PairingInfo` v2) and pins that exact incarnation. The inviter reaches `Registered` while serving the joiner's final Completion POST: it durably commits the joiner before answering with `EAP-Success`, flips its invite to `paired`, and waits for the joiner's commit acknowledgment before evicting the retry cache, surfacing the member, and fanning the roster out. Duplicate final Completion POSTs return the cached success frame. An unacknowledged cache entry expires without undoing two already-durable memberships.
  - **Cancel — the inviter (HTTP client) signals the joiner, out of band from EAP.** On `cluster:cancel-invite` the inviter first evicts its own `Server` session (which alone prevents any later Completion — a Completion POST for the now-unknown `inviteId` gets `409`), then POSTs `phase:"cancel"` (no `msg`, correlated by `inviteId`) to the joiner's pairing endpoint. The joiner drops its pending-inbound session/invite/member and emits `cluster:invite-canceled`. This carries no EAP-NOOB message — it is a best-effort teardown signal answered `200` unconditionally (idempotent; a `cancel` for an unknown or already-resolved `inviteId` is a silent no-op), so a lost or late cancel never wedges either side.
  - **Cancel and the joiner-driven Completion serialize on the per-`inviteId` session.** Both `cluster:cancel-invite` and the inviter's Completion handler take the session mutex and re-read the invite state **inside** the critical section, so exactly one wins and there is no torn outcome. If a Completion has already committed (pinned + recorded the member + flipped to `paired`), the cancel observes the terminal state under the lock and returns `-32002` rather than tearing a finished pairing down; if the cancel wins, the Completion handler sees the invite is no longer `pending` and returns `409` (never an `EAP-Success` blob), so the joiner's Completion fails cleanly instead of a one-sided join. The pending-state check is therefore authoritative only when held with the session lock — an unlocked check would race the pin/commit.
- Reaching the inviter for Completion: the joiner POSTs to the inviter's listening `host:port` taken from `addr` in the inviter's `ServerInfo` (§7.2 `PairingInfo`), falling back to the inviter's source address seen during the Initial Exchange. (The inviter was the HTTP *client* during Initial, so its TCP source port is not its listening port — hence `addr` is required.) The Initial/Completion traffic is still the un-pinned pairing channel, correlated by `inviteId`; only the post-commit `ack`/`fail` reuses this path over the newly established mTLS pins.
- Responses: `200 OK` (blob relayed, including the empty-`msg` kickoff); `400` (malformed / EAP-NOOB protocol error, e.g. MAC mismatch or unrecognized `NoobId`); `409` (unknown or expired `inviteId`, a `phase` that doesn't match the session's current state, or final EAP success whose local membership commit was refused after teardown).

**`POST /v1/cluster/members/remove`** (mTLS) — notify a peer it has been **removed** from the cluster (`nodes:remove`'s outbound half). Body: `{nodeUuid, proof}` where `nodeUuid` is the removing node (and must match the client-cert principal) and `proof` is the §7.7 admission-targeted removal proof. The recipient snapshots the exact client DER, cluster admission, and composition generation after initial pin authentication, reads the body, then revalidates all three and the proof under the teardown lock immediately before clearing state. Response `200 OK` (recipient tears down *all* local cluster state — pins, membership, proofs, current admission, and `clusterId` — becoming unclustered, same local outcome as `cluster:leave`); `409` (the request crossed a teardown/rejoin or other composition change); `403` (not a trusted peer, invalid proof, or proof for another admission); `400` (malformed). Self-initiated departure uses `cluster:leave`, not this endpoint.

**`POST /v1/cluster/roster`** (mTLS) — exchange and reconcile cluster rosters for trust fan-out (§7.7). Body: the sender's `Roster` (`{clusterId, members[], removalProofs[], tombstones[]}`; `tombstones` is a legacy compatibility mirror and is never admission-aware self-removal authority). The recipient merges it (transitively pinning every exact admission endorsed by a node it already trusts, durably persisting valid proofs before applying removals) and replies `200 OK` with **its own** `Roster`, which the sender merges in turn — so two peers fully converge in one round trip. If the caller is no longer pinned, `403` carries `{removalProofs:[...]}` when the recipient holds proof targeting that caller; this lets an offline victim verify its removal even though normal roster access is denied. `403` without such proof is only a rejection, not evidence that the caller was removed. `400` means malformed; rosters whose `clusterId` differs from the recipient's are ignored.

- Idempotency: pairing messages are correlated/deduplicated by `inviteId`; removals by `nodeUuid`. Re-pinning a cert already pinned for a UUID is a no-op; a *different* cert presented for an already-pinned UUID is rejected (see §12, key-rotation / impersonation).

Example pairing message (`POST /v1/cluster/pairing`; EAP-NOOB blob truncated):
```json
{
  "inviteId": "inv-9f3a1c",
  "phase": "initial",
  "msg": "eyJUeXBlIjoxLCJWZXJzIjpbMV0sLi4ufQ"
}
```
Response (the receiver's next blob): `200 OK` with the same envelope shape and the receiver's `Outcome.Send` in `msg`. The Completion kickoff is the same shape with `"phase":"completion"` and `"msg":""`.

Example removal (`POST /v1/cluster/members/remove`, mTLS):
```json
{
  "nodeUuid": "7a1c9e22-44b0-4d3f-8e10-aa55cc77dd99",
  "proof": {
    "tombstone": {
      "nodeUuid": "victim-uuid",
      "clusterId": "cluster-uuid",
      "admissionEpoch": 4,
      "by": "7a1c9e22-44b0-4d3f-8e10-aa55cc77dd99",
      "byAdmissionEpoch": 2,
      "removedAt": 1784678400000,
      "sigV2": "..."
    },
    "signerCertPem": "-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----\n",
    "signerFingerprint": "sha256:..."
  }
}
```
Response: `200 OK`.

### 7.3 Versioning
- URL path prefix (`/v1`) on the inter-node interface.
- JSON-RPC `method` names and HTTP endpoints are namespaced and additive. Wire objects gain optional fields, while security validation may fail closed on evidence that predates the admission-bound schema (for example a timestamp-only removal cannot authorize self-eviction).
- `ClusterNode` / `Invite` schema changes are backward compatible (new fields optional, unknown fields ignored); breaking changes require a new version (`/v2`).
- The trust model can evolve behind the same wire shape: the handshake already carries certs, so swapping pinned-leaf verification for a cluster-CA chain check (§4) needs no `/v2`.

### 7.4 Identity, certificates, and the trusted-node store

This subsection answers the two design questions head-on: **how nodes map to certs**, and **where cert material lives on disk**.

**Cryptographic identity = a stable node UUID, not the hostname.** The process owns its own identity: at startup it reads `<config-dir>/cluster/identity.json`, and **if the file is absent (first launch) it generates a fresh random UUID (v4), writes `identity.json` atomically (`0600`), and reuses it forever after** — no parent or external service supplies it. That UUID — not the hostname — is the node's security principal: it is the certificate's subject (`CN=<uuid>`), is also embedded as a URI SAN (`URI:urn:nvpair:node:<uuid>`), and is the key under which the trusted-node store pins the node's cert. Hostname is carried only as a display attribute (a DNS SAN and the `name` field) and is never trusted. The UUID (and the derived `nodeId`/`name`/cert fingerprint) is read back by any client via the **`cluster:get-node-id`** request (§7.0); `nodeId`/`name` default to the OS hostname (`os.Hostname()`), so the only identity input the process needs from a parent is the *cluster* identity (`cluster:set-identity`), never the node identity.

Why UUID over hostname (the alternatives in the original question):
- **Hostname is mutable** — a user renaming their machine would silently change identity and break every existing pin. A UUID is stable for the life of the install.
- **Hostname is not unique** — two machines on the LAN can share `DESKTOP-1`; UUID collisions are effectively impossible.
- **Hostname is trivially spoofable** and is already the (non-security) `nodeId` everywhere else (errors, workloads, discovery). Reusing it as the trust principal would conflate a display label with a credential.

Note the deliberate split: `ClusterNode.id` (hostname) stays the *logical/display* id for consistency with the rest of the system; `ClusterNode.nodeUuid` is the *cryptographic* id. Unifying the whole system onto the UUID is out of scope (open question, §4).

**Each node self-signs its own leaf** (no CA, §2/§4). The keypair (Ed25519 or P-256 — implementation choice) and a long-lived self-signed cert (proposed 1–2 years, §4) are generated on first run and reused across restarts.

**Trust is established by an EAP-NOOB pairing authenticated by a six-digit PIN.** The join handshake runs the in-tree `eap-noob` library (RFC 9140) over the pairing channel (§7.2):

1. **Bind the certs into the exchange.** The inviter (EAP-NOOB **Server**) puts its identity + **full cert PEM** in the EAP-NOOB `ServerInfo`; the joiner (**Peer**) puts the same in `PeerInfo`. Both objects are folded into the `Hoob` fingerprint and the Completion `MACs`/`MACp` (`eap-noob/mac.go::noobArray`), so a successful pairing proves both sides agree on each other's exact cert — no separate encrypted channel is needed to ship them (certs are public; only integrity matters).
2. **Initial Exchange** (in-band, machine-to-machine): ephemeral ECDH; both reach `Waiting`.
3. **OOB step = the PIN.** OOB direction is **server-to-peer** (`ServerConfig.Dirs = 2`, `PeerConfig.PreferDir = 2`): the inviter (Server) produces the OOB value and displays it as the PIN; the joiner (Peer) consumes it. The PIN is the **only** thing carried out of band, by the human. Nothing is relayed in-band for the OOB step — the joiner already holds the `PeerId` (assigned during the Initial Exchange) and recomputes `Hoob` locally from the transcript, so it builds its EAP-NOOB OOB input entirely from local state + the typed PIN (see "Library extension" below).
4. **Completion Exchange** (in-band): MAC verification; both reach `Registered`.
5. **Pin.** Each side reads the peer's cert out of the now-authenticated `PairingInfo` (§7.2) and pins it (full-cert match, keyed by the peer's UUID) into the trusted-node store. Optionally `Export(...)` a confirmation value to check both derived the same `Kz` before committing the pin.

The user entering the PIN is the out-of-band authorization — there is no silent auto-pin. Thereafter, every inter-node call must present a cert matching the pin for its claimed UUID.

**PIN ↔ `Noob` encoding (strongly defined — both nodes MUST agree byte-for-byte).**
- The **PIN** is exactly **six ASCII decimal digits** (`"000000".."999999"`, zero-padded, leading zeros significant). The inviter generates it with a uniform CSPRNG (`crypto/rand`) over `[0, 1000000)`.
- The EAP-NOOB **`Noob` is the fixed 16-byte big-endian encoding of that integer**: a 16-byte buffer, bytes `0..7` zero, bytes `8..15` = `binary.BigEndian.PutUint64(noob[8:], uint64(pinValue))`. (E.g. PIN `"402199"` → `pinValue = 402199` → `noob = 00 00 00 00 00 00 00 00 00 00 00 00 00 06 23 57`.) The base64url JSON form of these 16 bytes is what feeds `Hoob`/`NoobId`/MACs, so identical `pinValue` on both sides ⇒ identical transcript.
- The **joiner** parses the typed PIN: require exactly six digits, `pinValue = strconv.Atoi`, re-encode the same 16 bytes, and feed that `Noob` to its Peer (see below). A non-six-digit / non-numeric entry is rejected locally before EAP-NOOB (no wasted attempt).

**Library extension (the §4 delta, concretely).** The in-tree `eap-noob` library today random-generates a 16-byte `Noob` (`helpers.go::oobOutput`) and its OOB API exchanges the full `OOBMessage{PeerId, Noob, Hoob}` (`Server.OOBOutput`/`Peer.OOBInput`). Our flow human-carries only the `Noob`, so each side needs one small **additive** entry point that injects a caller-supplied `Noob` and derives the rest internally — no `OOBMessage` is built, transmitted, or parsed by the cluster-manager:
- **Inviter (Server):** `Server.OOBOutputWith(noob []byte) error` — set `s.noob` to the PIN-derived 16 bytes (instead of `rand.Read`), compute `s.noobID`, and stay in `Waiting`. (It needs only the side effect of registering the `Noob`; nothing is sent.) The CM calls this when it displays the PIN.
- **Joiner (Peer):** `Peer.OOBInputNoob(noob []byte) error` — set `p.noob`/`p.noobID` from the PIN-derived 16 bytes and move to `OOBReceived`, using the Peer's own already-known `PeerId` and its locally-recomputed `Hoob` (no externally-supplied `Hoob` to verify — for a human-carried low-entropy `Noob` there is no separate `Hoob` channel; the real transcript agreement is checked by the Completion `MACs`/`MACp`). The CM calls this on `cluster:respond-to-invite` with the typed PIN.

  (The existing `Server.OOBOutput()` / `Peer.OOBInput(OOBMessage)` stay untouched for full-`OOBMessage` callers; the two `*Noob`/`*With` variants are purely additive.)
- This single delta — caller-injected `Noob` on both roles — is the *only* change the cluster-manager requires in `eap-noob`. It vanishes when the OOB upgrades to a high-entropy pairing code (§4), which would carry a real `OOBMessage` and could use the original API.

Two integration notes:
- **EAP-NOOB association `Store`:** construct `NewServer`/`NewPeer` with a `nil` (in-memory) store. The library's association store is an *ephemeral* per-pairing artifact and is **not** the trusted-node store — the cluster-manager pins certs into its own `trusted/<uuid>.json` files (below) from the authenticated `PairingInfo`, independent of whatever `eap-noob` keeps internally.
- **`ServerInfo`/`PeerInfo` size:** RFC 9140 suggests ≤ 500 bytes for these objects; a full leaf PEM exceeds that (more so for P-256 than Ed25519). The in-tree library does **not** enforce the cap and the transport is HTTP (no EAP/RADIUS fragmentation), so embedding the PEM works today — this is a deliberate, documented deviation. If the library is ever hardened to enforce the RFC limit, switch the embedded cert to DER/base64 or raise the cap; prefer **Ed25519 leaves** to stay smallest.

> **⚠️ The six-digit PIN is temporary, low-entropy security debt (full analysis in §4):** it guards against passive eavesdroppers and accidental joins but not an active MITM, and must be replaced by a high-entropy pairing code before cluster trust is relied on. That upgrade changes only the OOB encoding and the library's `Noob` source — the EAP-NOOB transcript and pin format are unchanged.

**Revocation = removal.** `nodes:remove` deletes the peer's pin file; there is no CRL/OCSP. A removed node's subsequent handshakes fail closed (`403`).

**On-disk layout** — under the same per-user config directory the other subprocesses use (the dir that holds `settings.json`, `manual-nodes.json`, `tls-settings.json`), in a dedicated `cluster/` subtree. Directories `0700`, key/pin files `0600`, matching `nvpair-node-settings`:

```
<config-dir>/cluster/
  identity.json              # { "node_uuid": "...", "created_at": <epoch ms> }
  node.key                   # this node's private key (PEM, 0600) — never leaves the host
  node.crt                   # this node's self-signed leaf (PEM)
  trusted/                   # the trusted-node store: ONE file per pinned peer (dir 0700)
    <peerNodeUuid>.json      # 0600; the pin for one peer (schema below)
```

**The trusted-node store is a directory, one file per peer, named `<peerNodeUuid>.json`.** The filename *is* the store key (the peer's `nodeUuid` — a v4 UUID, so always a filesystem-safe name on every platform; this is another payoff of keying trust on the UUID rather than a hostname, which would not be a safe filename). Each file holds one pin:
```json
{
  "nodeUuid": "7a1c9e22-44b0-4d3f-8e10-aa55cc77dd99",
  "nodeId": "NODE-B",
  "name": "Lab desk B",
  "clusterId": "cluster-xyz",
  "certPem": "-----BEGIN CERTIFICATE-----\n...\n-----END CERTIFICATE-----\n",
  "certFingerprint": "sha256:...",
  "pinnedAt": 1716998460000
}
```

Operations on the store map directly to single-file filesystem actions, so an edit to one peer can never corrupt or lose the others:
- **Pin** (on pairing success): write `trusted/<uuid>.json.tmp` then atomically rename to `trusted/<uuid>.json` (`0600`).
- **Remove** (`nodes:remove`): `os.Remove("trusted/<uuid>.json")` — a single atomic delete; a missing file is treated as already-removed (idempotent success).
- **Load** (startup): read `trusted/*.json` into the in-memory `uuid → pin` map, **skipping** `*.tmp` and any file that doesn't parse. On load, verify the file's inner `nodeUuid` (and the `certPem` subject `CN`) matches the filename `<uuid>`; reject/skip-and-log a mismatch so a renamed or tampered file cannot masquerade as another UUID.
- **Re-pin guard** (§7.2): re-pinning the *same* cert for an existing `<uuid>.json` is a no-op; a *different* cert for an already-present `<uuid>.json` is rejected, not overwritten (explicit re-pin/re-invite required — §12 key rotation).

Writes are serialized (membership changes are rare and human-driven), so readers tolerate seeing a just-added or just-removed file without a global lock; there is no single-file consistent snapshot, which is acceptable for this low-churn store.

`certFingerprint` is **`"sha256:" + lowercase-hex(SHA-256(cert DER))`** (the DER bytes, not the PEM text) — used as a log/display key only; trust decisions always use the full-DER pin match (§7.2), never the fingerprint alone. The same value is returned by `cluster:get-node-id`.

The exact `<config-dir>` is the per-user data dir shared by every component: `%LocalAppData%\Nvidia Corporation\Personal AI Router` on Windows (local, non-roaming), `$XDG_CONFIG_HOME`/`~/.config/Nvidia Corporation/Personal AI Router` on Linux, `~/Library/Application Support/Nvidia Corporation/Personal AI Router` on macOS, with a portable next-to-exe fallback for dev builds, so cluster identity sits beside the other persisted state rather than inventing a new location.

**Reuse by other services.** This keypair and the `trusted/` store are the shared trust fabric. `nvpair-workload-manager` and `nvpair-errors` peer-sync are intended to load the same `node.crt`/`node.key` for their client/server TLS and verify peers against the same `trusted/<uuid>.json` pins. The handoff mechanism (shared paths passed by the Broker vs. a small local query API) is an open question (§4); what this spec fixes is the **format and location** so consumers have a stable contract.

### 7.5 Discovery (peer address resolution)

The Cluster Manager runs no responder or browser of its own. `nvpair-node-scanner` is the single discovery daemon on the node: the Broker registers this service's `cl` service key and listening port with that daemon, the daemon carries the key on this node's one consolidated `_nvpair-node._tcp` record, and the Cluster Manager learns peers by subscribing upward — `discovery:subscribe` filtered to the `cl` service — and consuming the `discovery:nodes` snapshots the Broker relays back.

- **Registered port**: the inter-node listener, **`14321`** (one above `nvpair-workload-manager`'s `14320`; the pairing channel and the mTLS endpoints share it, distinguished by path and by whether the TLS handshake presents a client cert). It reaches the record as the `cl` service key.
- **The record layout is the daemon's contract, not this service's.** Service type, instance name, SRV port, and TXT keys are owned by the discovery daemon and the shared `nvpair-shared/noderec` wire format; this service reads the directory entries it is handed and never parses a record itself. The record's SRV port is a fixed, non-authoritative constant, so a consumer takes each service's port from TXT and never from SRV. The instance name is the OS hostname (the logical `nodeId`), surfaced on a directory entry as its `name` — display only, never trusted.
- **TXT keys this service depends on** (all carried on the one consolidated record, alongside the other services' port keys):

| Key | Value | Why |
|-----|-------|-----|
| `uuid` | this node's `nodeUuid` (§7.4) | The **node id in the TXT record** — lets a consumer map a discovered node to its stable cryptographic principal *before* pairing, and resolve an invite target given only a UUID. Reaches a directory entry as `hostUuid`. |
| `cluster-uuid` | current `clusterId`; **omitted entirely** while the node is unclustered | Lets the UI show which discovered nodes are already in a cluster (and which one) without a round-trip, and selects which pin a cluster-scoped dial should present. Reaches a directory entry as `clusterUuid`. |
| `cl` | this node's inter-node listener port | The per-service port key for the Cluster Manager. A record without it advertises no Cluster Manager, and the entry is filtered out of a `cl` subscription. |
| `ip` | this node's canonical dialable LAN address | The address the resolver hands back for a peer. |
| `v` | the record's schema version (`1`) | Disambiguates the record format across wire changes; diagnostics and tests, not mixed-version negotiation. |

- **Resolution** is best-effort and used only to build a `nodeUuid`/`nodeId` → `host:port` map, so an invite targeting a UUID/hostname can be resolved to an address, so a transitively-learned member whose address was never observed can still be reached (§7.7), and so the joiner can corroborate the inviter's `addr` from `PairingInfo` (§7.2). Each snapshot carries the full filtered set and **replaces** the map wholesale rather than applying per-node deltas, so a node absent from the next snapshot stops resolving. The Cluster Manager does **not** expose its own discovery RPC — node selection for invites is the Broker's job (it already owns the discovery relay and manual nodes); a Broker-supplied address always wins over a resolved one, so a node reachable only by manual entry is still invitable. Where the daemon reports nothing (no multicast, or a parent that is not relay-aware), resolution degrades gracefully to Broker-supplied addresses.

### 7.6 Local-interface errors and the result-vs-error contract

Every local call is JSON-RPC 2.0. The contract draws a hard line between a *negative domain outcome* and a *protocol error* so the Broker can branch deterministically: `result` vs `error` **is** the "did it pair / apply?" vs "could I even process this?" signal. This subsection is normative for the Broker integration.

**Rule 1 — negative outcomes are successful `result`s, never errors.** An operation that ran correctly but ended unfavorably returns a normal `result` carrying a terminal `state`/flag:
- `cluster:invite-node` to an unreachable or incompatible target → `result {inviteId, state:"failed"}` (no `pin`).
- `cluster:respond-to-invite` with a well-formed but **wrong/expired** PIN → `result {…, state:"failed"}`; a decline (`accept:false`) → `result {…, state:"declined"}`; a peer that drops mid-Completion → `result {…, state:"failed"}`.
- `nodes:remove` on a node that isn't a member → `result {nodeId, removed:false}`.
- A peer-notify that fails after a local removal already applied → still `result {nodeId, removed:true}` (best-effort, §12).

A negative outcome is therefore **success at the JSON-RPC layer**; the Broker reads `state`/`removed`, not the presence of an `error`.

**Rule 2 — protocol errors use the JSON-RPC `error` object** `{code, message, data?}`, where optional `data` carries structured detail (`{field}` for bad params; `{inviteId, state}` for a bad transition). Codes:

| Code | Meaning | Raised when |
|------|---------|-------------|
| `-32700` | Parse error | the stdin line is not valid JSON (see Rule 3). |
| `-32600` | Invalid Request | valid JSON, but not a JSON-RPC 2.0 request object. |
| `-32601` | Method not found | unknown `method` name. |
| `-32602` | Invalid params | missing/ill-typed/structurally-invalid params: empty `address`; `port` outside `1..65535`; absent `inviteId`; absent `accept`; `accept:true` with a `pin` that fails `^[0-9]{6}$`; an unknown `log/set-level` level; `nodes:remove` with neither `nodeId` nor `nodeUuid`; wrong-typed `cluster:set-identity` fields. |
| `-32603` | Internal error | an unexpected local failure (e.g. the trusted-store write failed while pinning, identity unreadable). The pairing/membership change did **not** take effect. |
| `-32001` | Unknown invite | `inviteId` not found or already evicted — `cluster:invite-status`, `cluster:respond-to-invite`, `cluster:cancel-invite`. |
| `-32002` | Invalid invite state | the invite exists but the call isn't valid for it — `respond-to-invite` on an **outbound** invite (one we sent, not received), `cancel-invite` on an **inbound** invite (one we received, not sent), or either on an invite whose `InviteState` is already terminal (`paired`/`declined`/`canceled`/`expired`/`failed`) rather than `pending`. `data` = `{inviteId, state}`. A second `respond-to-invite` for an already-`paired` invite lands here (the first already transitioned it). |
| `-32004` | Precondition not met | a call's required precondition isn't satisfied: `cluster:create` while the node is already clustered (`data:{reason:"already-clustered"}`). (`cluster:invite-node` no longer returns `-32004`: an unclustered invite auto-founds a cluster of one rather than rejecting — §7.0/§7.2.) |

App-specific codes live in the JSON-RPC server-error range (`-32000…-32099`); new ones are additive (§7.3). The split for PINs is deliberate: a **malformed** PIN (not six digits) is `-32602` (structurally invalid, no EAP-NOOB attempt is spent); a **well-formed-but-wrong** PIN is a `state:"failed"` *result* (an attempt was made and the peer rejected it). Likewise an unreachable peer is a `failed` *result*, not an error — the request itself was processed.

**Rule 3 — reply unless the message can't be correlated.** If a stdin line is unparseable JSON or lacks a usable `id`, the CM logs and drops it (there is no `id` to address a response to). Any well-formed request object carrying an `id` **always** receives exactly one matching response (`result` or `error`), so the Broker never blocks on a dangling `id`. (This refines §6's "drop-and-log": drop only the uncorrelatable lines; everything with an `id` is answered.)

**Per-method error surface** (anything not listed returns a `result`):
- `cluster:get-node-id` — none in normal operation (identity is minted at startup or the process exits, §12); only `-32603` if local state is unreadable.
- `cluster:set-identity` — `-32602` (bad field types). Otherwise `result`.
- `cluster:create` — `-32602` (bad field types); `-32004` (already clustered). Otherwise `result`.
- `nodes:get-initial` — none (empty list is a normal `result`).
- `cluster:invite-node` — `-32602` (bad `address`/`port`/`nodeId`); `-32004` (invited while unclustered — `cluster:create` or accept an invite first); `-32603` (internal). Peer problems → `failed` *result*; an already-clustered target refusing the pairing → `rejected` *result* (with `reason: "already-clustered"`), not an error.
- `cluster:invite-status` — `-32602` (absent `inviteId`); `-32001` (unknown `inviteId`).
- `cluster:respond-to-invite` — `-32602` (absent `inviteId`/`accept`, or malformed `pin`); `-32001` (unknown `inviteId`); `-32002` (invite is outbound, or its `InviteState` is already terminal rather than `pending`); `-32603` (internal, e.g. pin-write failure). Wrong PIN / decline / peer-drop → `failed`/`declined` *result*.
- `cluster:cancel-invite` — `-32602` (absent `inviteId`); `-32001` (unknown/evicted `inviteId`); `-32002` (invite is inbound, or its `InviteState` is already terminal rather than `pending`). A joiner that's unreachable for the best-effort cancel notification is **not** an error — the inviter-side teardown succeeded, so it's a normal `canceled` *result*.
- `nodes:remove` — `-32602` (neither id provided, or the resolved node is **self** — use `cluster:leave`); `-32603` (internal). Non-member → `removed:false` *result*; peer-notify failure → `removed:true` *result*.
- `cluster:leave` — none (already-unclustered is a normal `left:false` *result*, not an error).
- `log/set-level` — `-32602` (unknown level).

**Direction, concurrency, readiness.**
- **Notifications never carry errors.** `cluster:invite-received`, `cluster:invite-canceled`, `cluster:invite-declined`, `cluster:identity-changed`, and `nodes:changed` are JSON-RPC *notifications* (no `id`, no reply). The CM never sends a *request* to the Broker, so the Broker needs no inbound-request handler — the local channel is Broker-request → CM-response, plus these CM→Broker notifications.
- **Concurrency.** `cluster:invite-node` and `cluster:respond-to-invite` block on multi-round-trip network I/O (§7.2) for up to the pairing timeout (§10/§12 — bounded, default a few seconds). The CM **processes local requests concurrently** so a slow pairing never stalls reads or other calls; responses are matched by `id` and **may return out of order** (permitted by JSON-RPC). `stdout` frames are still serialized so they never interleave (§5). The Broker's per-request timeout must therefore exceed the pairing timeout for these two methods.
- **Readiness.** The CM starts reading stdin immediately; a first successful `cluster:get-node-id` is the natural readiness probe (the node identity exists from startup, §7.4). Results and notifications are eventually-consistent — a `nodes:changed` may arrive just before or after the `result` of the call that caused it — so the Broker should reconcile by `inviteId`/`nodeUuid`, not by ordering.

### 7.7 Cluster trust fan-out (roster reconciliation)

Point-to-point pairing alone leaves trust un-propagated: if A pairs B and then A pairs C, B and C have each pinned only A and cannot mTLS each other. The **roster reconciliation** layer closes this so every member transitively trusts every other member, while keeping the human-entered PIN as the sole root of trust.

- **Admission incarnation (the ordering primitive).** Every node owns a durable, node-global monotonic admission counter (`admission.json`). Founding or joining consumes the next epoch and records `{clusterId, epoch}`; restart reuses that active pair and teardown clears only the active pair, never the counter. `PairingInfo` v2 carries the epoch inside the EAP-NOOB-authenticated transcript, and the exact epoch is persisted on `ClusterNode`, `TrustedPin`, and `RosterEntry`. Pre-v2 epoch-zero pins/members (including records with an absent cluster id) map deterministically to epoch 1 in the active cluster and receive a local v2 endorsement. Therefore "newer admission" is a signed/pinned fact rather than a wall-clock comparison: rejoining the same cluster after removal necessarily has a larger epoch, while an old proof remains scoped to the incarnation it removed.
- **Endorsement (the trust primitive).** On every successful pairing, each side signs an **endorsement** of the other's exact certificate and admission with its own Ed25519 leaf key: a v2 payload of `(introduced nodeUuid, cert fingerprint, clusterId, introduced admission epoch, endorser admission epoch, issuedAt)` under a domain-separation tag, stored alongside that peer's pin. An endorsement is the cryptographic statement "I, this exact admission of a trusted member, vouch that this exact cert admission was authenticated by pairing." Because it is end-to-end signed, it stays verifiable across gossip hops (unlike mTLS, which authenticates only one hop). The legacy v1 signature remains serialized for compatibility but cannot authorize admission-aware removal.
- **Roster.** A node's roster is `{clusterId, members[], removalProofs[], tombstones[]}`: a self entry plus one entry per pinned peer (carrying that peer's stored endorsements, admission epoch, and last-known reachable address), durable removal-proof envelopes, and a bare-tombstone compatibility mirror.
- **Reconcile (the merge rule).** On receiving a roster (over the mTLS `POST /v1/cluster/roster` endpoint, so the sender is already a trusted member), a node **transitively pins** every entry whose exact cert admission is endorsed by a node/admission it *already* trusts, iterating to a fixpoint so a freshly-pinned node can in turn vouch for the nodes it endorsed. The endorsement graph it walks **is** the human-authorized pairing graph. The merge is bounded: an entry endorsed only by a node it does not trust is rejected, an endorser or introduced admission mismatch is rejected, and an entry whose cert does not match its claimed fingerprint is rejected. A newer authenticated admission may replace an older same-cert admission; stale gossip can never downgrade it.
- **Removal fan-out (durable proofs).** A removal first persists a `RemovalProof`: an admission-bound tombstone `(removed nodeUuid, clusterId, removed admission epoch, remover nodeUuid, remover admission epoch, removedAt, signature)`, the original remover certificate/fingerprint, and signed endorsements of that remover from relays. Persistence is authoritative and precedes de-pinning: if it fails, removal fails with membership intact; restart replays a persisted proof against a stale pin/member if the process stopped between those steps. A recipient verifies the original tombstone signature and either the exact directly-pinned remover admission or an endorsement from an exact admission it still trusts. This lets an older member relay proof from a remover the offline victim never pinned. Proof retention has **no TTL**: it remains durable and gossiped until a strictly newer authenticated admission of the removed node has been durably pinned, at which point that older proof is superseded and deleted. `removedAt` is audit metadata only, never ordering authority.
- **Convergence triggers.** Reconciles are pushed to all members on **pair success** (the inviter fans the new member out; the joiner reconciles with the inviter to learn the rest of the cluster from its reply) and on **removal**, with a periodic **heartbeat** (~30 s) as the catch-up backstop for members that were offline during a change. Peer addresses come from the address recorded at pairing (the observed source IP plus the listening port the joiner advertises in its PairingInfo), falling back to the discovery resolver (§7.5) for a member whose address was never observed.
- **Self-removal on unanimous rejection plus proof (offline-removed backstop).** A node removed while offline misses the direct notify and every peer has already de-pinned it, so normal roster reconciles receive `403`. A unanimous `403` is **necessary but not sufficient**: a peer that merely left also drops all pins and returns the same status. A rejecting peer therefore includes any durable proof targeting the caller in the `403` body. The caller self-removes only when every current peer rejects, no peer is unreachable, and at least one response carries a fully verified proof for this node's **exact current cluster admission** (direct remover or relay-endorsed unknown remover). A bare `403`, bare/legacy tombstone, wrong cluster/epoch, unknown signer, stale endorser admission, or tampered proof never authorizes self-teardown; instead, after the same cluster/generation recheck, a bare authenticated rejector is de-pinned and removed as a peer that departed or lost its cluster trust. This lets a surviving node converge to a solo roster after its only offline peer voluntarily leaves. The network verdict is revalidated against cluster identity and the in-process composition generation under `rosterMu`; a concurrent different- or same-cluster rejoin invalidates it. Teardown, invite/session reset, initial invite publication, pairing commit, and final EAP-Success release all share that composition boundary, so an in-flight exchange cannot republish state or produce a one-sided Registered pairing after teardown.
- **Threat model.** Trust is now **transitive**: a compromised member can endorse certs that every other member will then pin, so the blast radius of one bad node is the whole cluster rather than just its direct pairings. This is a deliberate trade-off (re-pairing every pair by hand does not scale) layered on the same "pair only on a network you control" debt as the PIN. Hardening options include a cluster CA with revocation, endorsement quorums, and audit logging.

## 8. Dependencies
- **Upstream**: a supervising parent — in practice `nvpair-ui-broker`, which spawns the Cluster Manager, drives the §7.0 requests, and consumes its notifications. The process is **supervisor-agnostic**: it runs under any parent that speaks the §7.0 JSON-RPC contract over stdio (or `--ipc`). Since the node identity is self-owned (§7.4), the only thing a parent must supply is the *cluster* identity via `cluster:set-identity` (from `nvpair-node-settings`); without it the node stays unclustered until it pairs.
- **Downstream**: local UI Broker (consumes results, `cluster:invite-received`, and `nodes:changed`); peer Cluster Manager instances (run pairings over the pairing channel and receive removals over mTLS).
- **Library**: the in-tree `eap-noob` Go module (RFC 9140) drives the PIN-authenticated key exchange; it needs the one small additive extension noted in §4/§7.4 (caller-injected `Noob` on each role, `Server.OOBOutputWith` / `Peer.OOBInputNoob`).
- **External / sibling**:
  - **`nvpair-node-settings`** — owns `cluster_id` / `cluster_friendly_name` / `cluster_auto_sync`. The Cluster Manager reads the local identity (through the Broker) and reports the outcome of a join; it does not write settings directly (open question §4).
  - **`nvpair-node-scanner` discovery daemon, via the Broker's relay** — advertises this node's `cl` port on the node's single consolidated record and supplies the `discovery:nodes` snapshots the peer resolver is rebuilt from (§7.5). Its snapshots are merged with the addresses the Broker supplies directly (e.g. from `discovery:get-nodes` / manual nodes), which take precedence, so a node reachable only by manual entry can still be invited.
  - **Multicast-capable network** — required by the discovery daemon's browse, not by this service directly; not required at all when the Broker supplies the target address.
  - **Per-user config directory** — the on-disk home for this node's keypair/cert and the trusted-node store (§7.4), shared with `nvpair-node-settings` and the TLS settings file.
- **Consumers of the trust fabric** (inverse dependency): `nvpair-workload-manager` and `nvpair-errors` peer-sync are intended to authenticate against the keypair + trusted-node store this service owns; they depend on its on-disk format/location (§7.4), not the reverse (handoff open, §4).

## 9. Data Ownership
- **Owned**: the local membership set (members + pending invites), the in-flight invite table, **this node's cryptographic identity (UUID + keypair + self-signed cert), and the trusted-node store** (UUID → pinned peer cert). **Source of truth for cluster membership and for cluster trust from this node's perspective** — distinct from `nvpair-node-settings`, which owns the `cluster_id` value but neither the member list nor any key material.
- **Source of truth**: yes, for *membership* (which nodes this node considers cluster peers) and for *trust* (which peer certs are pinned). No, for *cluster identity* (`cluster_id`) — that's `nvpair-node-settings`.
- **Storage**: the identity (`node.key`/`node.crt`/`identity.json`), admission counter/current-or-retired incarnation (`admission.json`), trusted-node store (`trusted/<uuid>.json`), confirmed member set (`members.json`), admission-targeted proof set (`removal-proofs.json`), and teardown intent (`teardown.pending`) are **durable**. Writes are atomic. Create/join commits persist a provisional nonzero self member, peer membership, and peer pin before activating `admission.json`; startup rolls back that recognizable provisional state if a crash occurs before activation. Pairing and roster fan-out persist membership before granting the peer's mTLS pin, and startup removes any orphan pin with no matching durable member. Removal proof persistence occurs under the live composition boundary before de-pinning, and proof replay completes an interrupted peer removal on restart. Teardown writes its intent first; while `teardown.pending` is set, old pins cannot authenticate and no new admission can activate. Startup completes every remaining cleanup step before serving, and any replay/cleanup persistence error aborts startup rather than serving partial authorization. Proofs are retained until a newer authenticated admission supersedes them, not deleted by elapsed time. The in-flight invite/session table is transient and bounded. Private keys never leave the host; only the public cert is transmitted.

## 10. Design Constraints
- **Performance**: very low event volume — membership changes are human-driven (invites, accepts, removals), not telemetry. No latency SLA; the invite flow is inherently async (gated on a remote user).
- **Scalability**: ~dozen-node cluster. Membership and invite tables are small; each change touches at most one peer.
- **Reliability**: best-effort inter-node delivery with bounded timeouts/retries; a local membership change always applies even if the peer notification fails, and re-delivery reconciles idempotently. Serialized `stdout`. No replay — divergent views reconcile via later actions (or a future heartbeat, §4).
- **Security**: trusted inter-node traffic is mTLS over HTTPS, authenticated against the trusted-node store (§7.4); unpinned/absent client certs are rejected (`403`). Pins are bootstrapped by an EAP-NOOB pairing authenticated by a user-carried PIN (no central CA), keyed by stable node UUID; removal de-pins (the only revocation path). Private keys are stored `0600` and never transmitted; the PIN is never sent on the wire or logged. Payloads carry node IDs/UUIDs, display names, addresses, certs (public), and the `cluster_id` — no inference data or PII. `nodeId` / `nodeUuid` / `inviteId` / `clusterId` are opaque system-generated identifiers. **Known, accepted, temporary weakness (§4): the six-digit pairing PIN is low-entropy and does not resist an active MITM** — it must be upgraded to a high-entropy OOB pairing code before cluster trust is relied on. Other residual exposure: no cert revocation beyond removal; monitor cert expiry.
- **Compliance**: membership and invite payloads must contain no PII (display names are user-chosen labels, not identities).

## 11. Assumptions
- A parent supervises the process (in practice `nvpair-ui-broker`, but supervisor-agnostic — §8); on `stdin` EOF the manager shuts down cleanly, with no reconnect or buffering.
- Cluster identity (`cluster_id` / `cluster_friendly_name`) is owned by `nvpair-node-settings` and arrives via `cluster:set-identity`; the *node* identity (UUID/keypair/cert) is self-owned, generated once on first run, and stable for the life of the install (§7.4).
- Invites are always interactive — no auto-accept path; entering the PIN is the out-of-band authorization, so pinning is never silent (§4).
- Small cluster (~dozen nodes), low human-driven change rate — one invite at a time, no bulk join or historical replay.
- A node leaves the unclustered state in exactly two ways: founding a cluster (explicitly via `cluster:create`, or implicitly as the auto-found side effect of the first `cluster:invite-node` while unclustered — §7.0/§7.2), or adopting the inviter's cluster on accept (the current "join the inviter's cluster" working model — §4).
- The per-user config dir is writable and private to the user.

## 12. Failure Modes and Mitigations
- **Peer unreachable during the Initial Exchange** (down, partitioned, suspended): the pairing can't start. → Bounded timeout/retries; the outbound invite ends `failed` and `cluster:invite-status` reports it. No PIN is issued and no membership is created.
- **Peer unreachable during the Completion Exchange or a removal**: the two sides may disagree about membership. → A wrong PIN or unfinished completion simply leaves both unpaired (fail closed). For removal, the local change applies regardless; re-delivery on reconnect reconciles idempotently (and a future membership heartbeat, §4, would close the gap). Removal is authoritative locally even if the peer never hears it.
- **Wrong / mistyped PIN (surfaces as a Completion `MACs`/`MACp` mismatch or an unrecognized `NoobId`)**: a typo or a tampered transcript. → EAP-NOOB completion fails; the invite ends `failed` with no pin and no membership. The user can retry by re-inviting (a fresh PIN). A few failed attempts should expire the session (§4) to bound online guessing.
- **Active MITM on the pairing channel** (the accepted §4 weakness): because the PIN is only ~20 bits and `NoobId`/MACs are in-band, a determined on-path attacker can recover the PIN and substitute its cert. → **Not fully mitigated today** — documented debt; partial mitigations are session expiry and limited PIN attempts. Real fix is the high-entropy pairing code (§4).
- **Duplicate / retried pairing or removal message**: could create a second pending invite or double-apply a change. → Deduplicate by `inviteId` (pairing) and `nodeUuid` (removals); re-delivery is a no-op that returns the same `2xx`.
- **Unwanted / spoofed removal on the LAN**: a stranger could send a removal. → `members/remove` requires a pinned client cert over mTLS (`403` otherwise), so only an already-paired peer can trigger it; pairing itself requires the user to enter the PIN.
- **Presented cert doesn't match the pin (impersonation / stale pin)**: a peer presents a cert that doesn't match what's pinned for its claimed UUID. → Reject the handshake (`403`), log the presented subject/fingerprint vs. the pinned one. A *different* cert for an already-pinned UUID is never silently re-pinned — it requires an explicit re-pin / re-invite flow (§4, key rotation).
- **Cert expiry**: a peer's (or this node's) leaf expires, breaking handshakes. → Monitor and warn ahead of expiry (§13); rotation re-pins via a re-invite or explicit re-pin path (§4). Clock skew between nodes can cause spurious not-yet-valid / expired errors — leaves are issued with a small backdated `notBefore` to absorb modest skew.
- **Key material missing or corrupt on startup** (`node.key`/`node.crt` unreadable): the node can't authenticate. → If `identity.json` exists but the keypair is gone/corrupt, fail loudly (don't silently mint a new UUID — that would orphan every existing pin on peers); a genuinely first run (no `identity.json`) mints fresh. Loss of identity requires re-inviting the node.
- **Malformed payload / unknown method or endpoint**: could propagate garbage. → Validate every envelope; reject (`400`) inter-node or drop-and-log locally; never apply unvalidated changes.
- **Invite never answered**: a `pending` invite would linger forever on both the inviter and the receiver. → TTL expiry (default 5 minutes) moves it to `expired` on **both** sides — the receiver also signals the inviter (`phase:"expire"`) for immediate teardown, with each side's own TTL as the fallback; `cluster:invite-status` reflects it.
- **Local interface severed** (Broker exited → `stdin` EOF / `stdout` `EPIPE` / `ERROR_BROKEN_PIPE`): an orphaned manager would have no one to serve. → Treat EOF/`EPIPE` as the shutdown signal: stop the listener and exit cleanly. No reconnect/buffering — a new Broker spawns a fresh manager (which reloads persisted membership if §4 lands on persistence).
- **Stale discovery snapshot** (departed node still resolvable, or new node not yet in a snapshot): wasted delivery attempts or a not-yet-invitable node. → Replace the resolver map from every `discovery:nodes` snapshot, so a node absent from the next one stops resolving without any expiry logic here; liveness is the discovery daemon's. A Broker-supplied address is the fallback when the resolver is stale or empty.

## 13. Observability
- **Logging**: pairing lifecycle (initial-exchange started / completed, PIN issued [**never the PIN value**], PIN entered, paired / declined / expired / failed) with `inviteId` and peer; EAP-NOOB protocol errors (`MACs`/`MACp` mismatch, unrecognized `NoobId`); membership changes (join, local removal, peer-initiated removal) with `nodeId`/`nodeUuid`; cert pins added and dropped (with UUID + fingerprint); mTLS rejections (`403`) with the presented subject/fingerprint vs. the pinned one; cert expiry warnings (own and peer leaves); first-run identity generation; inter-node delivery failures including final drop; malformed/protocol-error rejections (`400`); resolver changes applied from a `discovery:nodes` snapshot, and whether a dialed address was resolved or Broker-supplied; startup and clean shutdown on `stdin` EOF / `stdout` `EPIPE`. Never log private key material, full cert PEMs at info level, or the PIN.
- **Metrics**: pairings started/completed and outcome counts by terminal state; failed-PIN-attempt count (online-guessing signal); pending-pairing count and age (expiry pressure); current member count; pinned-cert count; mTLS handshake failure count and `403` count; days-to-expiry of the nearest-expiring pinned/own cert; inter-node delivery success/failure/retry/drop counts; resolvable peer count and the resolved-vs-Broker-supplied split of dialed addresses.
- **Alerts**: a spike in failed PIN attempts for a session (possible online guessing); sustained inter-node delivery failures to a peer; elevated `403` / `400` rate (untrusted caller or misconfiguration); a pinned or local cert approaching expiry; persistent membership divergence once a heartbeat exists.

## 14. Sample usage
On first run, nodes A and B each mint a stable UUID + keypair and a self-signed cert under `<config-dir>/cluster/` (§7.4). Both start unclustered (`cluster:get-node-id` → `clusterId: ""`).

On node A, the user clicks "Create cluster"; A's Broker sends `cluster:create`, which mints a unique UUID v4 `clusterId` (shown here as the illustrative `"cluster-xyz"`, friendly name "Lab 3 desks"), records A as founder, and emits `cluster:identity-changed` (the Broker persists it to `nvpair-node-settings`). A is now a cluster of one and can invite. (Node B does nothing here — it will join A's cluster by accepting the invite.)

On node A, the user opens the cluster view and invites node B (discovered on the LAN or entered manually). The Broker on A sends `cluster:invite-node` with B's address. A's Cluster Manager (EAP-NOOB **Server**) runs the Initial Exchange with B over `POST /v1/cluster/pairing`, embedding A's cert PEM in `ServerInfo`; B (EAP-NOOB **Peer**) auto-responds, embedding its cert in `PeerInfo`. Both reach `Waiting`. A derives the six-digit PIN `402199` and returns `{inviteId: "inv-9f3a1c", state: "pending", pin: "402199"}`; A's UI shows the PIN.

B's Cluster Manager emits `cluster:invite-received`; B's UI prompts "enter the PIN shown on Lab desk A". The user reads `402199` from A's screen (in person / over the phone) and types it on B; B's Broker sends `cluster:respond-to-invite` (`accept: true, pin: "402199"`). B feeds the PIN to EAP-NOOB (`OOBInputNoob`, moving its Peer to `OOBReceived`) and then **drives the Completion Exchange back to A** on the pairing channel: B POSTs an empty-`msg` kickoff, A (resuming its `inviteId`-keyed Server) replies with the reconnect Discovery, and the loop runs Discovery → NoobID → MACs/MACp until A returns `EAP-Success` (B is the HTTP client throughout; A is still the EAP-NOOB Server — §7.2). Both reach `Registered`. Each side reads the other's now-authenticated cert from the transcript and **pins it** (keyed by the peer's UUID); A's invite flips to `paired` and both record each other as `member`.

From here every trusted inter-node call between A and B is mutually authenticated over mTLS — each presents its leaf and is verified against the other's pin. Both Brokers see each other in `nodes:get-initial` (and via the proposed `nodes:changed` push). Later, the user on A removes B: the Broker sends `nodes:remove` (`nodeUuid` of B); A drops B locally, **deletes B's pin** (so any further handshake from B fails `403`), and notifies B via `POST /v1/cluster/members/remove` over mTLS, so B drops A too. If B was offline, the removal and de-pin still apply on A immediately and reconcile when B returns.

> Reminder (§4): the six-digit PIN is a temporary, MITM-vulnerable bootstrap accepted to ship the flow; it must be replaced by a high-entropy pairing code before the cluster trust is relied on in production.

## 15. Process model, CLI, and build wiring

**Process model.** A single Go binary, `nvpair-cluster-manager`, speaking newline-delimited JSON-RPC 2.0 on stdio by default (`--ipc` selects a Unix socket / Windows named pipe, like the other subprocesses). It runs one inter-node HTTP listener on `:14321` (§7.5) serving both the plain-HTTP pairing channel and the mTLS trusted endpoints. On Windows the console window is hidden by the parent via `syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}` — the same pattern every other subprocess uses (`subprocess_windows.go` / `subprocess_other.go`); the binary itself needs no special handling.

**CLI flags** (minimal, matching repo conventions):
- `--version` — print `main.Version` (stamped via `-ldflags "-X main.Version=…"` from `versions.json`; never hardcoded — §AGENTS Versioning) and exit.
- `--ipc <path>` — use a Unix socket / named pipe instead of stdio.
- `--log-level <level>` — initial `nvpair-shared/applog` level; mutable at runtime via the `log/set-level` JSON-RPC method (§7.0).
- `--config-dir <path>` (optional override) — where `cluster/` lives; defaults to the shared per-user config dir (§7.4).
- `--port <n>` (optional override, default `14321`) — the inter-node listener port.

No flag carries the node identity (self-generated, §7.4) or the cluster identity (`cluster:set-identity`, §7.0), keeping the binary self-bootstrapping under any supervisor.

**Build wiring**: `nvpair-cluster-manager` is one of the Go binaries in the product bundle. Adding it requires, in the same change:
- a `components.nvpair-cluster-manager` entry in `versions.json` and a `services` bump, both declared in the pull request's release-intent block rather than edited by hand (see `VERSIONING.md`);
- a build + copy step in **both** `build.bat` and `build.sh` (→ the repo-root `build/bin/` bundle), with the `-X main.Version=…` ldflag;
- inclusion in the NSIS installer (`installer/nvpair-setup.nsi`) and any firewall-rule list (it opens TCP `14321`), plus `bom.md`;
- a `product` bump declared in the pull request's release-intent block (see `VERSIONING.md`).

Because it is launched on demand by `nvpair-ui-broker` (which expects workers as sibling files in its working dir, like the other broker-supervised binaries), it must land in the same `build/bin/` directory as the rest.
