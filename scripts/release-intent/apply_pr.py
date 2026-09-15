#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Apply a merged pull request's release-intent block to the version files.

Writes three files in ONE commit via the git data API: desktop/package.json
(the release version, patch-bumped), services/versions.json (declared services
and component bumps), and CHANGELOG.md (a new section).

The contents API would be one commit per file, which for three files means a
release landing in pieces and a window where the changelog names a version that
package.json does not yet carry. Building a tree and moving the ref once avoids
that, and the ref update doubles as the concurrency check: a non-fast-forward
means the branch moved underneath us, which is the only condition worth
retrying.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from lib import (  # noqa: E402
    BOT_COMMIT_PREFIX,
    CHANGELOG_PATH,
    PACKAGE_JSON_PATH,
    REPO_ROOT,
    VERSIONS_PATH,
    apply_bumps,
    applies_pr_trailer,
    bump_semver,
    format_changelog_entry,
    load_versions,
    parse_release_intent,
    parse_versions,
    prepend_changelog,
    read_release_version,
    render_package_json,
    render_versions_json,
)

VERSIONS_REPO_PATH = str(VERSIONS_PATH.relative_to(REPO_ROOT))
CHANGELOG_REPO_PATH = str(CHANGELOG_PATH.relative_to(REPO_ROOT))
PACKAGE_REPO_PATH = str(PACKAGE_JSON_PATH.relative_to(REPO_ROOT))

BLOB_MODE = '100644'


@dataclass(frozen=True)
class ReleaseUpdate:
    """The three file bodies a release intent produces, and what to call it."""

    package: str
    versions: str
    changelog: str
    release: str

    def as_paths(self) -> dict[str, str]:
        return {
            PACKAGE_REPO_PATH: self.package,
            VERSIONS_REPO_PATH: self.versions,
            CHANGELOG_REPO_PATH: self.changelog,
        }


def _api_base() -> str:
    return os.environ.get('GITHUB_API_URL', 'https://api.github.com').rstrip('/')


def _headers(token: str) -> dict[str, str]:
    return {
        'Authorization': f'Bearer {token}',
        'Accept': 'application/vnd.github+json',
        'X-GitHub-Api-Version': '2022-11-28',
    }


def _request(
    method: str, url: str, token: str, payload: dict[str, object] | None = None
) -> tuple[int, object]:
    data = json.dumps(payload).encode('utf-8') if payload is not None else None
    headers = _headers(token)
    if data is not None:
        headers['Content-Type'] = 'application/json'
    request = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(request, timeout=60) as response:
            body = response.read().decode('utf-8')
            return response.status, (json.loads(body) if body else {})
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode('utf-8', errors='replace')
        try:
            return exc.code, json.loads(raw)
        except json.JSONDecodeError:
            return exc.code, raw


def _get(url: str, token: str) -> object:
    status, body = _request('GET', url, token)
    if status != 200:
        raise SystemExit(f'GitHub API HTTP {status} for {url}: {body}')
    return body


def _post(url: str, token: str, payload: dict[str, object]) -> tuple[int, object]:
    return _request('POST', url, token, payload)


def current_commit_message() -> str:
    return subprocess.run(
        ['git', 'log', '-1', '--pretty=%B'],
        cwd=REPO_ROOT,
        check=True,
        capture_output=True,
        text=True,
    ).stdout


def fetch_merged_pr(repo: str, sha: str, token: str) -> tuple[str, str] | None:
    """The pull request that brought `sha` to the branch, as (number, body).

    GitHub's commits/{sha}/pulls is the direct analogue of GitLab's
    commits/{sha}/merge_requests.
    """
    url = f'{_api_base()}/repos/{repo}/commits/{urllib.parse.quote(sha)}/pulls'
    payload = _get(url, token)
    if not isinstance(payload, list) or not payload:
        return None
    chosen: dict[str, object] | None = None
    for item in payload:
        if isinstance(item, dict) and item.get('merged_at'):
            chosen = item
            break
    if chosen is None and isinstance(payload[0], dict):
        chosen = payload[0]
    if chosen is None:
        return None
    number = chosen.get('number')
    body = chosen.get('body')
    if not isinstance(number, int):
        return None
    # A pull request with an empty body has body: null.
    return (str(number), body if isinstance(body, str) else '')


