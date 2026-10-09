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
- Use `require.NoError` for setup or decoding that must succeed before the test
  can proceed. Use `assert.NoError` for other errors, including previously
  discarded JSON encoding/decoding errors, when continuation is safe. Keep added
  error checks separate from a pure assertion migration when requested.
- Decode JSON and other structured output, then assert the relevant fields.
  Avoid `strings.Contains` as evidence that a serialized response or persisted
  configuration has the correct values. Check parsing errors before fields.
- Use substring assertions when the contract is actually unstructured text, such
  as a diagnostic, and the selected text is what the test intends to verify.
- Use named constants such as `http.StatusBadGateway` and `http.MethodPut` instead
  of magic protocol values. Format multiline fixtures and header maps readably;
  run `gofmt` on changed Go tests.

## Use Testify concisely

- Prefer `assert` for independent expectations so a failure can report alongside
  later failures. Use `require` for prerequisites such as a non-nil pointer or
  sufficient slice length before dereferencing or indexing. When unsure whether
  continuation is safe, use `require`; preserving a fatal assertion is fine.
- Prefer `assert.Equal` and `require.Equal` with expected values first. Avoid
  `EqualValues` when both arguments already have the same type. For numeric
  literals compared with typed values, use an explicitly typed expectation,
  for example `assert.Equal(t, int64(0), counter.Load())`. Reserve `EqualValues`
  for cases where comparison across types is part of the intended behavior.
- Inline values used only once: `assert.Equal(t, "v8-version", versions["v8"])`
  and `require.NoError(t, json.Unmarshal(data, &result))`. Avoid a temporary
  `got` or `err` and a surrounding block just to make one assertion. Retain a
  variable when it is reused or makes a complicated operation easier to read.
- Prefer direct comparisons of complete values over `assert.True` with
  `bytes.Equal` or `reflect.DeepEqual`. Compare slices directly with `Equal`
  where possible; avoid branching on an empty expected slice just to change
  assertion methods. Use `Empty` for an emptiness expectation instead of `Nil`
  unless the nil distinction is an explicit contract. If direct equality would
  distinguish nil from empty against the intended contract, normalize concisely
  or use `Empty` for that case.
- Use `Same`/`NotSame` only when pointer identity is the behavior under test.
  Otherwise compare the contents or relevant fields; investigate an existing
  identity comparison before carrying it over mechanically.
- Let Testify print the compared values. Add assertion messages only for useful
  context; avoid repeating actual/expected values in diagnostic arguments but you
  can explain the intended contract.
  Omit labels such as `"got"` when the comparison is self-explanatory.
  In loops without named subtests, include the case input when it helps locate
  the failure, for example `assert.True(t, isHostname(s), "hostname %q", s)`.
  Although such lack of named subtests should be considered for rewrite.
- Keep refactors faithful: preserve cases, expected values, evaluation count,
  and necessary fatal guards. Flag behavioral changes, added/removed tests, or
  modified assertion helpers. A fatal-to-nonfatal change is acceptable when
  continuation is safe. Format with `gofmt`/`goimports` after editing.

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
