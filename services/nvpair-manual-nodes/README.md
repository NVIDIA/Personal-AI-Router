<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# nvpair-manual-nodes

A Go service for managing manually configured nodes on networks where mDNS discovery is unavailable — a filtered LAN, or an overlay network such as a Tailscale tailnet, which carries no multicast at all. Accepts node addresses via JSON-RPC, probes each one, and emits status events.

There are two kinds of manual node, and one probe tells them apart. **node-info is asked first**, and its answer decides everything else:

- **A bare inference host** — Ollama or LM Studio on a machine that does not run PAIR. It serves no `/v1/node-info`, so its engines are probed on their own ports in plain HTTP and a supervising broker bridges it into the local proxies as a routing target. This is what manual nodes were originally for.
- **A PAIR node** — it answers `/v1/node-info` *with a `services` map*. It is then reported with `pair_node: true`, its cluster principal, its service map, and its model inventory read from its engine manager over cluster mTLS. A supervising broker folds it into the discovery directory as if it had been found over mDNS.

A PAIR node is **never** probed on its engine ports. On such a node `:11434` and `:1234` are the proxy facades, which refuse plaintext from anything but loopback, so a probe there is a guaranteed `403` that would report a healthy peer as having no engines.

## Communication

Uses bidirectional newline-delimited JSON-RPC 2.0. By default, communication is over stdin/stdout. An alternative IPC transport (Unix domain socket or Windows named pipe) can be specified with `--ipc <path>`.

## CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--ipc <path>` | _(stdio)_ | IPC endpoint: Unix socket or Windows named pipe |
| `--client-cert <path>` | _(none)_ | PEM client certificate to present when probing TLS-enabled manual nodes (requires `--client-key`) |
| `--client-key <path>` | _(none)_ | PEM client private key matching `--client-cert` |
| `--ca-bundle <path>` | _(none)_ | PEM bundle of CAs to trust when verifying server certificates (additive to the system trust store) |
| `--cluster-dir <path>` | _(none)_ | Cluster config dir; when set, a TLS manual node's node-info is probed over cluster mTLS, presenting this node's leaf and accepting any currently-pinned server cert |
| `--log-level <level>` | `info` | `error` / `warn` / `info` / `debug`; falls back to `$NVPAIR_LOG_LEVEL`. Also changeable at runtime via the `log/set-level` method |
| `--version` | | Print version and exit |

## JSON-RPC Events (notifications, manager → caller)

### `ready`

Emitted once on startup.

```json
{"jsonrpc":"2.0","method":"ready","params":{"version":"0.1.0"}}
```

### `node/discovered`

Emitted when a manually added node has been probed and its initial status determined.

```json
{
  "jsonrpc":"2.0",
  "method":"node/discovered",
  "params":{
    "id":"manual:10.0.1.50",
    "address":"10.0.1.50",
    "ollama_up":true,
    "ollama_port":11434,
    "ollama_models":["llama3.2:latest","gemma3:4b"],
    "lmstudio_up":true,
    "lmstudio_port":1234,
    "lmstudio_models":["qwen2.5-7b-instruct"],
    "node_info_up":true,
    "node_info_port":14318,
    "pair_node":false,
    "gpus":[{"name":"NVIDIA GeForce RTX 3080","utilization_percent":37}],
    "telemetryValid":true,
    "msSince":120,
    "hostUuid":"stable-node-uuid"
  }
}
```

Each node is probed for both inference engines: Ollama on its default `:11434` (`GET /` + `/api/tags`) and LM Studio on its default `:1234` (`GET /v1/models`, which doubles as the liveness check and the model list). `lmstudio_up` / `lmstudio_port` / `lmstudio_models` mirror the `ollama_*` fields and let a supervising broker bridge the node into `lmstudio-proxy` the same way it bridges Ollama into `ollama-proxy`. A node can run either engine, both, or neither.

### `node/updated`

Emitted when a periodic probe detects a change (service going up/down, models,
GPU values, telemetry validity, or sample age changed). `telemetryValid` and
`msSince` preserve node-info's distinction between an idle 0% sample and missing
telemetry so the broker can feed manual nodes into the same scheduler cache.

### `node/removed`

Emitted when a node is explicitly removed via `node/remove`.

### `errors:report` / `errors:clear`

