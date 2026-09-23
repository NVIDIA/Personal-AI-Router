#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Parse and validate PAIR pull request release-intent blocks (v1).

Three numbers, each with one job:

- the **release** version in `desktop/package.json`, which is what users
  install. It is never declared in an intent block: it patch-bumps
  automatically whenever an intent declares a release, and a minor or major
  release is a deliberate manual bump at cut time.
- **services** in `services/versions.json`, the services suite version that
  stamps the standalone installer and Go `main.Version`.
- **components.\\*** in `services/versions.json`, per-binary SemVer.

Only the last two are declared, because only they carry SemVer meaning that a
human has to judge.
"""

from __future__ import annotations

import json
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Literal

BumpKind = Literal['none', 'patch', 'minor', 'major']
BUMP_KINDS: tuple[BumpKind, ...] = ('none', 'patch', 'minor', 'major')
BUMP_RANK: dict[BumpKind, int] = {'none': 0, 'patch': 1, 'minor': 2, 'major': 3}

# How hard to be about the block's key set not matching versions.json.
#
# - 'strict'         — any mismatch fails. Used on pull requests, where a typo
#                      should be corrected rather than silently absorbed.
# - 'reject-unknown' — an unknown key fails; a missing key warns and is treated
#                      as none. Used when applying. An unknown key means the
#                      block was edited to something a pull request check would
#                      have rejected, which is worth failing on. A *missing* key
#                      is the legitimate case of a concurrent pull request having
#                      added a component since this one was written, and reading
#                      it as none is what its author meant.
# - 'lenient'        — both warn. Local inspection only.
KeyPolicy = Literal['strict', 'reject-unknown', 'lenient']

INTENT_VERSION = 'v1'
# Canonical forms (template / docs). Parsers also accept HTML comments with
# optional internal whitespace — GitLab often collapses `<!-- foo -->` to
# `<!--foo-->` when the description is saved or re-rendered.
INTENT_START = f'<!-- pair-release-intent:{INTENT_VERSION} -->'
INTENT_END = f'<!-- /pair-release-intent:{INTENT_VERSION} -->'
ALLOW_OWNED_FILES_MARKER = '<!-- pair-release-intent-allow-owned-files -->'
_INTENT_START_RE = re.compile(
    rf'<!--\s*pair-release-intent:{re.escape(INTENT_VERSION)}\s*-->'
)
_INTENT_END_RE = re.compile(
    rf'<!--\s*/pair-release-intent:{re.escape(INTENT_VERSION)}\s*-->'
)
_ALLOW_OWNED_FILES_RE = re.compile(
    r'<!--\s*pair-release-intent-allow-owned-files\s*-->'
)

BOT_COMMIT_PREFIX = '[release-intent]'
APPLIES_PR_TRAILER_PREFIX = 'Applies-PR: #'

# Path-granular, and check_forbidden_paths rejects any edit to a listed file.
# desktop/package.json is deliberately absent even though the bot writes its
# `version`: dependency work touches that file constantly, so listing it would
# block ordinary pull requests.
BOT_OWNED_MODIFY_PATHS = (
    'services/versions.json',
    'CHANGELOG.md',
)
SERVICES_CHANGELOG_PATH = 'services/changelog.md'

REPO_ROOT = Path(__file__).resolve().parents[2]
VERSIONS_PATH = REPO_ROOT / 'services' / 'versions.json'
CHANGELOG_PATH = REPO_ROOT / 'CHANGELOG.md'
PACKAGE_JSON_PATH = REPO_ROOT / 'desktop' / 'package.json'

VERSIONS_COMMENT = (
    'Single source of truth for all version numbers. See VERSIONING.md for bump rules.'
)


@dataclass(frozen=True)
class ReleaseIntent:
    changelog_title: str
    changelog_body: str
    bumps: dict[str, BumpKind]

    @property
    def has_release(self) -> bool:
        return any(kind != 'none' for kind in self.bumps.values())


@dataclass(frozen=True)
class VersionsManifest:
    services: str
    components: dict[str, str]

    @property
    def bump_keys(self) -> list[str]:
        return ['services', *sorted(self.components)]


def parse_versions(text: str, source: str) -> tuple[VersionsManifest, dict[str, Any]]:
    """Parse a versions manifest. `source` names it in errors."""
    raw_obj: Any = json.loads(text)
    if not isinstance(raw_obj, dict):
        raise ValueError(f'{source}: top-level value must be a JSON object')
    raw: dict[str, Any] = raw_obj
    components_raw = raw.get('components')
    if not isinstance(components_raw, dict) or not components_raw:
        raise ValueError(f'{source}: missing non-empty components object')
    components: dict[str, str] = {}
    for key, value in components_raw.items():
        if not isinstance(key, str) or not isinstance(value, str):
            raise ValueError(f'{source}: component entries must be string→string')
        components[key] = value
    services = raw.get('services')
    if not isinstance(services, str) or not services:
        raise ValueError(f'{source}: services must be a non-empty string')
    return (VersionsManifest(services=services, components=components), raw)


def read_release_version(text: str, source: str) -> str:
    """The release version from a desktop/package.json body."""
    raw: Any = json.loads(text)
    if not isinstance(raw, dict):
        raise ValueError(f'{source}: top-level value must be a JSON object')
    version = raw.get('version')
    if not isinstance(version, str) or not version:
        raise ValueError(f'{source}: version must be a non-empty string')
    return version


def load_release_version(path: Path = PACKAGE_JSON_PATH) -> str:
    return read_release_version(path.read_text(encoding='utf-8'), str(path))


_PACKAGE_VERSION_RE = re.compile(r'^(?P<lead>\s*"version"\s*:\s*")[^"]*(?P<tail>")', re.M)


def render_package_json(text: str, version: str) -> str:
    """Replace only the version value, leaving the rest byte-for-byte intact.

    desktop/package.json is hand-maintained, so it is rewritten surgically
    rather than re-serialized: a json.dumps round trip would reformat the whole
    file and bury the one-line bump in an unreviewable diff.
    """
    replaced, count = _PACKAGE_VERSION_RE.subn(
        lambda m: f'{m.group("lead")}{version}{m.group("tail")}', text, count=1
    )
    if count != 1:
        raise ValueError('Could not locate a single "version" field in package.json')
    return replaced


def load_versions(path: Path = VERSIONS_PATH) -> tuple[VersionsManifest, dict[str, Any]]:
    return parse_versions(path.read_text(encoding='utf-8'), str(path))


def extract_intent_block(description: str) -> str:
    start_match = _INTENT_START_RE.search(description)
    end_match = _INTENT_END_RE.search(description)
    if (
        start_match is None
        or end_match is None
        or end_match.start() <= start_match.start()
    ):
        raise ValueError(
            'Pull request body is missing the exact release-intent fences:\n'
            f'  {INTENT_START}\n'
            f'  ...\n'
            f'  {INTENT_END}\n'
            'Copy them from the pull request template and fill them in.'
        )
    return description[start_match.end() : end_match.start()]


def _parse_section(inner: str, heading: str) -> str:
    pattern = rf'(?m)^### {re.escape(heading)}\s*\n(.*?)(?=^### |\Z)'
    match = re.search(pattern, inner, flags=re.S)
    if not match:
        raise ValueError(f'Missing required section heading: ### {heading}')
    kept = [
        line
        for line in match.group(1).splitlines()
        if not _is_ignorable_line(line.strip())
    ]
    return '\n'.join(kept).strip()


def _parse_bump_kind(key: str, value: str) -> BumpKind:
    for kind in BUMP_KINDS:
        if value == kind:
            return kind
    raise ValueError(f'{key}: expected one of {", ".join(BUMP_KINDS)}, got {value!r}')


def _is_ignorable_line(line: str) -> bool:
    return line.startswith('#') or (line.startswith('<!--') and line.endswith('-->'))


def _strip_bump_line(line: str) -> str:
    if line.startswith(('- ', '* ')):
        return line[2:].strip()
    return line


def _parse_bumps(
    section: str,
    expected_keys: list[str],
    *,
    key_policy: KeyPolicy,
) -> dict[str, BumpKind]:
    lines = [
        _strip_bump_line(line.strip())
        for line in section.splitlines()
        if line.strip() and not _is_ignorable_line(line.strip())
    ]
    if not lines:
        raise ValueError('### Bumps section is empty')

    parsed: dict[str, BumpKind] = {}
    for line in lines:
        if ':' not in line:
            raise ValueError(
                f'Bump line must be "- key: none|patch|minor|major", got: {line!r}'
            )
        key, value = line.split(':', 1)
        key = key.strip()
        value = value.strip().lower()
        if key in parsed:
            raise ValueError(f'Duplicate bump key: {key}')
        kind = _parse_bump_kind(key, value)
        parsed[key] = kind

    expected = set(expected_keys)
    got = set(parsed)
    missing = sorted(expected - got)
    extra = sorted(got - expected)
    if missing or extra:
        fatal_missing = missing if key_policy == 'strict' else []
        fatal_extra = extra if key_policy in ('strict', 'reject-unknown') else []
        if fatal_missing or fatal_extra:
            parts: list[str] = []
            if fatal_missing:
                parts.append('missing keys: ' + ', '.join(fatal_missing))
            if fatal_extra:
                parts.append('unknown keys: ' + ', '.join(fatal_extra))
            raise ValueError(
                'Bump keys must match services/versions.json exactly ('
                + '; '.join(parts)
                + ')'
            )
        for key in missing:
            print(
                f'warning: {key} absent from the intent block; treating as none',
                flush=True,
            )
        for key in extra:
            print(
                f'warning: ignoring unknown bump key {key} (not in versions.json)',
                flush=True,
            )
        return {key: parsed.get(key, 'none') for key in expected_keys}
    return {key: parsed[key] for key in expected_keys}


def _is_na(text: str) -> bool:
    return text.strip().lower() == 'n/a'


def parse_release_intent(
    description: str,
    expected_bump_keys: list[str],
    *,
    key_policy: KeyPolicy = 'strict',
) -> ReleaseIntent:
    inner = extract_intent_block(description)
    title = _parse_section(inner, 'Changelog title')
    body = _parse_section(inner, 'Changelog body')
    bumps = _parse_bumps(
        _parse_section(inner, 'Bumps'),
        expected_bump_keys,
        key_policy=key_policy,
    )

    if not title:
        raise ValueError('Changelog title must be non-empty (use n/a when there is no release)')
    if not body:
        raise ValueError('Changelog body must be non-empty (use n/a when there is no release)')

    component_keys = [key for key in expected_bump_keys if key != 'services']
    max_component = max((BUMP_RANK[bumps[key]] for key in component_keys), default=0)
    services_rank = BUMP_RANK[bumps['services']]

    if max_component > 0 and services_rank == 0:
        raise ValueError(
            'services must be patch|minor|major when any component bump is not none'
        )
    if services_rank < max_component:
        raise ValueError(
            'services bump severity must be >= the highest component bump '
            f'(services={bumps["services"]}, highest component rank requires >= '
            f'{next(k for k, r in BUMP_RANK.items() if r == max_component)})'
        )

    has_release = services_rank > 0 or max_component > 0
    if has_release:
        if _is_na(title) or _is_na(body):
            raise ValueError(
                'Changelog title and body are required when any bump is not none '
                '(do not use n/a)'
            )
    else:
        if not _is_na(title) or not _is_na(body):
            raise ValueError(
                'When every bump is none, Changelog title and body must both be exactly n/a'
            )

    return ReleaseIntent(changelog_title=title, changelog_body=body, bumps=bumps)


def allows_owned_file_edits(description: str) -> bool:
    return _ALLOW_OWNED_FILES_RE.search(description) is not None


def bump_semver(version: str, kind: BumpKind) -> str:
    if kind == 'none':
        return version
    parts = version.split('.')
    if len(parts) != 3 or not all(part.isdigit() for part in parts):
        raise ValueError(f'Version must be MAJOR.MINOR.PATCH, got {version!r}')
    major, minor, patch = (int(parts[0]), int(parts[1]), int(parts[2]))
    if kind == 'major':
        return f'{major + 1}.0.0'
    if kind == 'minor':
        return f'{major}.{minor + 1}.0'
    return f'{major}.{minor}.{patch + 1}'


def apply_bumps(manifest: VersionsManifest, intent: ReleaseIntent) -> VersionsManifest:
    services = bump_semver(manifest.services, intent.bumps['services'])
    components = {
        name: bump_semver(version, intent.bumps[name])
        for name, version in manifest.components.items()
    }
    return VersionsManifest(services=services, components=components)


def render_versions_json(manifest: VersionsManifest, original: dict[str, Any]) -> str:
    doc = dict(original)
    doc.pop('product', None)
    doc.pop('installer', None)
    doc['$comment'] = VERSIONS_COMMENT
    doc['services'] = manifest.services
    doc['components'] = dict(manifest.components)
    return json.dumps(doc, indent=2, ensure_ascii=False) + '\n'


def format_changelog_entry(
    release_version: str, title: str, body: str, pr_ref: str
) -> str:
    """One changelog section. `pr_ref` is cited so an entry traces to its PR."""
    bullets = '\n'.join(
        line
        if line.lstrip().startswith(('-', '*'))
        else f'- {line}'
        for line in body.strip().splitlines()
        if line.strip()
    )
    suffix = f' (#{pr_ref})' if pr_ref else ''
    return f'## {release_version} — {title.strip()}{suffix}\n\n{bullets}\n'


def prepend_changelog(existing: str, entry: str) -> str:
    match = re.search(r'(?m)^## ', existing)
    if not match:
        raise ValueError('CHANGELOG.md has no ## release headings to prepend before')
    insert_at = match.start()
    return existing[:insert_at] + entry + '\n' + existing[insert_at:]


def applies_pr_trailer(pr_number: str) -> str:
    return f'{APPLIES_PR_TRAILER_PREFIX}{pr_number}'


def check_forbidden_paths(changed_name_status: list[tuple[str, str]], description: str) -> None:
    if allows_owned_file_edits(description):
        return

    errors: list[str] = []
    for status, path in changed_name_status:
        status = status.upper()
        if path in BOT_OWNED_MODIFY_PATHS and status != 'D':
            errors.append(
                f'{path} is bot-owned; do not modify it in a pull request '
                f'(declare bumps in the release-intent block instead)'
            )
        if path == SERVICES_CHANGELOG_PATH and status != 'D':
            errors.append(
                f'{path} is retired; delete it if present, do not add or edit it. '
                'Product notes go in root CHANGELOG.md via the release-intent bot.'
            )
    if errors:
        raise ValueError('Forbidden path changes:\n- ' + '\n- '.join(errors))
