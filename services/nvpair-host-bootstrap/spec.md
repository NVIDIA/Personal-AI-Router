<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Host Bootstrap Specification

## Purpose

`nvpair-host-bootstrap` is the only component that mutates privileged native
state for initial PAIR device preparation. It runs on the target, owns the
operation journal and receipt, and reconciles a closed resource set. It is not a
broker worker and exposes no network service.

## Scope

In scope:

- Quick Connect and Zero Touch operation bindings;
- Windows, macOS, and Linux on amd64 and arm64, with Linux restricted to Debian
  and Ubuntu;
- SSH service, firewall where applicable, authorized public key, fixed helper,
  exact product tree, and one runtime owner;
- inspect, deterministic review, apply, verify, complete receipt, repair, crash
  recovery, and ownership-bound uninstall.

Out of scope:

- storing or using account passwords or private keys;
- arbitrary commands, scripts, package-manager choices, or install paths;
- SSH host-key trust decisions after bootstrap;
- cluster pairing, certificates, or inference routing;
- supervision by Electron, the broker, or engine-manager.

## Input contract

All documents use `hostbootstrap.SchemaVersion`. The request carries a
32-lowercase-hex operation ID and one immutable binding:

- target: `windows|darwin|linux` and `amd64|arm64`;
- lane: `quick-connect|zero-touch`;
- role: `auto|desktop|headless`;
- runtime owner: `desktop|headless`;
- canonical target account and paths;
- canonical IP-literal SSH endpoint;
- validated ED25519, RSA, or ECDSA P-256 public-key identity; and
- distinct fixed product and helper artifact identities.

An explicit role must equal the runtime owner. Auto is valid only after the
caller resolves an owner. Product and helper paths and IDs must differ. Unknown,
duplicate, case-aliased, secret-bearing, command-shaped, oversized, or
noncanonical fields are rejected.

## State machine

The phases are `inspect`, `review`, `apply`, `verify`, `complete`, and
`blocked`.

- Inspect validates the native target and emits observations without mutation.
- Review is side-effect free and deterministically derives one decision and
  ordered action list from the request and observations.
- Apply requires a byte-equivalent fresh review state before mutation.
- Verify requires exact desired state and seals the receipt.
- Complete and blocked are terminal.

Decisions:

- `apply`: one or more resources are absent;
- `repair-owned`: PAIR-owned resources differ from the immutable binding;
- `no-op`: exact desired state already exists;
- `refuse-foreign`: at least one fixed resource has foreign ownership.

No-op transitions from review directly to verify. Apply and repair transition
through apply. A foreign review transitions only to blocked.

## Target and owner resolution

The target must equal the running OS and architecture. Linux additionally reads
a root-owned, non-writable canonical os-release file and accepts only
`ID=debian` or `ID=ubuntu`.

The operation must leave exactly one owner:

- Desktop: no fixed headless owner; the marker records the desktop broker tree.
- Windows Headless: SCM owns `nvpair-host-helper.exe headless-service`, which
  starts only the fixed installed broker path.
- macOS Headless: system launchd owns the exact generated definition.
- Linux Headless: system systemd owns the exact generated unit.

Owner inspection classifies the broker's parent process tree. An unclassified
broker, simultaneous Desktop and Headless trees, or a live Desktop tree during a
Headless transition fails closed.

## Payload and publication

The executable reads its sibling `payload/manifest.json` and exactly two payload
artifacts: helper and product archive. Target, IDs, versions, byte counts,
digests, filenames, and product file inventory must match the reviewed binding.

Archive paths, case folding, entry ordering, file types, modes, sizes, and
digests are canonical. Publication uses private staging, exact post-write
verification, durable rename/sync, and operation-bound recovery paths.
Existing bytes may be replaced only when their prior identity is explicitly
authorized.

## Authority and persistence

Target state is root-owned, private, no-follow, single-link storage. The
operation envelope, reviewed principal, resource markers, pending effect,
uninstall pending effect, immutable receipt, and uninstall tombstone are
separate exact-shape documents.

The target's observation and receipt are authoritative. A helper/controller
request contains no observation, action list, plan body, or receipt capable of
overriding that state.

## Recovery and repair

Every native action has an expected pre-state and post-state. Pending state is
written before the effect. Recovery validates operation, plan digest, action
index, resource, marker, and expected states, then proves whether the effect is
before or after; any third state returns `ErrPlanChanged`.

Repair requires the same operation and binding, a prior complete receipt, no
unresolved pending effect, and a plan whose decision is `repair-owned` or
`no-op`. Only receipt-and-marker-authorized owned drift is repairable.

## Uninstall

Uninstall requires administrator/root authority, the exact stored request, and
the matching complete receipt. All candidate removals are validated before the
first mutation. Owned resources are removed in this order:

1. active runtime owner;
2. product;
3. helper;
4. authorized key;
5. firewall; and
6. SSH service.

Each removal is journaled and absence-verified. Final absent observations and a
receipt digest are written to the uninstall tombstone before active operation
state is removed. Non-owned or changed resources are never deleted.

## Security constraints

- No shell interpretation or caller-selected executable path.
- No password, passphrase, private key, PIN, certificate, or cluster secret in
  any contract or state file.
- Administrator/root authority is checked natively.
- Fixed definitions, paths, ACLs/modes, owners, links, and process identities
  are re-inspected before mutation and verification.
- Official package provenance is valid only with complete required catalog and
  signature metadata. Unsigned engineering provenance remains non-official.

## Verification boundary

Unit tests cover strict decoding, state transitions, native probes/effects,
recovery, repair, uninstall, owner classification, and payload publication.
Cross-compilation and deterministic package verification do not constitute
native acceptance of the full six-target matrix.
