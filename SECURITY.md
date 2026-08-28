<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Security

NVIDIA is dedicated to the security and trust of its software products and
services, including source code repositories managed through its organization.

**Do not report security vulnerabilities through public GitHub issues or pull
requests.** If someone discloses a potential vulnerability publicly, maintainers
may limit public discussion and redirect the reporter to a private channel.

## Report a Vulnerability

Report a potential vulnerability in NVIDIA Personal AI Router (PAIR) or another
NVIDIA product through one of these channels:

- Submit the [NVIDIA Vulnerability Disclosure Program form](https://www.nvidia.com/en-us/security/report-vulnerability/).
- Email [psirt@nvidia.com](mailto:psirt@nvidia.com). For encrypted email, use
  the [NVIDIA public PGP key](https://www.nvidia.com/en-us/security/pgp-key).
- If private vulnerability reporting is enabled for this repository, open the
  repository's **Security** tab and select **Report a vulnerability**.

Include the following details in the report:

- The affected version or commit
- The vulnerability class
- Reproduction steps
- A proof of concept, when available
- The expected impact

Remove unrelated personal information, credentials, private keys, tokens,
prompts, and model data from the report.

The NVIDIA Product Security Incident Response Team (PSIRT) triages submitted
reports. Refer to
[NVIDIA Product Security](https://www.nvidia.com/en-us/security/) for current
policies, bulletins, and disclosure information.

## Security Architecture Context

PAIR is a LAN-first, multi-process application:

- Electron renderer code uses a typed preload bridge to reach Electron main.
- Electron main starts `nvpair-ui-broker` and exchanges newline-delimited
  JSON-RPC 2.0 over the broker's standard input and output.
- The broker supervises Go workers and relays their control-plane methods and
  notifications.
- Discovery and selected metadata endpoints operate on the local network.
- Ollama-compatible and OpenAI-compatible local HTTP proxies carry inference
  traffic and may route a request to another paired node.
- The cluster manager uses a six-digit PIN to bootstrap trust. Cluster-scoped
  peer traffic and promoted proxy ingress are designed to use certificates and
  mutual TLS after pairing.

The desktop application relays pairing and membership operations. The Go
services own cryptographic identity, trusted certificates, and inter-node
transport.

## Documented Risks and Assumptions

These statements describe security boundaries visible in the current source.
They are not claims that every deployment is secure.

### Inference Endpoints Are Loopback-Only

A proxy's plaintext personality accepts requests from loopback only. It refuses
a plaintext request from any other address. The same port also serves a
mutual-TLS ingress for paired cluster members, and PAIR forwards that traffic to
the node's own engine rather than routing it onward.

This is deliberate. A network-reachable plaintext endpoint would make any node an
open relay for inference to anything that can route to it. Run an application on
a node and use that node's local endpoint. Exposing an engine to the network
directly is outside PAIR and is the operator's decision and risk.

### Local Network Is a Trust-Relevant Boundary

PAIR discovers nodes and exposes service metadata on the LAN. Some discovery
enrichment and node-information traffic can use plain HTTP. Treat an untrusted
Wi-Fi, shared office network, compromised router, and hostile local process as
potentially adversarial. Network segmentation and host firewall rules remain
the operator's responsibility.

“Local-first” describes the intended topology. It does not prove that no data
leaves the machine or LAN. Inference engines, model catalogs, update systems,
applications, and user configuration may contact external services.

### Pairing PIN Is a Bootstrap Convenience

The six-digit PIN is low entropy. Do not treat it as a durable credential or as
strong proof of physical presence. Pair only while both devices and the network
are trusted, compare the displayed context, reject unexpected requests, and
remove members that are no longer authorized.

The cluster manager is expected to establish certificate trust after the PIN
exchange. The PIN does not protect a node before pairing and does not compensate
for a compromised paired member.

### Mutual TLS Has a Limited Scope

Cluster mTLS protects participating cluster channels only when identity and
certificate material are present and the relevant workers use that transport.
It does not automatically protect loopback HTTP, plain discovery metadata,
node-information HTTP, Electron IPC, third-party engine APIs, or traffic outside
PAIR. Certificate storage and operating-system account protection are part of
the boundary.

### Local APIs and Proxies Can Expose Sensitive Operations

The broker's stdio, Unix-socket, or named-pipe transport relies on the parent
process and operating-system endpoint permissions. It has no per-message bearer
token. PAIR's local HTTP proxies carry prompts, generated content, model names,
and request metadata. A process that can reach a local endpoint may be able to
submit inference or observe behavior allowed by that endpoint.

Do not bind local APIs or inference engines to untrusted interfaces, forward
PAIR ports through a router, or place an unauthenticated public reverse proxy in
front of them. Review browser access and CORS behavior before allowing web
content to reach a proxy. Prompts, messages, chunks, and response bodies should
not be written to logs.

### Supervised Workers Share the User's Authority

The broker starts workers under the current operating-system account. Engine
installation, model management, local settings, logs, and cluster keys may be
sensitive. Protect the account and PAIR data directory, and review changes to
worker paths or packaged binaries before running them.

## Security-Sensitive Changes

Changes to these areas require focused review and tests:

- Preload and Electron IPC exposure
- JSON-RPC framing, method routing, and subprocess paths
- Discovery, node identity, and manual-node handling
- Pairing, certificates, membership, and member removal
- Proxy listeners, CORS, routing, and engine port ownership
- Logging, diagnostics, updates, installers, and privileged helpers

Do not commit credentials, signing material, real cluster keys, private prompts,
personal data, or proprietary datasets. Use synthetic fixtures.
