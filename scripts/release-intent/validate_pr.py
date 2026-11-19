#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Validate a pull request body against the PAIR release-intent contract."""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from lib import (  # noqa: E402
    INTENT_END,
    INTENT_START,
    REPO_ROOT,
    check_forbidden_paths,
    load_versions,
    parse_release_intent,
)


def pull_request_body() -> str:
    """The pull request body, as the workflow passed it in.

    The workflow sets PR_BODY from github.event.pull_request.body through `env:`
    rather than interpolating it into a shell command, because the body is
    author-controlled and would otherwise be a shell injection.

    There is no API fallback: the webhook payload carries the whole body, so
    unlike GitLab's CI_MERGE_REQUEST_DESCRIPTION there is nothing to un-truncate
    and no token needed to read it. That is also why this script needs no
    secret, which is what lets it run on pull requests from forks.
    """
    body = os.environ.get('PR_BODY')
    if body is None:
        raise SystemExit(
            'PR_BODY is not set. In CI the workflow must pass it via `env:` from '
            'github.event.pull_request.body. Locally, use --description-file.'
        )
    return body


def changed_name_status(base: str, head: str) -> list[tuple[str, str]]:
    result = subprocess.run(
        ['git', 'diff', '--name-status', f'{base}...{head}'],
        cwd=REPO_ROOT,
        check=True,
        capture_output=True,
        text=True,
    )
    rows: list[tuple[str, str]] = []
    for line in result.stdout.splitlines():
        if not line.strip():
            continue
        parts = line.split('\t')
        if len(parts) == 2:
            rows.append((parts[0], parts[1]))
        elif len(parts) == 3 and parts[0].startswith('R'):
            rows.append((parts[0], parts[2]))
        else:
            raise SystemExit(f'Unrecognized git name-status line: {line!r}')
    return rows


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        '--description-file',
        type=Path,
        help='Read the body from a file instead of the PR_BODY environment variable',
    )
    parser.add_argument(
        '--skip-owned-files-check',
        action='store_true',
        help='Skip bot-owned path enforcement (local testing only)',
    )
    args = parser.parse_args()

    if args.description_file is not None:
        description = args.description_file.read_text(encoding='utf-8')
    else:
        description = pull_request_body()

    try:
        versions, _raw = load_versions()
    except ValueError as exc:
        # A traceback here reads as a script crash when it is really a malformed
        # manifest, which is the likely state mid-schema-change.
        print(f'services/versions.json could not be read: {exc}', file=sys.stderr)
        return 1

    try:
        intent = parse_release_intent(
            description, versions.bump_keys, key_policy='strict'
        )
        if not args.skip_owned_files_check:
            base = os.environ.get('PR_BASE_SHA')
            head = os.environ.get('PR_HEAD_SHA')
            if base and head:
                check_forbidden_paths(changed_name_status(base, head), description)
            elif os.environ.get('GITHUB_EVENT_NAME') == 'pull_request':
                # The merge-base diff is the only way to catch a hand edit to a
                # bot-owned file, so a pull request that cannot compute it must
                # fail rather than pass the check vacuously. Needs
                # actions/checkout with fetch-depth: 0.
                raise SystemExit(
                    'PR_BASE_SHA and PR_HEAD_SHA are required to enforce '
                    'bot-owned file rules on a pull request'
                )
    except ValueError as exc:
        print('release-intent validation failed:', file=sys.stderr)
        print(str(exc), file=sys.stderr)
        print(file=sys.stderr)
        print('Expected fences:', file=sys.stderr)
        print(f'  {INTENT_START}', file=sys.stderr)
        print(f'  {INTENT_END}', file=sys.stderr)
        return 1

    print('release-intent OK')
    print(f'  services bump: {intent.bumps["services"]}')
    for key in versions.bump_keys:
        if key == 'services':
            continue
        print(f'  {key}: {intent.bumps[key]}')
    if intent.has_release:
        print(f'  changelog title: {intent.changelog_title}')
        print('  release version: patch-bumps automatically on merge')
    else:
        print('  no version release (all bumps none)')
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
