---
name: pair-test-style
description: >-
  Write, refactor, or review tests in PAIR using focused cases,
  descriptive subtests, explicit error handling, and structured assertions.
  Use when adding or changing Go service or desktop tests; apply to the tests
  in scope rather than starting a repository-wide cleanup.
---
<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# NVIDIA PAIR test style

Keep each test's setup, action, and expected behavior easy to follow. These
conventions apply to contributors and coding agents working on repository tests.
They guide test structure; they do not require new coverage or unrelated rewrites.

## Focus each test

- Give each top-level test one behavior to prove. Split long tests that combine
  validation, persistence, restart, and recovery into independently readable tests.
- Use as many assertions as needed to prove that behavior. Keep assertions
  focused on the relevant results or fields; check unrelated behavior in a
  separate test so failures have a clear meaning.
- Exercise the interface callers use and assert observable results. Internal
  refactoring that preserves behavior should not require rewriting tests to
  match private helpers, internal state, or incidental call order.
- Keep a table-driven test dedicated to its related cases. Put an unrelated
  one-off assertion or scenario in its own test function.
- Prefer explicit cases over nested loops and boolean mode matrices when the
  combinations obscure what is being checked. Keep a matrix when the combinations
  themselves are the behavior under test and the names explain them.

## Name the behavior being proved

- Name Go tests `Test<Subject>_<ExpectedBehavior>`, for example
  `TestParseAddress_RejectsMissingPort`. Include the condition when needed to
  distinguish the behavior, as in `TestScheduler_SkipsNodesWhenEngineIsStopped`.
- Consider three candidate names before selecting the one that most clearly
  states the subject and expected behavior. Prefer concrete outcomes over vague
  labels such as `Works`, `HandlesErrors`, or `EdgeCases`.
- Name subtests after the condition or outcome that distinguishes each case:
  `rejects missing port` or `preserves response body`, rather than `true`,
  `false`, or an index. Read the parent and subtest names together to check that
  a failure identifies what was expected.
- For desktop tests, use natural-language names with the same subject and
  expected behavior, such as `parseAddress rejects an address without a port`.

## Make the test readable on its own

- Keep setup, the action under test, and expectations in a clear reading order.
  Separate them with blank lines when helpful; avoid comments that merely label
  obvious steps.
- Use descriptive fixture and variable names. Keep the relevant inputs and
  expected values visible in the case so readers do not have to trace helpers
  to understand what it proves.
- Avoid jargon in test comments where possible. Explain necessary domain terms
  or abbreviations on first use. For example, prefer `A stopped engine cannot
  accept new requests` over `Exercise the ineligible-node path`.
- Use comments to explain why a condition matters, why an expectation follows,
  or why unusual setup is necessary. Do not narrate the code or describe an
  abandoned implementation.
- Make failure messages identify the failed expectation and relevant case
  context. A reader should understand the failure without reconstructing an
  unrelated scenario.

## Share setup without hiding the cases

Prefer clarity over removing duplication. A little repeated setup is worthwhile
when it makes each scenario understandable on its own. Extract helpers that
remove distracting details while keeping the important inputs and expectations
visible; avoid shared mutable fixtures that hide how those values are set.

For Go cases with the same setup and assertions, prefer a local helper closure
that captures shared setup and calls `t.Run`. List explicit, named calls below
it. A signature such as `test := func(name, input string)` often makes cases
easier to read than a large table with mode-dependent branches.

Use ordinary tables when they are clearer. Separate acceptance and rejection
helpers when that removes branching and makes expected outcomes explicit.
For example, use `accept := func(name, input, expected string)` and
`reject := func(name, input, wantMessage string)` for distinct outcomes.
Define and list acceptance cases before rejection cases for consistency. Keep
mutable state fresh per case so cases do not depend on execution order.

Mark Go assertion and setup helpers with `t.Helper()`. Pass the subtest's
`*testing.T` to helpers that report failures so errors belong to the right case.
Keep helpers local unless reuse across tests justifies a shared fixture.

## Check errors and structured results

- State expected results directly using concrete values where practical. Avoid
  computing expectations with the same logic or production helper being tested;
  that can reproduce the implementation's bug and let an incorrect result pass.
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
  sufficient slice length before dereferencing or indexing. Protocol states and
  cluster membership must also hold before continuing a handshake or making
  authenticated requests. When unsure whether continuation is safe, use
  `require`; preserving a fatal assertion is fine.
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
  unless the nil distinction is an explicit contract. Preserve both the non-nil
  and empty checks when a result must encode as `[]` rather than `null`, and use
  `Nil` when absence is part of the contract. If direct equality would
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

## Check effects before internal calls

Prefer checking returned values or resulting state over verifying that internal
methods were called. When using mocks, avoid verifying every read or lookup;
check what the code does with the returned information. Verify an interaction
when it is itself part of the requirement, such as sending a notification,
avoiding repeated requests when caching, or preserving a required ordering.
Keep mock argument checks focused on values relevant to the behavior being
proved, and verify other requirements in separate tests.

## Fit the existing test suite

For desktop tests, carry over the same focus, naming, error handling, and
structured assertions using Vitest's existing patterns. Follow
[desktop test conventions](../../../desktop/tests/README.md) for typed mocks,
module-boundary fakes, network isolation, and temporary-directory cleanup.
Do not introduce a new framework or production-only test hooks for this style.

Run the relevant existing tests and required repository checks. In review,
verify that each changed test has a clear failure condition, names the behavior
it proves, checks its setup errors, and uses comments that explain intent in
plain language. Confirm that its inputs and expectations can be understood
without tracing an unrelated scenario. State any verification that could not
be run.
