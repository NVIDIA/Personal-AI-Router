---
name: pair-test-style
description: >-
  Write, refactor, or review tests in Personal AI Router using focused cases,
  descriptive subtests, explicit error handling, and structured assertions.
  Use when adding or changing Go service or desktop tests; apply to the tests
  in scope rather than starting a repository-wide cleanup.
---
<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# PAIR test style

Keep each test's setup, action, and expected behavior easy to follow. These
conventions apply to contributors and coding agents working on repository tests.
They guide test structure; they do not require new coverage or unrelated rewrites.

## Focus each test

- Give each top-level test one behavior to prove. Split long tests that combine
  validation, persistence, restart, and recovery into independently readable tests.
- Keep a table-driven test dedicated to its related cases. Put an unrelated
  one-off assertion or scenario in its own test function.
- Prefer explicit cases over nested loops and boolean mode matrices when the
  combinations obscure what is being checked. Keep a matrix when the combinations
  themselves are the behavior under test and the names explain them.
- Name subtests after their scenario or outcome: `process mode`, `command mode`,
  `rejects bind override`, or `preserves response body`, rather than `true`,
  `false`, or an index.

## Share setup without hiding the cases

For Go cases with the same setup and assertions, prefer a local helper closure
that captures shared setup and calls `t.Run`. List explicit, named calls below
it. A signature such as `test := func(name, input, wantError string)` often makes
cases easier to read than a large table with mode-dependent branches.

Use ordinary tables when they are clearer. Separate acceptance and rejection
helpers when that removes branching and makes expected outcomes explicit. Keep
mutable state fresh per case so cases do not depend on execution order.

Mark Go assertion and setup helpers with `t.Helper()`. Pass the subtest's
`*testing.T` to helpers that report failures so errors belong to the right case.
Keep helpers local unless reuse across tests justifies a shared fixture.

## Check errors and structured results

- Check setup, file I/O, encoding, decoding, and operation errors. Do not discard
  an error just because the fixture is expected to be valid. Fail at the operation
  that failed, with enough context to diagnose it.
- Prefer explicit `t.Fatalf` checks or a small test-only must-succeed helper that
  fails immediately. A `Must` helper is optional; avoid adding an abstraction
  when an explicit check is clearer.
- Decode JSON and other structured output, then assert the relevant fields.
  Avoid `strings.Contains` as evidence that a serialized response or persisted
  configuration has the correct values. Check parsing errors before fields.
- Use substring assertions when the contract is actually unstructured text, such
  as a diagnostic, and the selected text is what the test intends to verify.
- Use named constants such as `http.StatusBadGateway` and `http.MethodPut` instead
  of magic protocol values. Format multiline fixtures and header maps readably;
  run `gofmt` on changed Go tests.

## Fit the existing test suite

For desktop tests, carry over the same focus, naming, error handling, and
structured assertions using Vitest's existing patterns. Follow
[desktop test conventions](../../../desktop/tests/README.md) for typed mocks,
module-boundary fakes, network isolation, and temporary-directory cleanup.
Do not introduce a new framework or production-only test hooks for this style.

Run the relevant existing tests and required repository checks. In review,
verify that each changed test has a clear failure condition, names the behavior
it proves, checks its setup errors, and can be understood without tracing an
unrelated scenario. State any verification that could not be run.
