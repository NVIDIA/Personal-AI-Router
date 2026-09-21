# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# Release-intent automation

Contributors declare version bumps and changelog text in the
`pair-release-intent:v1` block in a pull request description. CI validates the
block on the pull request, and after the merge lands on `develop` a bot applies
it to the version files.

SemVer meaning for each number lives in [`services/VERSIONING.md`](../../services/VERSIONING.md).

## Scripts

| Script | When | Needs a credential |
| ------ | ---- | ------------------ |
| `validate_pr.py` | Pull requests (`release-intent-check.yml`) | No |
| `apply_pr.py` | Push to `develop` (`release-intent-apply.yml`) | Yes |
| `lib.py` | Shared parse / bump / render | — |
| `test_lib.py` | Offline unit checks, also run in CI | — |

`validate_pr.py` needs no credential on purpose: the body arrives in the webhook
payload, so there is nothing to fetch. That is what lets it run on pull requests
from forks like every other check.

The check has its own workflow rather than living in `ci.yml` so it can also
trigger on the `edited` pull request type. A body can change after checks go
green, and apply reads the live body at merge time; validating only on the
default types would leave a window where the applied bumps are not the ones CI
enforced. Putting `edited` on `ci.yml` would rebuild six installers for a typo
fix in a description.

## Key policy

How hard each entry point is about the block's key set not matching
`versions.json`:

| Caller | Policy | Unknown key | Missing key |
| ------ | ------ | ----------- | ----------- |
| `validate_pr.py` | `strict` | fails | fails |
| `apply_pr.py` | `reject-unknown` | fails | warns, treated as `none` |

Apply rejects an unknown key because that means the block was edited into a
state a pull request check would have refused. It tolerates a missing key
because that is the legitimate case of a concurrent pull request adding a
component after this one was written, and reading it as `none` is what the
author meant. Full strictness there would fail the apply job on `develop` for
an ordering accident nobody did wrong.

## What apply writes

Three files, in **one** commit built through the git data API:

- `desktop/package.json` — the release version, one PATCH forward
- `services/versions.json` — the declared `services` and component bumps
- `CHANGELOG.md` — a new section titled with the new release version, citing
  the pull request number

The contents API would be one commit per file, which means a release landing in
pieces and a window where the changelog names a version `package.json` does not
yet carry. Moving the ref once avoids that, and a rejected fast-forward is the
concurrency check — the script retries that and only that, up to three times.

## Known limits

**One apply per push.** `apply_pr.py` resolves the pull request for the push's
head commit only. A push carrying two merges — a direct push of a range, or a
merge-queue batch — applies the head commit's intent and **silently skips the
others**. Nothing fails. `develop` must therefore take one merge per push: no
merge queue batching, and no pushing a range of merge commits directly. Lifting
this means iterating the push's commits rather than reading only the head.

**No concurrency group on the apply workflow, on purpose.** GitHub keeps a
single pending run per group and cancels any earlier one, so under a burst of
merges a group would drop bumps rather than serialize them. Ordering is handled
in the script instead, by committing with `force: false` and re-reading the ref
when the branch moved. Do not add one.

**`### ` inside a changelog body truncates it.** `_parse_section` ends a section
at the next line beginning with `### `, so a body containing a literal `### `
line is silently cut at that point. Keep changelog bodies to prose and bullets.

**The gate assumes branch protection.** `develop` should require the
`Release intent check` status check. An admin merge that bypasses it with an
invalid block fails at apply with exit 1, loudly, but the versions simply do not
bump until someone fixes the body and re-runs.

## Idempotency

Each bot commit carries an `Applies-PR: #N` trailer, and every attempt scans the
branch's recent commits for it first, so a re-run after a successful apply is a
no-op. A commit whose message already starts with `[release-intent]` is skipped
outright, so the bot never reacts to itself.

## Credential

`apply_pr.py` authenticates with a GitHub App installation token, minted per run
and expiring in an hour.

- **Grant `contents: write` and nothing else.** In particular do **not** grant
  the Workflows permission: without it the token cannot modify
  `.github/workflows`, so a compromise of this job cannot rewrite the pipeline.
- Add the app as a bypass actor on `develop`, the only branch it writes.
- Configure `RELEASE_INTENT_APP_ID` as a repository variable and
  `RELEASE_INTENT_APP_PRIVATE_KEY` as a secret.

**The app and both settings must exist before the first push to `develop`.**
If the credential is missing, `apply_pr.py` exits 2 and says so rather than
silently skipping the bump. Backfill by re-running the job once it is configured,
or apply from a saved body locally.

## Bot-owned files

`services/versions.json` and `CHANGELOG.md` are written only by the bot;
`validate_pr.py` rejects a pull request that edits them. Override with
`<!-- pair-release-intent-allow-owned-files -->` in the description when adding
or removing a component key, which the bot cannot invent.

`desktop/package.json` is deliberately **not** on that list even though the bot
writes its `version`: dependency work touches the file constantly, and the check
is path-granular, so listing it would block ordinary pull requests.

## Local checks

```bash
python3 scripts/release-intent/test_lib.py

python3 scripts/release-intent/validate_pr.py \
  --description-file /path/to/body.md \
  --skip-owned-files-check

python3 scripts/release-intent/apply_pr.py \
  --description-file /path/to/body.md \
  --dry-run
```

`--dry-run` **writes the working tree** despite the name. Restore afterwards:

```bash
git restore desktop/package.json services/versions.json CHANGELOG.md
```