Emitted so the supervising broker can forward them into the `nvpair-errors` pipeline. A node whose probes fail `probeFailThreshold` consecutive times (3 probes, so roughly 30 s of unreachability) reports under the id `manual-nodes:probe-failed:<node-id>`; a subsequent successful probe clears the same id.

## JSON-RPC Methods (caller → manager)

### `node/add`

Add a node by address. The manager immediately probes it and emits a `node/discovered` event.

A hostname is preferred over an IP literal: probe clients disable keep-alives specifically so every probe re-resolves the name, which lets a node that gets a new address recover on its own. An IP-literal entry is dead once the device is renumbered. Supply the address on its own — a `host:port` string is not parsed, because ports are appended to it, so such an entry reads permanently down.

```json
{"jsonrpc":"2.0","id":1,"method":"node/add","params":{"address":"10.0.1.50","name":"my-server"}}
```

| Param | Required | Description |
|---|---|---|
| `address` | Yes | IP address or host name of the node, with no port. A `host:port` string is **rejected** with an actionable error: every probe appends its own service port, so such an entry could never be reached |
| `name` | No | Friendly name (used as node ID; defaults to `manual:<address>`) |
| `ports` | No | Per-service port overrides: `{node_info, cluster, ollama, lmstudio, vllm}`. An unset field keeps that service's default. Persisted and echoed back as `ports`. `vllm` is carried but not probed yet |
| `tls_port` | No | Probe node-info over HTTPS on this port instead of plain HTTP. Takes precedence over `ports.node_info`, since it names an HTTPS listener and therefore names its port. Echoed back as `tls_enabled` |
| `mtls` | No | Stored and echoed back as `mtls_required`. The probe transport itself is chosen by `tls_port` and live cluster membership, so this field records intent rather than driving it |

An address may be a MagicDNS name (`gpu-box.tail1234.ts.net`), a `.local` name, an IPv4 literal, or an IPv6 literal in plain or bracketed form.

Response: the initial node status object.

### `node/remove`

Remove a previously added manual node.

```json
{"jsonrpc":"2.0","id":2,"method":"node/remove","params":{"id":"my-server"}}
```

### `nodes/list`

Returns all currently tracked manual nodes with their latest probe status.

```json
{"jsonrpc":"2.0","id":3,"method":"nodes/list"}
```

### `shutdown`

Gracefully shuts down the manager.

```json
{"jsonrpc":"2.0","id":4,"method":"shutdown"}
```

### `log/set-level`

Changes the log level at runtime. Accepted as either a request (answered with `{"level":"<resolved>"}`) or a fire-and-forget notification.

```json
{"jsonrpc":"2.0","id":5,"method":"log/set-level","params":{"level":"debug"}}
```

## Probing

Each manual node is probed every 10 seconds, with a 3-second timeout per leg. **node-info is asked first**, because its answer decides which other legs may run at all:

- **Node Info** on port 14318 (or `ports.node_info`, or `tls_port` over HTTPS): hardware inventory, identity, cluster principal, and service map (`GET /v1/node-info`)
- Then, **only for a bare host** (node-info reported no service map):
  - **Ollama** on port 11434 (or `ports.ollama`): health check (`GET /`) and model list (`GET /api/tags`)
  - **LM Studio** on port 1234 (or `ports.lmstudio`): `GET /v1/models`, which doubles as the liveness check and the model list
- Or, **only for a PAIR node**, its model inventory from its engine manager (`GET /v1/models` on the `em` port from the service map) over cluster mTLS, pinned to the peer's cluster principal. No pin, no models: a peer does not serve its inventory to a stranger, so the node appears with its hardware and gains its models once paired.

A node can have any combination of these, or none if the target is unreachable. Status changes trigger `node/updated` events. Because change detection compares CPU, memory, and GPU values, a node running node-info emits a `node/updated` on most probe cycles as utilization moves.

`pair_node` holds across a failure episode rather than being recomputed per probe. One missed node-info answer is routine across an overlay network, and without that tolerance the gap would probe the peer's proxy facades in plaintext and withdraw it from every consumer for a cycle. It reverts past `probeFailThreshold` consecutive failures, so a node that genuinely stops being a PAIR node is not remembered as one.

## Shutdown

The manager shuts down on:
- stdin EOF (parent process closed the pipe)
- `SIGINT` / `SIGTERM`
- `shutdown` JSON-RPC request
