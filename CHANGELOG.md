<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Changelog

Release history for Personal AI Router, newest first. Entries match the
published releases on GitHub.

Builds from this repository are unsigned and configure no update feed, so
automatic updates are unavailable in them.

## 1.0.21 — Keep slow engine status checks from delaying engine controls (#180)

- Starting, stopping, and changing an engine's port no longer wait for a separate status check to finish.
- Model-list requests from other devices now return an error if they cannot finish in time, allowing those devices to keep their last known model lists.

## 1.0.20 — Recover stale macOS helper registrations after upgrades (#201)

- Allow a delayed, bounded retry when macOS retains the privileged helper's old signing identity after an upgrade.
- Preserve the retry limit across application restarts and pending background-item approval.

## 1.0.19 — Launch at login and start minimized to the tray (#97)

- New Startup section in Settings > Service with two switches: launch at login, and start minimized to the tray.
- A launch by the login item stays in the tray and opens the Overview from the tray menu; a launch by hand still opens the window.
- Launch at login shows what the operating system will do. If you disable or remove PAIR's startup entry outside the app (Windows Task Manager or startup apps settings, macOS System Settings, or your Linux desktop's startup settings), PAIR leaves it off and the switch shows off; turning the switch on again enables it.
- Supported on Windows, macOS and Linux. Off by default: nothing is registered until launch at login is enabled.

## 1.0.18 — Choose ModelScope for llama.cpp model downloads (#186)

- Select Hugging Face or ModelScope in llama.cpp Settings. The saved choice controls model browsing, search, and downloads on that device; Hugging Face remains the default.
- ModelScope verifies complete download sizes and SHA-256 checksums. Interrupted downloads restart from the beginning.
- Model search runs automatically after two characters and a short typing pause. Searches run one at a time, with only the latest query continuing next; Enter and the search button bypass the pause.

## 1.0.17 — Stalled candidates yield to their peers for one deadline (#144)

- A candidate that used up a full first-byte deadline without producing content is ranked behind its peers for one deadline.

## 1.0.16 — Local node figures stay current in discovery (#5)

- The scanner republishes its own entry on every peer refresh, so GPU, CPU and memory figures follow the node instead of freezing at daemon start.

## 1.0.15 — vLLM engine support (#181)

- vLLM can now be installed and managed from PAIR like the other engines.
- vLLM can download, load, eject and delete models from the local Hugging Face cache; loading another model restarts vLLM on it, and the previous model is restored if it fails.
- vLLM picks the right tool-call parser for each model, so agent clients work with Qwen, Granite and other models.
- vLLM uses the Hugging Face cache (or its `--download-dir`), mirror, token and offline settings set in its launch settings, and downloads only the weights it loads.
- vLLM is shown as starting while it downloads and loads a model, and Stop or Eject works during that time.
- A download that vLLM could not load is refused before anything is downloaded, and Add model shows who published each model.
- On a Mac, FP8 models that vLLM-Metal cannot run are refused with a message to choose the BF16 version.
- The terminal interface shows a downloaded or deleted model straight away, and can download into a stopped vLLM.
- vLLM is no longer offered on Windows, where it cannot run.

## 1.0.14 — Simplify Go concurrency and collections with standard library helpers (#195)

- Refresh internal service concurrency and collection code while preserving intended application behavior.
- Apply `go fix` across the repo

## 1.0.13 — Limit JSON reads for node metadata and peer messages (#167)

- Limit JSON memory usage when processing input for node telemetry, model inventories, error synchronization, and remote engine progress. Improve connection reuse when fetching node metadata and model inventories.

## 1.0.12 — Restore macOS privileged helper communication after upgrades (#192)

- Preserve the helper-specific signing identifier needed to communicate with the macOS background helper and complete setup.
- Replace an enabled but unreachable helper once per app version after an upgrade, and keep setup incomplete until helper communication and firewall configuration succeed.

## 1.0.11 — Use the PAIR symbol in the macOS menu bar (#194)

- Replace the opaque app tile with a transparent PAIR house-and-network symbol, with Retina support.

## 1.0.10 — Background services shut down in parallel (#173)

- The background services now shut down in parallel rather than one at a time, so a busy machine no longer pays each service's shutdown in turn.
- When quitting is slow, the service log records where the time went.

## 1.0.9 — Quitting is prompt again (#137)

- Quitting no longer stalls when an engine is running outside PAIR's control. The engine shutdown that runs on the way out was being performed twice, and the repeat cost about a second.

## 1.0.8 — Removing a machine you added by address forgets it in the terminal interface (#172)

- In the terminal interface, removing a cluster member that you added with `f` now also forgets its address, so it no longer reappears after a restart.

## 1.0.7 — Copy API endpoints with /v1 included (#185)

- API endpoint URLs now include `/v1` when displayed or copied, so they can be pasted directly into Hermes and other clients that expect an OpenAI-compatible base URL.
- The getting-started examples use the copied URL directly and explain how to remove `/v1` for native Ollama clients.

## 1.0.6 — Model lookups through PAIR reach a node that has the model (#163)

- `/api/show` requests to PAIR's Ollama port now go only to a node that has the requested model. Before, they could reach a node without it and get a 404, so a client such as Hermes Agent could not read the model's details.
- Ollama model names that include the default `library` namespace or `registry.ollama.ai` host, or that differ only in case, now match the same model when PAIR picks a node. Inference requests that use such names reach a node that has the model instead of failing with a 502.

## 1.0.5 — Harden trusted-peer file paths (#159)

- Reject unsafe peer identifiers before loading, writing, or removing trust-store files, protecting cluster data from path traversal.

## 1.0.4 — Manual nodes persist across restarts (#60)

- Manually added nodes are restored after the broker or manual-node worker restarts.

## 1.0.3 — Report Ollama model download failures correctly (#138)

- Ollama model downloads now report failures when the engine sends an error record after starting an HTTP 200 stream.

## 1.0.2 — Route OpenAI Responses requests by model (#155)

- POST /v1/responses now counts as inference. PAIR sends it to a node that advertises the requested model and records it as a workload. That path used to be forwarded with no model filter.

## 1.0.1 — Add documentation for registry.go and refactor types/tests (#140)

- Refactored saved engine launch settings while preserving literal arguments, environment values, and explicit empty overrides.

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
