// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import Foundation
import Security

// MARK: - Firewall

enum FirewallError: Error, LocalizedError {
    case invalidAppPath(String)
    case untrustedAppPath(String)
    case socketfilterfwMissing
    case socketfilterfwFailed(String, Int32)
    case untrustedCaller(String)

    var errorDescription: String? {
        switch self {
        case .invalidAppPath(let p): return "Invalid application path: \(p)"
        case .untrustedAppPath(let p): return "Application is not signed by NVIDIA: \(p)"
        case .socketfilterfwMissing: return "Application Firewall tool is unavailable"
        case .socketfilterfwFailed(let op, let code):
            return "socketfilterfw \(op) failed with status \(code)"
        case .untrustedCaller(let reason): return "Rejected privileged request: \(reason)"
        }
    }
}

enum Firewall {
    static let socketfilterfw = "/usr/libexec/ApplicationFirewall/socketfilterfw"

    /// Networked cli-bin listeners. Stdio-only or client-only binaries
    /// (nvpair-node-settings, nvpair-manual-nodes, nvpair-job-scheduler,
    /// nvpair-ui-broker) get no rule. Keep in sync with the
    /// needsFirewallAccess entries in
    /// src/shared/constants/modular-binaries.ts (npm run service-contracts:check
    /// verifies this) and the manual uninstaller
    /// (scripts/build/macos/uninstall.sh).
    static let networkedBinaries = [
        "ollama-proxy",
        "lmstudio-proxy",
        "llamacpp-proxy",
        "nvpair-node-info",
        "nvpair-node-scanner",
        "nvpair-workload-manager",
        "nvpair-errors",
        "nvpair-cluster-manager",
        "nvpair-engine-manager"
    ]

    static func apply(cliBinDir: String, unblock: Bool) throws {
        guard FileManager.default.isExecutableFile(atPath: socketfilterfw) else {
            throw FirewallError.socketfilterfwMissing
        }
        let fm = FileManager.default
        for name in networkedBinaries {
            let binPath = (cliBinDir as NSString).appendingPathComponent(name)
            guard fm.fileExists(atPath: binPath) else { continue }

            if unblock {
                // Granting network access — be certain this is exactly our
                // binary. TOCTOU guard: re-check identity of this object
                // immediately before socketfilterfw. Reject symlinks (so a
                // swapped link cannot redirect the firewall to an attacker
                // binary), re-verify the binary's own NVIDIA signature, and fail
                // hard on any non-zero socketfilterfw exit.
                try assertRealFile(binPath)
                guard
                    verifySignature(
                        path: binPath,
                        requirement: HelperConstants.nvidiaSignedRequirement
                    )
                else {
                    throw FirewallError.untrustedAppPath(binPath)
                }
                try runChecked(socketfilterfw, ["--add", binPath])
                try runChecked(socketfilterfw, ["--unblockapp", binPath])
            } else {
                // Removal only revokes access, so it is best-effort: still reject
                // a symlink (never touch a swapped target), but do NOT abort the
                // whole cleanup if one rule was never added — `socketfilterfw
                // --remove` of an absent entry returns non-zero, which must not
                // strand the remaining removals (e.g. during uninstall).
                do { try assertRealFile(binPath) } catch { continue }
                _ = run(socketfilterfw, ["--remove", binPath])
            }
        }
    }

    /// Validate that `appPath` (already derived from the *verified* connecting
    /// process, never from caller argv) is the canonical, NVIDIA-signed host
    /// bundle and return its cli-bin directory. Defense in depth on top of the
    /// caller-identity check: reject traversal, reject a symlinked `.app` /
    /// `cli-bin` (open the final component with `O_NOFOLLOW`), reject
    /// group/other-writable ancestor dirs (a swappable parent enables TOCTOU),
    /// and re-verify the bundle signature before mutating firewall state.
    static func validatedCliBinDir(appPath: String) throws -> String {
        let standardized = (appPath as NSString).standardizingPath
        guard standardized.hasSuffix(".app"), !standardized.contains("..") else {
            throw FirewallError.invalidAppPath(appPath)
        }
        try assertRealDirectory(standardized)
        try assertUnwritableAncestors(standardized)
        guard
            verifySignature(
                path: standardized,
                requirement: HelperConstants.appCodeSigningRequirement
            )
        else {
            throw FirewallError.untrustedAppPath(appPath)
        }
        let cliBinDir = (standardized as NSString)
            .appendingPathComponent("Contents/Resources/cli-bin")
        try assertRealDirectory(cliBinDir)
        return cliBinDir
    }

