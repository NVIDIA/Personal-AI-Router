<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Changelog

Release history for Personal AI Router, newest first. Entries match the
published releases on GitHub.

Builds from this repository are unsigned and configure no update feed, so
automatic updates are unavailable in them.

## 1.0.0 — Personal AI Router 1.0.0

- Cuts release 1.0.0, which collects every change since 0.1.1 listed below. See the [1.0.0 release notes](docs/release-notes.mdx).

## 0.1.15 — Safer engine removal and data resets (#153)

- Removing an engine never follows a symlink or Windows junction into another folder.
- An LM Studio models folder linked from `~/.lmstudio/models` is kept when LM Studio is removed.
- Resetting app data, from Settings or the terminal interface, stops before deleting anything if an engine PAIR installed cannot be removed, and says which one.

## 0.1.14 — Engine install no longer shows a fake percentage (#145)

- Installing an engine no longer sits at a made-up 75%. Download progress still shows a real percentage; the install step itself now shows "Installing" with no number.

## 0.1.13 — Uninstalling keeps your downloaded models (#150)

- Uninstalling an engine keeps the models it downloaded. Uninstalling LM Studio no longer deletes `~/.lmstudio/models`.
- llama.cpp keeps its downloaded models in `~/.llamacpp`, outside PAIR's data folder, so removing PAIR's data no longer deletes them.
- Removing all data — when uninstalling PAIR, from Settings > Reset app data, or from the terminal interface's reset — now also uninstalls the engines PAIR installed, including LM Studio on Windows, and leaves engines you installed yourself alone. Downloaded models are kept.

## 0.1.12 — A rebuilt terminal interface, and one model catalogue for both front ends (#117)

- The terminal interface is organised around machines: Nodes, Jobs, Service, Errors, and Logs, with a per-machine drill-down for engines, models, ports, and hardware.
- Engines can be started with your own arguments and environment variables, edited from the terminal interface on this machine or on a paired one. Changes are validated before they are saved.
- Downloadable models are served by the engine manager, so the terminal interface and the desktop application browse the same catalogue.
- The Inference Demo runs from the terminal interface's Jobs tab, so a headless machine can demonstrate routing.
- The terminal interface says when a newer release of PAIR is published, on every tab until dismissed. It installs nothing.
- Pairing keys follow the words on screen: `p` pairs, `a` accepts, `f` finds a machine by address.

## 0.1.11 — llama.cpp is a PAIR-managed engine (#148)

- llama.cpp can be installed, started, stopped, and updated from PAIR, alongside Ollama and LM Studio.
- Browse and pull llama.cpp models from the model hub, sourced live from approved Hugging Face publishers, with a bounded search across public publishers.
- Delete a downloaded llama.cpp model from the engine's model list.
- Inference routed to llama.cpp goes through the local proxy, so the engine participates in cluster routing like the others.
- On Windows, PAIR installs the Visual C++ runtime llama.cpp requires.

## 0.1.10 — The desktop app is called NVIDIA PAIR (#147)

- The application now appears as **NVIDIA PAIR** in Finder, the Dock, and the macOS menu bar, instead of the abbreviated `PAIR`.

## 0.1.9 — Clearer in-flight jobs (#141)

- Jobs that have not started yet are labeled In flight, and they and their connection lines use the same yellow as the GPU chart.
- A job's card names the node it was sent to with "Sent to" until the job starts, including a job that fails or is cancelled before it starts, instead of "Ran on".

## 0.1.8 — Retry failed scheduler priority notifications (#122)

- The scheduler retries changed priorities after a failed local notification write and keeps its status aligned with the last successful delivery.

## 0.1.7 — Workload broadcast reliability: ordered broadcasts, dedup after emit (#80)

- Inter-node workload broadcasts are now serialized in origin order, so a lifecycle remove can no longer overtake its own upsert on a peer and resurrect a ghost workload.
- Dedup keys are recorded only after the broker emit succeeds; a failed emit's retry is no longer swallowed as a duplicate.

## 0.1.6 — The app is now called NVIDIA PAIR (#132)

- The desktop app, tray, shortcuts, and installer now show the name NVIDIA PAIR.
- Installed programs lists show NVIDIA Corporation as the publisher.
- Existing installs keep their settings, engines, cluster membership, and firewall rules when they update.

## 0.1.5 — Avoid unnecessary cluster trust notifications (#62)

- Cluster trust notifications now follow successfully saved peer endorsements. Duplicate endorsements and failed writes no longer cause unnecessary refreshes.

## 0.1.4 — Fix HTTP connection reuse in engine health probes (#37)

- Periodic engine monitoring now reuses HTTP/1 connections instead of opening a new connection for every health probe.

## 0.1.3 — Route Anthropic Messages API requests (#27)

- The Ollama and LM Studio endpoints offered by PAIR now accept requests for the Anthropic Messages API via POST to the /v1/messages endpoint and direct them to the owner of the requested model, just as happens with the other inference methods.

## 0.1.2 — fix(mdns): send responses from UDP 5353 (GitHub Issue #1) (#102)

- Discovery traffic now originates from UDP 5353, allowing standards-compliant mDNS peers and network reflectors to accept PAIR node records.

## 0.1.1

### Fixed

- Building from source for an Intel/AMD (`x64`) target failed while compiling
  the bundled log sanitizer. The packaging script passed Electron's `x64`
  architecture name straight to the Go toolchain, which expects `amd64`.
  Arm64 targets were unaffected.

This release contains no application changes. The fix is to the build tooling
only, and the published 0.1.0 installers were not affected by it, so 0.1.1 is
functionally identical to 0.1.0 for anyone installing it.

## 0.1.0

### Fixed

- **Windows on Arm installations were missing every executable.** The
  installer's compressed payload used a compression filter its extractor could
  not decode, so an install reported success with the application tree present
  but all `.exe` and `.dll` files silently skipped. Arm64 installs now
  complete correctly.
- **Windows on Arm now installs under 64-bit Program Files** instead of the
  32-bit location.
- **Silent installs no longer abort** on the installer's payload check.

### Changed

- Raised the Electron floor and updated the Go toolchain and service
  dependencies to pick up security fixes.
- Trimmed the README, and added an `AGENTS.md` so an agent working from a fork
  has an entry point.
