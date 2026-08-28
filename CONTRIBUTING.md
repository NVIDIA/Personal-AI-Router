<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Contributing to NVIDIA Personal AI Router

Thank you for helping improve NVIDIA Personal AI Router (PAIR). PAIR routes
independent inference requests across compatible systems on a local network.
Contributions should keep that behavior reliable, understandable, and secure.

Report suspected vulnerabilities through [SECURITY.md](SECURITY.md), not a
public issue or pull request.

## Before You Start

Complete these checks before you open a change:

- Search existing [issues](https://github.com/NVIDIA/Personal-AI-Router/issues)
  and pull requests.
- An issue is not required before you open a pull request. Link one if it
  already covers the change.
- When practical, discuss substantial behavior, API, dependency, security, or
  compatibility changes with maintainers before implementation.
- Keep each change focused. Avoid unrelated cleanup and speculative
  abstractions.
- Never commit the following:

  - Credentials
  - Tokens
  - Private keys
  - Personal data
  - Private prompts
  - Proprietary datasets
  - Real cluster identities

Use **PAIR** for the product name. Do not describe PAIR as pooling memory,
combining GPUs, or sharding one model across nodes.

## Issues and Feature Requests

Use GitHub Issues for bug reports, feature requests, and other product feedback.

For a bug report, include as much of the following as you can:

- The PAIR version
- Your operating system and relevant hardware
- Clear steps to reproduce the problem
- The expected and actual behavior
- Any other details that could help reproduce the issue

Logs are helpful but optional. You may report a bug without attaching them. A
maintainer may ask for more information when the available details are not
enough to reproduce or diagnose the problem.

Before you post logs publicly, review and redact sensitive personal or network
information such as usernames, device names, file paths, and IP addresses. Never
post credentials, tokens, private keys, or other secrets. If requested logs are
not suitable for a public issue, do not post them.
[Collecting logs](docs/log-collection.mdx) describes the script that redacts
this information for you.

For a feature request, describe the user problem, the outcome you want, and any
alternatives you considered.

## Development Setup

```bash
git clone https://github.com/NVIDIA/Personal-AI-Router.git
cd Personal-AI-Router
```

Development requires Node.js 25.5.0 or newer, npm, Go 1.25 or newer, `jq`, and
Git. Refer to [Building PAIR](docs/building.mdx).

On Linux, the repository-root `Makefile` wraps the commands below:

- `make dev` installs the Go and npm dependencies.
- `make build` builds the service binaries and desktop bundles.
- `make run` starts the application.
- `make check` runs the desktop gates.
- `make test` adds the Go suites.

Run `make` to list every target.

### Desktop Application

```bash
cd desktop
npm install
npm start
```

The desktop build compiles required Go binaries from `../services`.

Routine desktop checks:

```bash
npm run lint
npm run typecheck
npm test
npm run service-contracts:check
```

Use `npm run format` to apply repository formatting.

### Go Services

Linux and macOS:

```bash
cd services
./build.sh
```

Windows Command Prompt:

```bat
cd services
build.bat
```

Run cross-process tests:

```bash
cd services/tests
go test ./...
```

Run component tests from that component directory:

```bash
cd services/nvpair-ui-broker
go test ./...
```

Live engine and multi-node tests can modify real system state. Run them only
when explicitly intended and only against systems and networks you control.

## License Headers

Every file in this repository carries a two-line SPDX header naming the
copyright holder and the license. **Add one to every new file you contribute.**

```go
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
```

Use the comment syntax of the file's own language: `//` for Go, TypeScript, and
Swift, `#` for shell, Python, YAML, and dotfiles, `<!-- -->` for Markdown and
HTML, `/* */` for CSS. The year is the year the file was created. The header
goes at the top, below anything the format requires to come first, such as a
shebang, a YAML frontmatter block, or an XML declaration.

Check the tree, and fill in anything you missed:

```bash
node scripts/spdx-headers.mjs         # report files missing a header
node scripts/spdx-headers.mjs --fix   # insert the header where it is missing
```

The checker needs only Node.js and Git, so it works in a fresh clone before you
install any dependencies. On Linux and macOS, `make headers` and
`make headers-fix` run the same two commands, and `make check` includes the
check. Run it before you open a pull request: a maintainer will run it during
review, and a missing header will send the change back to you.

`--fix` only ever inserts a header. A file that already has one is reported
rather than rewritten, so an existing third-party or differently licensed notice
is never overwritten. Some files are exempt, mostly generated output and formats
with no comment syntax; `node scripts/spdx-headers.mjs --show-skipped` lists
them with the reason for each. If the checker reports that no rule covers your
file type, say so in the pull request instead of working around it, so a
maintainer can extend the policy.

## Architecture and Boundaries

Start with the [Developer Guide](docs/developing.mdx) for where the code lives and
how a change travels through the layers. Read
[Architecture](docs/architecture.mdx) before changing any of the following:

- Process ownership
- Inter-process communication (IPC)
- Discovery
- Pairing
- Engines
- Routing

Important rules:

- The renderer communicates through typed preload APIs. It does not connect
  directly to Go workers.
- Electron starts `nvpair-ui-broker`, not individual workers.
- The broker owns worker supervision and JSON-RPC relays.
- Backend notifications are authoritative for service state.
- Each inference request is routed to one eligible node.
- Pairing, certificates, and inter-node transport are owned by the services.
- Prompts and generated response bodies must not be logged.

When changing a JSON-RPC method or payload, update its producer, broker relay,
all consumers, tests, and relevant API documentation together.

## Tests and Evidence

Use the narrowest stable test that proves the intended outcome. Add regression
coverage at the earliest boundary that could have caught a defect.

Relevant areas include:

- Discovery and stable node identity
- PIN pairing, cancellation, rejection, expiry, and member removal
- Engine lifecycle, ports, model operations, and readiness
- Routing eligibility, priority, streaming, cancellation, and upstream errors
- Proxy ownership, listener exposure, cross-origin resource sharing (CORS), and port conflicts
- Subprocess restart, shutdown, and orphan prevention
- Sanitization of logs, diagnostics, and errors
- Install, upgrade, uninstall, and data retention

Performance results must identify the following:

- Hardware
- Software versions
- Model
- Engine settings
- Cluster size
- Concurrency
- Warm-up state
- Measurement method

## Documentation

Update documentation in the same pull request when behavior changes affect any of
the following areas:

- Build, installation, first run, or supported configurations
- Discovery, pairing, nodes, engines, models, or routing
- Endpoints, JSON-RPC methods, defaults, or ports
- Security, privacy, permissions, networking, or data handling
- Errors, troubleshooting, or known limitations

Distinguish current verified behavior from proposals. Do not publish internal
systems, credentials, unannounced features, or personal information in
screenshots and examples.

## Versions

When your change alters a service binary's compiled output, bump that component
in `services/versions.json` in the same pull request.
[Versioning](services/VERSIONING.md) gives the rules for choosing a patch, minor,
or major bump, and covers the `desktop/package.json` version, which follows its
own release cycle.

Say in the pull-request description which components you bumped and why, so a
reviewer can check the decision rather than infer it from the diff.

Describe any user-facing change in plain terms in the same description. Release
notes are written from the merged pull requests and published on the
[releases page](https://github.com/NVIDIA/Personal-AI-Router/releases), so a
clear description is what makes a change show up there correctly.

## Pull Requests

Anyone may submit a pull request. Follow this checklist:

1. Fork the repository.
2. Create a focused branch from `main` in your fork.
3. Explain the problem and observable desired outcome.
4. State what is intentionally in and out of scope.
5. Add or update tests and documentation.
6. Run relevant checks and record the commands and results.
7. Confirm every new file carries the SPDX license header by running
   `node scripts/spdx-headers.mjs`.
8. Sign off every commit, as described in
   [Developer Certificate of Origin (DCO) and Sign-Off](#developer-certificate-of-origin-dco-and-sign-off).
9. Review the diff for secrets, generated files, personal data, and unrelated
   edits.
10. Open the pull request against this repository for maintainer review.

Work from a fork rather than creating branches in this repository. Only
maintainers can merge into `main`.

A useful pull-request description covers the following:

- Outcome
- Validation environment
- Compatibility and security risks
- Documentation changes
- The related issue, if one exists

Maintainers review pull requests as bandwidth allows, so neither a response nor
a merge timeline is promised, and opening a pull request does not guarantee
acceptance. A review may cover code quality, architecture, security, dependency
provenance, licensing, and product testing. Maintainers may request changes, or
decline a contribution that does not meet the project's technical, security,
quality, or open-source requirements.

## Dependencies and Integrations

Maintainer alignment is required before you add any of the following:

- New dependencies
- Inference engines
- Model sources
- APIs
- Distribution channels

You must explain the following:

- Current requirement
- License
- Supported-version policy
- Validation boundary
- Maintenance plan
- Security implications

## Developer Certificate of Origin (DCO) and Sign-Off

Every commit in a pull request must be signed off. Signing off certifies that
you wrote the contribution, or that you otherwise have the right to submit it
under this project's license. A pull request containing commits that are not
signed off will not be accepted.

Sign off by passing `--signoff` (or `-s`) when you commit:

```bash
git commit -s -m "Add cool feature."
```

That appends a trailer to the commit message:

```text
Signed-off-by: Your Name <your@email.com>
```

The trailer must match the author of the commit it appears on, so set your
identity before you start:

```bash
git config user.name "Your Name"
git config user.email "your@email.com"
```

A trailer that does not match the commit author is the most common reason
sign-off is rejected. It usually means the commit was authored under a
different name or address than the one configured now.

If you forget the flag, `git commit --amend -s` fixes the most recent commit and
`git rebase --signoff main` fixes every commit on your branch. Both rewrite
history, so force-push the branch in your fork afterward.

This requirement is `--signoff`, which records the certification below. It is
unrelated to `--gpg-sign` (`-S`), which cryptographically signs a commit and is
a separate, optional practice.

Full text of the [Developer Certificate of Origin](https://developercertificate.org/):

```text
Developer Certificate of Origin
Version 1.1

Copyright (C) 2004, 2006 The Linux Foundation and its contributors.

Everyone is permitted to copy and distribute verbatim copies of this
license document, but changing it is not allowed.


Developer's Certificate of Origin 1.1

By making a contribution to this project, I certify that:

(a) The contribution was created in whole or in part by me and I
    have the right to submit it under the open source license
    indicated in the file; or

(b) The contribution is based upon previous work that, to the best
    of my knowledge, is covered under an appropriate open source
    license and I have the right under that license to submit that
    work with modifications, whether created in whole or in part
    by me, under the same open source license (unless I am
    permitted to submit under a different license), as indicated
    in the file; or

(c) The contribution was provided directly to me by some other
    person who certified (a), (b) or (c) and I have not modified
    it.

(d) I understand and agree that this project and the contribution
    are public and that a record of the contribution (including all
    personal information I submit with it, including my sign-off) is
    maintained indefinitely and may be redistributed consistent with
    this project or the open source license(s) involved.
```

## Contribution Terms

Do not submit code, documentation, or other material that you do not have the
right to contribute. Your sign-off on each commit is how you certify that you
have that right.

Contributions accepted into this repository are licensed under the terms in
[LICENSE](LICENSE) (Apache License 2.0). This project uses the Developer
Certificate of Origin above. It requires no contributor license agreement.
