#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""Offline unit checks for ci/release-intent/lib.py."""

from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from lib import (  # noqa: E402
    ALLOW_OWNED_FILES_MARKER,
    INTENT_END,
    INTENT_START,
    VERSIONS_PATH,
    VersionsManifest,
    apply_bumps,
    bump_semver,
    check_forbidden_paths,
    format_changelog_entry,
    load_versions,
    parse_release_intent,
    prepend_changelog,
    read_release_version,
    render_package_json,
    render_versions_json,
)

KEYS = [
    'services',
    'lmstudio-proxy',
    'nvpair-cluster-manager',
    'nvpair-engine-manager',
    'nvpair-errors',
    'nvpair-job-scheduler',
    'nvpair-manual-nodes',
    'nvpair-node-info',
    'nvpair-node-scanner',
    'nvpair-node-settings',
    'nvpair-tui',
    'nvpair-ui-broker',
    'nvpair-workload-manager',
    'ollama-proxy',
]


def _block(title: str, body: str, bumps: dict[str, str], *, bullets: bool = True) -> str:
    if bullets:
        bump_lines = '\n'.join(f'- {key}: {bumps[key]}' for key in KEYS)
    else:
        bump_lines = '\n'.join(f'{key}: {bumps[key]}' for key in KEYS)
    return (
        f'{INTENT_START}\n'
        f'### Changelog title\n{title}\n\n'
        f'### Changelog body\n{body}\n\n'
        f'### Bumps\n{bump_lines}\n'
        f'{INTENT_END}\n'
    )


class ReleaseVersionTests(unittest.TestCase):
    """The release version is read from desktop/package.json and patch-bumped.

    It is never declared in an intent block, so the only judgement a human makes
    is the services and component severity.
    """

    PACKAGE = '{\n    "name": "pair",\n    "version": "0.1.1",\n    "private": true\n}\n'

    def test_read_release_version(self) -> None:
        self.assertEqual(read_release_version(self.PACKAGE, 'pkg'), '0.1.1')

    def test_release_patch_bump(self) -> None:
        self.assertEqual(bump_semver('0.1.1', 'patch'), '0.1.2')
        self.assertEqual(bump_semver('0.1.9', 'patch'), '0.1.10')

    def test_render_package_json_touches_only_the_version(self) -> None:
        rendered = render_package_json(self.PACKAGE, '0.1.2')
        self.assertIn('"version": "0.1.2"', rendered)
        # Every other byte is untouched, so the diff is one line.
        self.assertEqual(
            rendered.replace('"version": "0.1.2"', '"version": "0.1.1"'),
            self.PACKAGE,
        )

    def test_render_package_json_requires_a_version_field(self) -> None:
        with self.assertRaises(ValueError):
            render_package_json('{\n    "name": "pair"\n}\n', '0.1.2')

    def test_missing_fences_rejected(self) -> None:
        with self.assertRaises(ValueError):
            parse_release_intent('## Summary\nno fences here', KEYS)