def compute_release_update(
    package_text: str,
    versions_text: str,
    changelog_text: str,
    description: str,
    pr_ref: str,
) -> ReleaseUpdate | None:
    """Apply the intent to three file bodies. None when nothing changes."""
    versions, raw = parse_versions(versions_text, VERSIONS_REPO_PATH)
    intent = parse_release_intent(
        description, versions.bump_keys, key_policy='reject-unknown'
    )
    if not intent.has_release:
        return None

    updated = apply_bumps(versions, intent)
    release_before = read_release_version(package_text, PACKAGE_REPO_PATH)
    # The release version is never declared: any release is one patch forward.
    release_after = bump_semver(release_before, 'patch')

    print(f'Applying release {release_before} → {release_after}')
    if updated.services != versions.services:
        print(f'  services: {versions.services} → {updated.services}')
    for name in sorted(versions.components):
        before, after = versions.components[name], updated.components[name]
        if before != after:
            print(f'  {name}: {before} → {after}')

    entry = format_changelog_entry(
        release_after, intent.changelog_title, intent.changelog_body, pr_ref
    )
    return ReleaseUpdate(
        package=render_package_json(package_text, release_after),
        versions=render_versions_json(updated, raw),
        changelog=prepend_changelog(changelog_text, entry),
        release=release_after,
    )


def read_file_at(repo: str, token: str, path: str, ref: str) -> str:
    url = (
        f'{_api_base()}/repos/{repo}/contents/{urllib.parse.quote(path)}'
        f'?ref={urllib.parse.quote(ref)}'
    )
    payload = _get(url, token)
    if not isinstance(payload, dict):
        raise SystemExit(f'Unexpected payload reading {path} at {ref}')
    content = payload.get('content')
    if not isinstance(content, str):
        raise SystemExit(f'Incomplete payload reading {path} at {ref}')
    return base64.b64decode(content).decode('utf-8')


def branch_has_trailer(repo: str, token: str, branch: str, trailer: str) -> bool:
    url = (
        f'{_api_base()}/repos/{repo}/commits'
        f'?sha={urllib.parse.quote(branch)}&per_page=50'
    )
    payload = _get(url, token)
    if not isinstance(payload, list):
        return False
    for item in payload:
        if not isinstance(item, dict):
            continue
        commit = item.get('commit')
        if isinstance(commit, dict) and trailer in str(commit.get('message', '')):
            return True
    return False


def commit_tree(
    repo: str, token: str, branch: str, head: str, message: str, files: dict[str, str]
) -> tuple[bool, object]:
    """Commit every file at once and move the branch. Returns (stale, detail)."""
    head_commit = _get(f'{_api_base()}/repos/{repo}/git/commits/{head}', token)
    if not isinstance(head_commit, dict):
        raise SystemExit(f'Unexpected payload for commit {head}')
    tree_info = head_commit.get('tree')
    if not isinstance(tree_info, dict) or not isinstance(tree_info.get('sha'), str):
        raise SystemExit(f'Commit {head} has no tree sha')

    entries: list[dict[str, object]] = []
    for path, content in files.items():
        status, blob = _post(
            f'{_api_base()}/repos/{repo}/git/blobs',
            token,
            {'content': content, 'encoding': 'utf-8'},
        )
        if status != 201 or not isinstance(blob, dict):
            raise SystemExit(f'Could not create a blob for {path}: HTTP {status} {blob}')
        entries.append(
            {'path': path, 'mode': BLOB_MODE, 'type': 'blob', 'sha': blob.get('sha')}
        )

    status, tree = _post(
        f'{_api_base()}/repos/{repo}/git/trees',
        token,
        {'base_tree': tree_info['sha'], 'tree': entries},
    )
    if status != 201 or not isinstance(tree, dict):
        raise SystemExit(f'Could not create a tree: HTTP {status} {tree}')

    status, commit = _post(
        f'{_api_base()}/repos/{repo}/git/commits',
        token,
        {'message': message, 'tree': tree.get('sha'), 'parents': [head]},
    )
    if status != 201 or not isinstance(commit, dict):
        raise SystemExit(f'Could not create a commit: HTTP {status} {commit}')

    status, detail = _request(
        'PATCH',
        f'{_api_base()}/repos/{repo}/git/refs/heads/{urllib.parse.quote(branch)}',
        token,
        {'sha': commit.get('sha'), 'force': False},
    )
    if status == 200:
        print(f'Committed {BOT_COMMIT_PREFIX} to {branch}')
        return False, detail
    # A rejected fast-forward means the branch moved while we were building.
    if status in (409, 422):
        return True, detail
    raise SystemExit(
        f'GitHub refused the release-intent commit on {branch} with HTTP {status}: '
        f'{detail}\nCheck that the app is a bypass actor for {branch} and holds '
        'contents: write.'
    )


