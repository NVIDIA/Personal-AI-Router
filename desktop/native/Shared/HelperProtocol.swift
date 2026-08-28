// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import Foundation

/// Shared between the privileged daemon (`PrivilegedHelper`) and the user-context
/// control tool (`HelperControl`). Both binaries compile this file in, so the
/// XPC interface and the security constants can never drift between the two
/// sides of the connection.
enum HelperConstants {
    /// launchd `Label` of the daemon and the basename of its LaunchDaemon plist.
    static let daemonLabel = "com.nvidia.nvpair.helper"
    static let daemonPlistName = "com.nvidia.nvpair.helper.plist"

    /// Mach service the daemon vends and the control tool connects to. Must match
    /// the `MachServices` key in the LaunchDaemon plist.
    static let machServiceName = "com.nvidia.nvpair.helper.xpc"

    /// NVIDIA's Apple Developer Team ID and the host app's bundle id.
    static let teamIdentifier = "6KR3T733EC"
    static let appBundleIdentifier = "com.nvidia.nvpair"

    /// Basenames `codesign` assigns as the signing identifier of the two loose
    /// helper Mach-O files (codesign defaults the identifier to the file name).
    static let ctlIdentifier = "nvpair-helper-ctl"
    static let daemonIdentifier = "com.nvidia.nvpair.helper"

    /// Common trust anchor clause. `anchor apple generic` proves an Apple-issued
    /// chain; the Developer ID intermediate marker (`6.2.6`) and the Developer ID
    /// Application leaf marker (`6.1.13`) prove the chain is specifically a
    /// notarization-grade Developer ID chain (not just any Apple-anchored cert,
    /// e.g. a Mac App Store or ad-hoc-into-keychain cert); the leaf OU pins
    /// NVIDIA's Team ID. `anchor apple generic and OU = TeamID` alone was too
    /// loose — any code signed under the same Team ID satisfied it.
    ///
    /// NOTE (mac-builder): the Developer ID marker OIDs assume the 3S signing
    /// service issues Developer ID Application certs (required for notarized
    /// non-App-Store distribution, which PAIR uses). Verify on the mac-builder
    /// that the shipped helpers satisfy this requirement (`codesign -R`).
    static let developerIdAnchor =
        "anchor apple generic "
        + "and certificate 1[field.1.2.840.113635.100.6.2.6] exists "
        + "and certificate leaf[field.1.2.840.113635.100.6.1.13] exists "
        + "and certificate leaf[subject.OU] = \"\(teamIdentifier)\""

    /// Requirement any NVIDIA-signed Mach-O must satisfy (no identifier pin).
    /// Used to re-verify each individual `cli-bin` executable immediately before
    /// handing it to `socketfilterfw`.
    static let nvidiaSignedRequirement = developerIdAnchor

    /// The daemon pins this on every **inbound** connection: only the control
    /// tool (`nvpair-helper-ctl`) may drive privileged operations.
    static let ctlCodeSigningRequirement =
        "identifier \"\(ctlIdentifier)\" and " + developerIdAnchor

    /// The control tool pins this on its **outbound** connection: defense against
    /// a hijacked Mach service name answering as the daemon.
    static let daemonCodeSigningRequirement =
        "identifier \"\(daemonIdentifier)\" and " + developerIdAnchor

    /// Requirement for the host `.app` bundle the daemon configures. The bundle
    /// identifier is deterministic (electron-builder sets it), so pin it too.
    static let appCodeSigningRequirement =
        "identifier \"\(appBundleIdentifier)\" and " + developerIdAnchor
}

/// Fixed command surface exposed over XPC. No arbitrary shell: every method maps
/// to one validated privileged operation.
@objc(PAIRHelperProtocol)
protocol PAIRHelperProtocol {
    /// Returns the daemon's build version (stamped to equal the app version).
    func getVersion(reply: @escaping (String) -> Void)

    /// Adds + unblocks the bundled networked binaries in the macOS Application
    /// Firewall. `appPath` is **ignored** (retained only for wire compatibility):
    /// the daemon derives the target bundle from the *verified* identity of the
    /// connecting process, so a caller can only ever configure its own bundle —
    /// never point the daemon at an arbitrary NVIDIA-signed `.app`.
    func configureFirewall(appPath: String, reply: @escaping (Bool, String?) -> Void)

    /// Removes the bundled networked binaries from the Application Firewall.
    /// `appPath` is ignored (see `configureFirewall`).
    func removeFirewall(appPath: String, reply: @escaping (Bool, String?) -> Void)
}
