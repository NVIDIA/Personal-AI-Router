<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Changelog

Release history for Personal AI Router, newest first. Entries match the
published releases on GitHub.

Builds from this repository are unsigned and configure no update feed, so
automatic updates are unavailable in them.

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