class ReleaseIntentTests(unittest.TestCase):
    def test_all_none_requires_na(self) -> None:
        bumps = {key: 'none' for key in KEYS}
        intent = parse_release_intent(_block('n/a', 'n/a', bumps), KEYS)
        self.assertFalse(intent.has_release)

    def test_all_none_rejects_prose(self) -> None:
        bumps = {key: 'none' for key in KEYS}
        with self.assertRaises(ValueError):
            parse_release_intent(_block('Oops', 'n/a', bumps), KEYS)

    def test_component_requires_services(self) -> None:
        bumps = {key: 'none' for key in KEYS}
        bumps['ollama-proxy'] = 'patch'
        with self.assertRaises(ValueError):
            parse_release_intent(_block('Title', '- body', bumps), KEYS)

    def test_services_must_dominate(self) -> None:
        bumps = {key: 'none' for key in KEYS}
        bumps['ollama-proxy'] = 'major'
        bumps['services'] = 'patch'
        with self.assertRaises(ValueError):
            parse_release_intent(_block('Title', '- body', bumps), KEYS)

    def test_release_ok(self) -> None:
        bumps = {key: 'none' for key in KEYS}
        bumps['services'] = 'minor'
        bumps['nvpair-engine-manager'] = 'minor'
        intent = parse_release_intent(
            _block('Engine load progress', '- Shows percent while pulling.', bumps),
            KEYS,
        )
        self.assertTrue(intent.has_release)
        manifest = VersionsManifest(
            services='0.82.0',
            components={key: '1.0.0' for key in KEYS if key != 'services'},
        )
        updated = apply_bumps(manifest, intent)
        self.assertEqual(updated.services, '0.83.0')
        self.assertEqual(updated.components['nvpair-engine-manager'], '1.1.0')
        self.assertEqual(updated.components['ollama-proxy'], '1.0.0')

    def test_inline_comments_are_ignored(self) -> None:
        bumps = {key: 'none' for key in KEYS}
        block = _block('n/a', 'n/a', bumps)
        block = block.replace(
            '### Bumps\n',
            '### Bumps\n<!-- Set each value to: none | patch | minor | major -->\n',
        )
        block = block.replace(
            '### Changelog title\nn/a',
            '### Changelog title\n<!-- headline here -->\nn/a',
        )
        intent = parse_release_intent(block, KEYS)
        self.assertFalse(intent.has_release)

    def test_bullet_bumps_parse(self) -> None:
        bumps = {key: 'none' for key in KEYS}
        bumps['services'] = 'patch'
        bumps['ollama-proxy'] = 'patch'
        intent = parse_release_intent(
            _block('Routing fix', '- Fixes routing.', bumps, bullets=True),
            KEYS,
        )
        self.assertTrue(intent.has_release)
        self.assertEqual(intent.bumps['ollama-proxy'], 'patch')

    def test_plain_bumps_still_parse(self) -> None:
        bumps = {key: 'none' for key in KEYS}
        intent = parse_release_intent(_block('n/a', 'n/a', bumps, bullets=False), KEYS)
        self.assertFalse(intent.has_release)

    def _with_unknown_key(self) -> str:
        bumps = {key: 'none' for key in KEYS}
        return _block('n/a', 'n/a', bumps).replace(
            'ollama-proxy: none\n',
            'ollama-proxy: none\nextra-thing: none\n',
        )

    def _without_ollama_proxy(self) -> str:
        bumps = {key: 'none' for key in KEYS if key != 'ollama-proxy'}
        bump_lines = '\n'.join(f'- {key}: {bumps[key]}' for key in bumps)
        return (
            f'{INTENT_START}\n'
            f'### Changelog title\nn/a\n\n'
            f'### Changelog body\nn/a\n\n'
            f'### Bumps\n{bump_lines}\n'
            f'{INTENT_END}\n'
        )

    def test_unknown_key_strict(self) -> None:
        with self.assertRaises(ValueError):
            parse_release_intent(self._with_unknown_key(), KEYS, key_policy='strict')

    def test_missing_key_strict(self) -> None:
        with self.assertRaises(ValueError):
            parse_release_intent(self._without_ollama_proxy(), KEYS, key_policy='strict')

    def test_unknown_key_rejected_when_applying(self) -> None:
        """An unknown key at apply time means the block was edited past CI."""
        with self.assertRaises(ValueError):
            parse_release_intent(
                self._with_unknown_key(), KEYS, key_policy='reject-unknown'
            )

    def test_missing_key_tolerated_when_applying(self) -> None:
        """A concurrent pull request can add a component after this one is written."""
        intent = parse_release_intent(
            self._without_ollama_proxy(), KEYS, key_policy='reject-unknown'
        )
        self.assertEqual(intent.bumps['ollama-proxy'], 'none')

    def test_unknown_key_lenient(self) -> None:
        intent = parse_release_intent(
            self._with_unknown_key(), KEYS, key_policy='lenient'
        )
        self.assertFalse(intent.has_release)
        self.assertNotIn('extra-thing', intent.bumps)

    def test_missing_key_lenient(self) -> None:
        intent = parse_release_intent(
            self._without_ollama_proxy(), KEYS, key_policy='lenient'
        )
        self.assertEqual(intent.bumps['ollama-proxy'], 'none')

    def test_bump_semver(self) -> None:
        self.assertEqual(bump_semver('1.2.3', 'none'), '1.2.3')
        self.assertEqual(bump_semver('1.2.3', 'patch'), '1.2.4')
        self.assertEqual(bump_semver('1.2.3', 'minor'), '1.3.0')
        self.assertEqual(bump_semver('1.2.3', 'major'), '2.0.0')

    def test_changelog_prepend(self) -> None:
        existing = '# Changelog\n\nIntro\n\n## 0.1.0 — Old\n\n- old\n'
        entry = format_changelog_entry('0.1.2', 'New thing', '- bullet\n', '123')
        out = prepend_changelog(existing, entry)
        self.assertIn('## 0.1.2 — New thing (#123)', out)
        self.assertTrue(out.index('## 0.1.2') < out.index('## 0.1.0'))

    def test_changelog_entry_without_pr_ref_has_no_suffix(self) -> None:
        entry = format_changelog_entry('0.1.2', 'Local', '- bullet\n', '')
        self.assertIn('## 0.1.2 — Local\n', entry)
        self.assertNotIn('(#', entry)

    def test_changelog_mixed_prose_no_double_dash(self) -> None:
        entry = format_changelog_entry(
            '0.1.2',
            'Mixed',
            'Plain prose line\n- Already a bullet\nAnother prose',
            '7',
        )
        self.assertIn('- Plain prose line\n', entry)
        self.assertIn('- Already a bullet\n', entry)
        self.assertIn('- Another prose\n', entry)
        self.assertNotIn('- - ', entry)

    def test_forbidden_paths(self) -> None:
        with self.assertRaises(ValueError):
            check_forbidden_paths([('M', 'services/versions.json')], 'no marker')
        check_forbidden_paths(
            [('M', 'services/versions.json')],
            f'hello {ALLOW_OWNED_FILES_MARKER}',
        )
        check_forbidden_paths(
            [('M', 'services/versions.json')],
            'hello <!--pair-release-intent-allow-owned-files-->',
        )
        check_forbidden_paths([('D', 'services/changelog.md')], 'no marker')
        with self.assertRaises(ValueError):
            check_forbidden_paths([('M', 'services/changelog.md')], 'no marker')

    def test_collapsed_html_comment_fences(self) -> None:
        bumps = {key: 'none' for key in KEYS}
        bump_lines = '\n'.join(f'- {key}: {bumps[key]}' for key in KEYS)
        text = (
            '<!--pair-release-intent-allow-owned-files-->\n'
            '<!--pair-release-intent:v1-->\n'
            '### Changelog title\nn/a\n\n'
            '### Changelog body\nn/a\n\n'
            f'### Bumps\n{bump_lines}\n'
            '<!--/pair-release-intent:v1-->\n'
        )
        intent = parse_release_intent(text, KEYS)
        self.assertFalse(intent.has_release)

    def test_render_versions_mutates_original_keys(self) -> None:
        original = {
            '$comment': 'keep me',
            'services': '0.82.0',
            'components': {
                'ollama-proxy': '0.23.0',
                'lmstudio-proxy': '0.13.1',
            },
            'extra': 'preserved',
        }
        manifest = VersionsManifest(
            services='0.83.0',
            components={'ollama-proxy': '0.23.1', 'lmstudio-proxy': '0.13.1'},
        )
        text = render_versions_json(manifest, original)
        parsed = json.loads(text)
        self.assertEqual(parsed['services'], '0.83.0')
        self.assertEqual(parsed['extra'], 'preserved')
        self.assertEqual(parsed['components']['ollama-proxy'], '0.23.1')
        self.assertTrue(text.endswith('\n'))

    def test_render_versions_drops_retired_keys(self) -> None:
        """A manifest still carrying product/installer renders without them."""
        original = {
            'product': '0.91.7',
            'installer': '0.91.7',
            'components': {'ollama-proxy': '0.26.2'},
        }
        manifest = VersionsManifest(
            services='0.91.8', components={'ollama-proxy': '0.26.3'}
        )
        parsed = json.loads(render_versions_json(manifest, original))
        self.assertEqual(parsed['services'], '0.91.8')
        self.assertNotIn('product', parsed)
        self.assertNotIn('installer', parsed)

    def test_repo_versions_roundtrip(self) -> None:
        manifest, raw = load_versions(VERSIONS_PATH)
        text = render_versions_json(manifest, raw)
        with tempfile.NamedTemporaryFile('w', encoding='utf-8', delete=False) as handle:
            handle.write(text)
            temp_path = Path(handle.name)
        try:
            again, _ = load_versions(temp_path)
            self.assertEqual(again.services, manifest.services)
            self.assertEqual(again.components, manifest.components)
        finally:
            temp_path.unlink(missing_ok=True)


if __name__ == '__main__':
    unittest.main()
