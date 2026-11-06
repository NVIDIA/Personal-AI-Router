// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import Foundation

guard CommandLine.arguments.count == 2 else {
    fputs("usage: verify-helper-signatures <signed-app.app>\n", stderr)
    exit(2)
}

let app = URL(fileURLWithPath: CommandLine.arguments[1]).standardizedFileURL
let checks = [
    (
        app.appendingPathComponent("Contents/MacOS/\(HelperConstants.daemonIdentifier)"),
        HelperConstants.daemonCodeSigningRequirement
    ),
    (
        app.appendingPathComponent("Contents/MacOS/\(HelperConstants.ctlIdentifier)"),
        HelperConstants.ctlCodeSigningRequirement
    ),
    (app, HelperConstants.appCodeSigningRequirement)
]

var failed = false
for (target, requirement) in checks {
    let process = Process()
    process.executableURL = URL(fileURLWithPath: "/usr/bin/codesign")
    process.arguments = [
        "--verify", "--strict", "--all-architectures", "--verbose=2",
        "-R=\(requirement)", target.path
    ]
    do {
        try process.run()
        process.waitUntilExit()
        if process.terminationStatus != 0 {
            fputs("Signing requirement failed: \(target.path)\n", stderr)
            failed = true
        }
    } catch {
        fputs("Cannot verify \(target.path): \(error.localizedDescription)\n", stderr)
        failed = true
    }
}

exit(failed ? 1 : 0)