def apply_release(
    message: str, description: str, trailer: str, pr_ref: str, dry_run: bool
) -> None:
    if dry_run:
        update = compute_release_update(
            PACKAGE_JSON_PATH.read_text(encoding='utf-8'),
            VERSIONS_PATH.read_text(encoding='utf-8'),
            CHANGELOG_PATH.read_text(encoding='utf-8'),
            description,
            pr_ref,
        )
        if update is None:
            print('Dry-run: nothing to apply')
            return
        PACKAGE_JSON_PATH.write_text(update.package, encoding='utf-8')
        VERSIONS_PATH.write_text(update.versions, encoding='utf-8')
        CHANGELOG_PATH.write_text(update.changelog, encoding='utf-8')
        print(
            'Dry-run MODIFIED the working tree (package.json, versions.json, '
            'CHANGELOG.md). Restore with: git restore '
            f'{PACKAGE_REPO_PATH} {VERSIONS_REPO_PATH} {CHANGELOG_REPO_PATH}'
        )
        print(message)
        return

    token = os.environ.get('RELEASE_INTENT_TOKEN')
    repo = os.environ.get('GITHUB_REPOSITORY')
    branch = os.environ.get('GITHUB_REF_NAME')
    if not token:
        raise SystemExit('RELEASE_INTENT_TOKEN is required to apply release-intent')
    if not repo or not branch:
        raise SystemExit('GITHUB_REPOSITORY and GITHUB_REF_NAME are required to apply')

    for attempt in range(1, 4):
        if trailer and branch_has_trailer(repo, token, branch, trailer):
            print(f'{trailer} is already on {branch}; nothing to do')
            return

        ref = _get(
            f'{_api_base()}/repos/{repo}/git/ref/heads/{urllib.parse.quote(branch)}',
            token,
        )
        if not isinstance(ref, dict) or not isinstance(ref.get('object'), dict):
            raise SystemExit(f'Could not read the {branch} ref')
        head = ref['object'].get('sha')
        if not isinstance(head, str):
            raise SystemExit(f'Could not read the {branch} head sha')

        update = compute_release_update(
            read_file_at(repo, token, PACKAGE_REPO_PATH, head),
            read_file_at(repo, token, VERSIONS_REPO_PATH, head),
            read_file_at(repo, token, CHANGELOG_REPO_PATH, head),
            description,
            pr_ref,
        )
        if update is None:
            print(f'Already applied on current {branch}; nothing to do')
            return

        stale, detail = commit_tree(
            repo, token, branch, head, message, update.as_paths()
        )
        if not stale:
            return
        print(f'{branch} advanced during attempt {attempt}; retrying\n{detail}')

    raise SystemExit('Could not apply release intent after 3 attempts')


def main() -> int:
    # Without this, buffered progress output reaches the job log after the
    # failure written to stderr, which reads as though it happened first.
    sys.stdout.reconfigure(line_buffering=True)

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--description-file', type=Path)
    parser.add_argument('--dry-run', action='store_true')
    parser.add_argument(
        '--sha',
        default=os.environ.get('GITHUB_SHA', ''),
        help='Commit SHA whose merged pull request body should be applied',
    )
    args = parser.parse_args()

    message = current_commit_message()
    if message.lstrip().startswith(BOT_COMMIT_PREFIX):
        print(f'Skipping: commit already starts with {BOT_COMMIT_PREFIX}')
        return 0

    if args.description_file is not None:
        description = args.description_file.read_text(encoding='utf-8')
        pr_ref = ''
    else:
        token = os.environ.get('RELEASE_INTENT_TOKEN')
        repo = os.environ.get('GITHUB_REPOSITORY')
        if not token or not repo:
            print(
                'RELEASE_INTENT_TOKEN is not configured, so this release intent '
                'was NOT applied. Record the merged pull request body for '
                'backfill (see scripts/release-intent/README.md).',
                file=sys.stderr,
            )
            return 2
        if not args.sha:
            raise SystemExit('--sha or GITHUB_SHA is required')
        found = fetch_merged_pr(repo, args.sha, token)
        if found is None:
            print('No pull request associated with this commit; nothing to apply')
            return 0
        pr_ref, description = found

    try:
        versions, _raw = load_versions()
        intent = parse_release_intent(
            description, versions.bump_keys, key_policy='reject-unknown'
        )
    except ValueError as exc:
        print('release-intent parse failed on merged pull request:', file=sys.stderr)
        print(str(exc), file=sys.stderr)
        return 1

    if not intent.has_release:
        label = f'PR #{pr_ref}' if pr_ref else 'this body'
        print(f'{label}: all bumps none — no version files to update')
        return 0

    trailer = applies_pr_trailer(pr_ref) if pr_ref else ''
    suffix = f' after PR #{pr_ref}' if pr_ref else ''
    commit_message = f'{BOT_COMMIT_PREFIX} bump release{suffix}\n\n'
    if trailer:
        commit_message += f'Apply release intent from PR #{pr_ref}.\n\n{trailer}\n'
    apply_release(commit_message, description, trailer, pr_ref, dry_run=args.dry_run)
    return 0


if __name__ == '__main__':
    raise SystemExit(main())