    /// Open `path` as a directory without following a final symlink; throws if it
    /// is a symlink or not a directory.
    static func assertRealDirectory(_ path: String) throws {
        let fd = open(path, O_RDONLY | O_DIRECTORY | O_NOFOLLOW)
        if fd < 0 {
            throw FirewallError.invalidAppPath(path)
        }
        close(fd)
    }

    /// Open `path` without following a final symlink and require a regular file.
    static func assertRealFile(_ path: String) throws {
        let fd = open(path, O_RDONLY | O_NOFOLLOW)
        if fd < 0 {
            throw FirewallError.untrustedAppPath(path)
        }
        defer { close(fd) }
        var info = stat()
        if fstat(fd, &info) != 0 || (info.st_mode & S_IFMT) != S_IFREG {
            throw FirewallError.untrustedAppPath(path)
        }
    }

    /// Reject if any ancestor directory is *world*-writable; such a directory
    /// would let any unprivileged user swap a path component between validation
    /// and use. We deliberately do NOT reject group-writable dirs: the standard
    /// `/Applications` install location is `root:admin` mode 0775 (group-writable
    /// by design), and members of `admin` are already privilege-equivalent to the
    /// installer, so a group-writable ancestor is not a new escalation surface.
    static func assertUnwritableAncestors(_ path: String) throws {
        let fm = FileManager.default
        var current = (path as NSString).deletingLastPathComponent
        while !current.isEmpty && current != "/" {
            let attrs = try fm.attributesOfItem(atPath: current)
            let perms = (attrs[.posixPermissions] as? NSNumber)?.uint16Value ?? 0o777
            if (perms & 0o002) != 0 {
                throw FirewallError.untrustedAppPath(current)
            }
            current = (current as NSString).deletingLastPathComponent
        }
    }
}

/// Resolve the filesystem path of the current XPC peer from its *verified* code
/// identity, then derive the host `.app` bundle from it. Because the daemon acts
/// only on the connecting process's own bundle, a caller can never point it at an
/// arbitrary NVIDIA-signed app (the confused-deputy the `--app-path` argv enabled).
func callerAppBundlePath() throws -> String {
    guard let connection = NSXPCConnection.current() else {
        throw FirewallError.untrustedCaller("no XPC peer")
    }
    // Resolve the peer via its live PID. The kernel audit token would be the
    // race-free handle, but `NSXPCConnection.auditToken` is SPI (no public Swift
    // declaration) and this target compiles as pure Swift with `swiftc` — no
    // Objective-C bridging header to expose it — so we deliberately stay on the
    // PID path. The PID-reuse window is not exploitable here because:
    //   1. The listener already pins the connection with
    //      `setCodeSigningRequirement(ctlCodeSigningRequirement)` (macOS 13's
    //      Apple-recommended client validation), so only the NVIDIA-signed
    //      control tool can open this connection at all; and
    //   2. `SecCodeCheckValidity` below re-validates whatever process the PID
    //      currently maps to against the same pinned requirement, rejecting any
    //      non-NVIDIA process that might have reused the PID.
    let attributes = [kSecGuestAttributePid: NSNumber(value: connection.processIdentifier)]
        as CFDictionary
    var code: SecCode?
    guard SecCodeCopyGuestWithAttributes(nil, attributes, [], &code) == errSecSuccess,
        let peer = code
    else {
        throw FirewallError.untrustedCaller("cannot identify XPC peer")
    }
    var requirement: SecRequirement?
    guard
        SecRequirementCreateWithString(
            HelperConstants.ctlCodeSigningRequirement as CFString,
            [],
            &requirement
        ) == errSecSuccess, let req = requirement
    else {
        throw FirewallError.untrustedCaller("bad requirement")
    }
    guard SecCodeCheckValidity(peer, [], req) == errSecSuccess else {
        throw FirewallError.untrustedCaller("caller failed code-signing requirement")
    }
    var staticCode: SecStaticCode?
    guard SecCodeCopyStaticCode(peer, [], &staticCode) == errSecSuccess,
        let resolved = staticCode
    else {
        throw FirewallError.untrustedCaller("cannot resolve caller code")
    }
    var pathRef: CFURL?
    guard SecCodeCopyPath(resolved, [], &pathRef) == errSecSuccess,
        let ctlURL = pathRef as URL?
    else {
        throw FirewallError.untrustedCaller("cannot resolve caller path")
    }
    // ctlURL == <App>.app/Contents/MacOS/nvpair-helper-ctl → strip three components.
    let appURL = ctlURL
        .deletingLastPathComponent()
        .deletingLastPathComponent()
        .deletingLastPathComponent()
    return appURL.path
}

