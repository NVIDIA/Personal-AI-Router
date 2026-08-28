// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import Foundation
import ServiceManagement

/// `nvpair-helper-ctl` — the only privileged-helper surface Electron ever spawns.
/// It registers/unregisters the LaunchDaemon via SMAppService (using its own
/// `Bundle.main`, which resolves to the host `.app` because the tool ships in
/// `Contents/MacOS/`) and relays firewall/status/version requests to the daemon
/// over XPC. Every command prints a single JSON object to stdout.

// MARK: - Output

func emit(_ object: [String: Any]) {
    guard
        let data = try? JSONSerialization.data(
            withJSONObject: object,
            options: [.sortedKeys]
        ),
        let json = String(data: data, encoding: .utf8)
    else {
        print("{\"ok\":false,\"error\":\"failed to encode result\"}")
        return
    }
    print(json)
}

func statusString(_ status: SMAppService.Status) -> String {
    switch status {
    case .notRegistered: return "notRegistered"
    case .enabled: return "enabled"
    case .requiresApproval: return "requiresApproval"
    case .notFound: return "notFound"
    @unknown default: return "unknown"
    }
}

let daemon = SMAppService.daemon(plistName: HelperConstants.daemonPlistName)

// MARK: - XPC client

func connectToDaemon() -> NSXPCConnection {
    let connection = NSXPCConnection(
        machServiceName: HelperConstants.machServiceName,
        options: .privileged
    )
    connection.remoteObjectInterface = NSXPCInterface(with: PAIRHelperProtocol.self)
    // Pin the daemon's identity (by signing identifier, not just Team ID) so a
    // hijacked Mach service name cannot answer.
    connection.setCodeSigningRequirement(HelperConstants.daemonCodeSigningRequirement)
    connection.resume()
    return connection
}

func daemonVersion(timeout: TimeInterval = 5) -> String? {
    let connection = connectToDaemon()
    defer { connection.invalidate() }
    let semaphore = DispatchSemaphore(value: 0)
    var version: String?
    guard
        let helper = connection.remoteObjectProxyWithErrorHandler({ _ in
            semaphore.signal()
        }) as? PAIRHelperProtocol
    else {
        return nil
    }
    helper.getVersion { value in
        version = value
        semaphore.signal()
    }
    _ = semaphore.wait(timeout: .now() + timeout)
    return version
}

func callFirewall(appPath: String, remove: Bool, timeout: TimeInterval = 30) -> (Bool, String?) {
    let connection = connectToDaemon()
    defer { connection.invalidate() }
    let semaphore = DispatchSemaphore(value: 0)
    var ok = false
    var errorMessage: String?
    guard
        let helper = connection.remoteObjectProxyWithErrorHandler({ error in
            errorMessage = error.localizedDescription
            semaphore.signal()
        }) as? PAIRHelperProtocol
    else {
        return (false, "could not reach privileged helper")
    }
    let reply: (Bool, String?) -> Void = { success, message in
        ok = success
        errorMessage = message
        semaphore.signal()
    }
    if remove {
        helper.removeFirewall(appPath: appPath, reply: reply)
    } else {
        helper.configureFirewall(appPath: appPath, reply: reply)
    }
    if semaphore.wait(timeout: .now() + timeout) == .timedOut {
        return (false, "privileged helper timed out")
    }
    return (ok, errorMessage)
}

// MARK: - Commands

func cmdInstall() -> Int32 {
    do {
        try daemon.register()
    } catch {
        // register() throws when the service is already registered or blocked;
        // surface the resolved status so the caller can decide what to do.
        emit([
            "action": "install",
            "ok": false,
            "status": statusString(daemon.status),
            "error": error.localizedDescription
        ])
        return 1
    }
    var approvalOpened = false
    if daemon.status == .requiresApproval {
        SMAppService.openSystemSettingsLoginItems()
        approvalOpened = true
    }
    emit([
        "action": "install",
        "ok": true,
        "status": statusString(daemon.status),
        "approvalOpened": approvalOpened
    ])
    return 0
}

func cmdUninstall() -> Int32 {
    let semaphore = DispatchSemaphore(value: 0)
    var unregisterError: Error?
    daemon.unregister { error in
        unregisterError = error
        semaphore.signal()
    }
    _ = semaphore.wait(timeout: .now() + 15)
    if let error = unregisterError {
        emit(["action": "uninstall", "ok": false, "error": error.localizedDescription])
        return 1
    }
    emit(["action": "uninstall", "ok": true, "status": statusString(daemon.status)])
    return 0
}

func cmdStatus() -> Int32 {
    let version = daemonVersion()
    var output: [String: Any] = [
        "registration": statusString(daemon.status),
        "ctlVersion": HelperVersionInfo.version,
        "reachable": version != nil
    ]
    if let version = version {
        output["daemonVersion"] = version
    }
    emit(output)
    return 0
}

func cmdVersion() -> Int32 {
    var output: [String: Any] = ["ctlVersion": HelperVersionInfo.version]
    if let version = daemonVersion() {
        output["daemonVersion"] = version
    }
    emit(output)
    return 0
}

func cmdFirewall(remove: Bool) -> Int32 {
    // The daemon derives the target bundle from this tool's own verified identity
    // and ignores any path we send, so `--app-path` is no longer required (nor
    // trusted). Pass an empty string for wire compatibility with the XPC method.
    let (ok, message) = callFirewall(appPath: "", remove: remove)
    var output: [String: Any] = [
        "action": remove ? "remove-firewall" : "configure-firewall",
        "ok": ok
    ]
    if let message = message, !ok {
        output["error"] = message
    }
    emit(output)
    return ok ? 0 : 1
}

// MARK: - Argument parsing

let arguments = Array(CommandLine.arguments.dropFirst())

let command = arguments.first ?? "status"
let exitCode: Int32

switch command {
case "install":
    exitCode = cmdInstall()
case "uninstall":
    exitCode = cmdUninstall()
case "status":
    exitCode = cmdStatus()
case "version":
    exitCode = cmdVersion()
case "configure-firewall":
    exitCode = cmdFirewall(remove: false)
case "remove-firewall":
    exitCode = cmdFirewall(remove: true)
default:
    emit(["ok": false, "error": "unknown command: \(command)"])
    exitCode = 2
}

exit(exitCode)
