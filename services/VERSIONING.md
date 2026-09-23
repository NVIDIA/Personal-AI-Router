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
├── services           # the services suite as a whole
└── components.*       # per-binary versions (independent SemVer)
```

Do not add or edit `services/changelog.md`.

## Three numbers, three jobs

| Number | Lives in | Stamps | Bumped |
| ------ | -------- | ------ | ------ |
| release | `desktop/package.json` `version` | The app users install, the update feed, the GitHub release tag | Automatically: one PATCH forward whenever a release-intent block declares a release |
| `services` | `services/versions.json` | The standalone services installer and Go `main.Version` | Declared in the release-intent block |
| `components.*` | `services/versions.json` | Each binary's own `--version` | Declared in the release-intent block |

Only the last two are declared, because only they carry SemVer meaning a human
has to judge. The release version is a counter: it answers "which build is
this?", not "how compatible is it?".

A MINOR or MAJOR release version is a deliberate manual edit at cut time. The
automation only ever moves it one PATCH forward, so it cannot promote a release
on its own.

The release version and `services` are **not** held equal, and no attempt is
made to align them. They version different artifacts.

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

### Services suite version (`services`)

| Change | `services` bump |
| ------ | --------------- |
| No release | none |
| Only PATCH-level component bumps | PATCH |
| At least one MINOR component bump, or user-visible change | MINOR |
| At least one MAJOR component bump, or breaking UX/data change | MAJOR |

`services` must be at least as severe as the highest component bump; CI rejects
a block where it is not.

## Declaring a version change

Declare bumps in the `pair-release-intent:v1` block in your pull request
description, using the tables above to pick the severity. **Do not edit
`services/versions.json` or `CHANGELOG.md` by hand** — they are written by
automation, and CI rejects a pull request that modifies them.

Describe any user-facing change in the block's changelog title and body in plain
terms; that text becomes the changelog entry verbatim, cited back to your pull
request number. A reviewer should be able to see the version decision without
inferring it from the diff.

## Verifying

Every binary supports `--version`:

```powershell
.\build\bin\nvpair-proxy.exe --version
```

```bash
./build/bin/nvpair-proxy --version
```

A binary built outside `build.bat` / `build.sh` reports `dev` by design.
