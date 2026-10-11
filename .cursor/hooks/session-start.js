#!/usr/bin/env node
// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Runs once per Cursor session. Clears the read-log so read-before-write
// enforcement only counts reads done inside the current session.
const fs = require('fs')
const path = require('path')

const STATE_DIR = path.join('.cursor', 'hooks', 'state')

try {
    fs.mkdirSync(STATE_DIR, { recursive: true })
    fs.writeFileSync(path.join(STATE_DIR, 'reads.json'), '{}')
} catch (err) {
    process.stderr.write(`[hook session-start] ${err && err.message}\n`)
}
