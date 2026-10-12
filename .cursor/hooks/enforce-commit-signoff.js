#!/usr/bin/env node
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// preToolUse(Shell): deny a `git commit` that would create a commit with no
// Signed-off-by trailer. CONTRIBUTING.md requires the DCO sign-off on every
// commit, and the standard check matches the trailer against the commit author,
// so an unsigned commit cannot be repaired in review — it has to be rewritten.
//
// Requiring -s unconditionally is safe: git does not add a second identical
// trailer, so `git commit --amend --no-edit -s` is correct whether or not the
// commit already carried one.
//
// Only a segment whose own command is `git commit` is inspected. cherry-pick,
// revert, rebase, and merge are never matched: a replayed commit must keep its
// original author's trailer, and merge commits are exempt.
//
// This hook gates agent tool calls only. A command typed directly into a
// terminal is not intercepted, so it is a guardrail, not a security control.

function readStdin() {
    return new Promise(resolve => {
        let data = ''
        process.stdin.setEncoding('utf8')
        process.stdin.on('data', c => (data += c))
        process.stdin.on('end', () => resolve(data))
    })
}

function allow() {
    process.stdout.write(JSON.stringify({ permission: 'allow' }))
    process.exit(0)
}

function deny(msg) {
    process.stdout.write(
        JSON.stringify({
            permission: 'deny',
            agent_message: msg,
            user_message: 'Sign-off hook blocked a git commit without -s.'
        })
    )
    process.exit(0)
}

// `cd x && git commit -m y` has to be inspected as its own command rather than
// as one opaque string, so the invocation is split on shell separators.
const SEPARATORS = /&&|\|\||;|\n|\|/

// Git-level options that may sit between `git` and the subcommand, so
// `git -C path commit` is still recognized.
const GIT_COMMIT =
    /^git\s+(?:(?:-C|-c|--git-dir|--work-tree|--namespace|--exec-path)(?:=|\s+)\S+\s+)*commit(?:\s|$)/

const LONG_SIGNOFF = /(?:^|\s)--signoff(?:\s|$)/
const NO_SIGNOFF = /(?:^|\s)--no-signoff(?:\s|$)/

// A short-option cluster such as -sm or -asm carries the sign-off too. Matched
// case-sensitively so -S (GPG signing) is never mistaken for -s.
function hasShortSignoff(segment) {
    const clusters = segment.match(/(?:^|\s)-[A-Za-z]+/g) || []
    return clusters.some(cluster => cluster.includes('s'))
}

// Only the flag counts. A trailer typed into the message is rejected: a newline
// is treated as a command separator above, so a hand-written trailer would be
// seen on a single-line command and missed on a multi-line one, and a commit
// message that merely mentions "Signed-off-by:" would bypass the gate entirely.
function isSignedOff(segment) {
    return LONG_SIGNOFF.test(segment) || hasShortSignoff(segment)
}

function offendingSegment(command) {
    for (const raw of command.split(SEPARATORS)) {
        const segment = raw.trim()
        if (!GIT_COMMIT.test(segment)) continue
        if (NO_SIGNOFF.test(segment)) continue
        if (isSignedOff(segment)) continue
        return segment
    }
    return null
}

async function main() {
    const raw = await readStdin()
    let input
    try {
        input = JSON.parse(raw || '{}')
    } catch {
        return allow()
    }

    // preToolUse carries the command at tool_input.command. Reading a
    // top-level `command` would silently allow everything: that field belongs
    // to beforeShellExecution, which is a different hook.
    const toolInput = input.tool_input || input.input || input.arguments || input.args || {}
    const command = toolInput.command
    if (typeof command !== 'string' || command.length === 0) return allow()

    const segment = offendingSegment(command)
    if (segment === null) return allow()

    return deny(
        `git commit blocked by project hook (commit-sign-off): "${segment}" would ` +
            `create a commit with no Signed-off-by trailer. CONTRIBUTING.md requires ` +
            `the DCO sign-off on every commit, and the trailer must match the commit ` +
            `author, so this cannot be fixed in review. Add -s (for example ` +
            `\`git commit -s -m "..."\`) and retry; -s is idempotent, so it is also ` +
            `correct with --amend. Concluding a conflicted merge, or any other commit ` +
            `that genuinely must not be signed off, needs an explicit --no-signoff. ` +
            `Never add your own sign-off to a commit authored by someone else.`
    )
}

main().catch(err => {
    process.stderr.write(`[hook enforce-commit-signoff] ${err && err.message}\n`)
    // Fail-open on internal errors so a buggy hook never hard-blocks the agent.
    allow()
})
