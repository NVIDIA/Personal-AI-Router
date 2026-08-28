<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Versioning

`services/versions.json` is the single source of truth for service version
numbers. Build scripts read it and stamp every binary at build time via Go's
`-ldflags "-X main.Version=..."`. Nothing else hardcodes a component version.

```
services/versions.json
├── product            # umbrella / product release
├── installer          # always equals product
└── components.*       # per-binary versions (independent SemVer)
```

Product release notes are maintained outside this tree. Do not add or edit
`services/changelog.md`.

`desktop/package.json` `version` is **out of scope** for automated service
bumps. Bump it manually when cutting an Electron / update-feed release.

`product` / `installer` follow the product release series (for example
`0.82.0`), not a separate services-only major line.

## Bumping rules (SemVer meaning)

We follow [SemVer](https://semver.org/) (`MAJOR.MINOR.PATCH`).

### Component versions (`components.*`)

| Change | Component bump |
| ------ | -------------- |
| Source-only formatting, comments, dead-code removal — byte-identical compiled output | none |
| Bug fix, internal refactor, log message | PATCH |
| New feature visible over IPC/HTTP, additive | MINOR |
| Breaking IPC/HTTP change (rename, removal) | MAJOR |

Ask: would a user reading `--version` learn something useful? If not, leave it
`none`.

### Product version (`product`)

| Change | Product bump |
| ------ | ------------ |
| No product-facing release | none |
| Only PATCH-level notes / component bumps | PATCH |
| At least one MINOR component bump, or user-visible product change | MINOR |
| At least one MAJOR component bump, or breaking UX/data change | MAJOR |

`installer` always equals `product` after a release apply.

## Declaring a version change

Update `versions.json` in the same pull request that changes compiled output,
using the tables above to pick the severity. Say in the pull-request description
which components you bumped and why, and describe any user-facing change in plain
terms so it can be carried into the release notes. A reviewer should be able to
see the version decision without inferring it from the diff.

## Verifying

Every binary supports `--version`:

```powershell
.\build\bin\ollama-proxy.exe --version
```

```bash
./build/bin/ollama-proxy --version
```

A binary built outside `build.bat` / `build.sh` reports `dev` by design.
