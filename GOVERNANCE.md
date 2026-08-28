<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Governance

NVIDIA Personal AI Router (PAIR) is developed through pull-request review.
Project maintainers review proposed changes and merge them after acceptance.

## How Changes Are Accepted

Follow this process to land a change:

1. Open a pull request describing the problem and the observable outcome. If an
   issue already covers it, link that issue.
2. A maintainer reviews the change and merges it when it is ready.
3. Security-sensitive changes follow the private process in
   [SECURITY.md](SECURITY.md) rather than a public pull request.

This repository runs no automated checks on pull requests, so contributors run
the checks documented in [CONTRIBUTING.md](CONTRIBUTING.md) and record the
results in the request.

## Releases

A release updates the following together:

- Version manifests
- Notices
- Affected documentation
- Release notes on the
  [releases page](https://github.com/NVIDIA/Personal-AI-Router/releases)

Artifact publication is a separate controlled process. This document does not
define that process.

## Amendments

Amend this document through the same pull-request process.
