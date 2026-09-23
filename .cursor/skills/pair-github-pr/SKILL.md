---
name: pair-github-pr
description: >-
  Fills GitHub pull request descriptions with the required PAIR
  pair-release-intent:v1 block so the release-intent check passes. Use when
  opening, updating, or rewriting a PR description, when the user mentions
  release intent, version bumps, changelog for a PR, or when a workflow run
  fails on release-intent-check.
---
<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# PAIR pull request release intent

`.github/workflows/release-intent-check.yml` runs on every pull request. A
missing or malformed intent block **fails the check**. Agents that create or
edit PR descriptions must include a valid block — do not invent a free-form
changelog section.

Canonical template (copy keys from here; keep the key set in sync with
`services/versions.json`):

`.github/PULL_REQUEST_TEMPLATE.md`

SemVer meaning: `services/VERSIONING.md`. Operational detail (truncation,
component add/remove): `scripts/release-intent/README.md`.

## When opening or updating a pull request

1. Push a **feature branch** only (never `develop` or `main` — see
   `no-push-to-main.mdc`).
2. Build the description from the template structure: **PAIR release intent
   fence first** (truncation-safe), then Summary → Test plan.
3. Decide release vs no-release (below).
4. **Do not** edit `services/versions.json` in the pull request unless you also
   add `<!-- pair-release-intent-allow-owned-files -->` (bootstrap/hotfix, or
   component add/remove). `desktop/package.json` `version` is a manual
   release-cut carve-out and is not part of this block.
5. Before pushing the description, validate locally:

```bash
# Write the PR body to a file, then:
python3 scripts/release-intent/validate_pr.py \
  --description-file /tmp/pr-description.md \
  --skip-owned-files-check
```

In the workflow run, drop `--skip-owned-files-check` (CI enforces owned paths).

## No release (most pull requests)

Every bump value is `none`. Changelog title and body are exactly `n/a`.

Use a Markdown bullet list for bumps (`- key: value`) so the rendered pull
request stays one key per line without a code fence.

```markdown
<!-- pair-release-intent:v1 -->
### Changelog title
n/a

### Changelog body
n/a

### Bumps
- product: none
- nvpair-cluster-manager: none
- nvpair-engine-manager: none
- nvpair-errors: none
- nvpair-job-scheduler: none
- nvpair-manual-nodes: none
- nvpair-node-info: none
- nvpair-node-scanner: none
- nvpair-node-settings: none
- nvpair-proxy: none
- nvpair-tui: none
- nvpair-ui-broker: none
- nvpair-workload-manager: none
<!-- /pair-release-intent:v1 -->
```

If `services/versions.json` gained or lost a component, edit that file in the
pull request with `<!-- pair-release-intent-allow-owned-files -->`, and
regenerate the bump key list (`product` + sorted component names). Extra or
missing keys fail the check.

## Shipping a product release

1. Set each touched component to `patch`, `minor`, or `major` (SemVer meaning
   in `VERSIONING.md`). Untouched components stay `none`.
2. Set `product` to a severity **≥** the highest component bump. UI-only
   product notes may bump `product` while all components stay `none`.
3. Replace changelog title/body with real **user-facing** prose (not `n/a`).
   Prefer bullets in the body.
4. Leave owned files alone — `release-intent-apply.yml` writes them after the
   pull request merges.

Example (engine-manager feature + product minor):

```markdown
<!-- pair-release-intent:v1 -->
### Changelog title
Model downloads show live progress

### Changelog body
- Pulling a model on this machine now shows a live percentage instead of a spinner.

### Bumps
- product: minor
- nvpair-cluster-manager: none
- nvpair-engine-manager: minor
- nvpair-errors: none
- nvpair-job-scheduler: none
- nvpair-manual-nodes: none
- nvpair-node-info: none
- nvpair-node-scanner: none
- nvpair-node-settings: none
- nvpair-proxy: none
- nvpair-tui: none
- nvpair-ui-broker: none
- nvpair-workload-manager: none
<!-- /pair-release-intent:v1 -->
```

## Hard format rules (the check fails otherwise)

- Exact fences: `<!-- pair-release-intent:v1 -->` … `<!-- /pair-release-intent:v1 -->`
- Exact headings: `### Changelog title`, `### Changelog body`, `### Bumps`
- Enums only: `none` | `patch` | `minor` | `major` (lowercase)
- One bullet per key: `- key: value` (plain `key: value` still accepted)
- HTML/`#` comment lines inside the intent block are allowed and ignored
- Any non-`none` bump ⇒ real changelog (not `n/a`)
- All bumps `none` ⇒ both changelog fields exactly `n/a`

## Fixing a failed release-intent check

1. Read the workflow log (it prints the parse error).
2. Edit the **pull request description** (not a new commit) unless owned files
   were wrongly changed — then revert those file edits on the branch.
3. Re-run the workflow if it does not re-run on description-only edits.

## Out of scope

- Do not put internal issue identifiers or private tracker URLs in the
  changelog body. Describe user-visible behavior instead.
