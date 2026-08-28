<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Support

Community support for NVIDIA Personal AI Router (PAIR) is best-effort.
Maintainers do not guarantee a response or a resolution time.

## Where to Get Help

Use the channel that matches your request:

- **Reproducible bugs:** Open a [bug report](https://github.com/NVIDIA/Personal-AI-Router/issues/new?template=bug.yml).
- **Feature proposals:** Open a [feature request](https://github.com/NVIDIA/Personal-AI-Router/issues/new?template=feature.yml).
- **Documentation or maintenance work:** Open a [task request](https://github.com/NVIDIA/Personal-AI-Router/issues/new?template=task.yml).
- **Security vulnerabilities:** Follow [SECURITY.md](SECURITY.md). Do not open a public issue.

Before opening an issue, refer to
[docs/troubleshooting.mdx](docs/troubleshooting.mdx) and search the existing
issues. Then include the following details in your report:

- The PAIR version
- The operating system
- The hardware
- The inference engine and model
- Reproduction steps
- Sanitized logs, where relevant

PAIR ships a sanitizer that collects the logs and replaces host names,
addresses, and account names with stable labels, so a bundle stays readable
without carrying your network's details. It is in your install under
`resources/scripts/`, and needs nothing else installed to run. See
[Collecting logs](docs/log-collection.mdx) for what it does and does not remove,
and [Before sharing a log](docs/troubleshooting.mdx#before-sharing-a-log) for
where to find it.

## Scope

Maintainers may prioritize reports based on the following factors:

- Severity
- Security impact
- Reproducibility
- Supported configurations
- Release readiness
- Available capacity