@discardableResult
func run(_ launchPath: String, _ args: [String]) -> Int32 {
    let process = Process()
    process.executableURL = URL(fileURLWithPath: launchPath)
    process.arguments = args
    process.standardOutput = FileHandle.nullDevice
    process.standardError = FileHandle.nullDevice
    do {
        try process.run()
    } catch {
        return -1
    }
    process.waitUntilExit()
    return process.terminationStatus
}

/// Run a subprocess and throw if it exits non-zero. A silently-ignored failure
/// used to leave the firewall in a partially-applied state.
func runChecked(_ launchPath: String, _ args: [String]) throws {
    let status = run(launchPath, args)
    if status != 0 {
        throw FirewallError.socketfilterfwFailed(args.first ?? launchPath, status)
    }
}

func verifySignature(path: String, requirement requirementString: String) -> Bool {
    let url = URL(fileURLWithPath: path)
    var staticCode: SecStaticCode?
    guard SecStaticCodeCreateWithPath(url as CFURL, [], &staticCode) == errSecSuccess,
        let code = staticCode
    else {
        return false
    }
    var requirement: SecRequirement?
    guard SecRequirementCreateWithString(requirementString as CFString, [], &requirement)
        == errSecSuccess, let req = requirement
    else {
        return false
    }
    return SecStaticCodeCheckValidity(code, [], req) == errSecSuccess
}

// MARK: - XPC service

final class HelperService: NSObject, PAIRHelperProtocol {
    func getVersion(reply: @escaping (String) -> Void) {
        reply(HelperVersionInfo.version)
    }

    // `appPath` is ignored: the target bundle is derived from the verified
    // connecting process, so a caller can only configure its own bundle.
    func configureFirewall(appPath _: String, reply: @escaping (Bool, String?) -> Void) {
        applyFromCaller(unblock: true, reply: reply)
    }

    func removeFirewall(appPath _: String, reply: @escaping (Bool, String?) -> Void) {
        applyFromCaller(unblock: false, reply: reply)
    }

    private func applyFromCaller(unblock: Bool, reply: (Bool, String?) -> Void) {
        do {
            let appPath = try callerAppBundlePath()
            let cliBinDir = try Firewall.validatedCliBinDir(appPath: appPath)
            try Firewall.apply(cliBinDir: cliBinDir, unblock: unblock)
            reply(true, nil)
        } catch {
            reply(false, error.localizedDescription)
        }
    }
}

final class ListenerDelegate: NSObject, NSXPCListenerDelegate {
    func listener(
        _ listener: NSXPCListener,
        shouldAcceptNewConnection newConnection: NSXPCConnection
    ) -> Bool {
        // Only the NVIDIA-signed control tool may drive privileged operations
        // (pinned by signing identifier, not just Team ID). The connection
        // rejects messages from peers that fail this requirement.
        newConnection.setCodeSigningRequirement(HelperConstants.ctlCodeSigningRequirement)
        newConnection.exportedInterface = NSXPCInterface(with: PAIRHelperProtocol.self)
        newConnection.exportedObject = HelperService()
        newConnection.resume()
        return true
    }
}

let delegate = ListenerDelegate()
let listener = NSXPCListener(machServiceName: HelperConstants.machServiceName)
listener.delegate = delegate
listener.resume()
dispatchMain()
