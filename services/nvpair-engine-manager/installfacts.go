// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"strings"
)

// Install facts: what this service is able to do about an engine on this machine,
// reported with its state.
//
// The engine card offers an Install button, an Uninstall button, or neither, and
// each of those is only worth offering when it can actually succeed. A button that
// can only produce a refusal is worse than no button, because by the time it is
// clicked the user has already decided what they want. Neither answer is something
// the interface can work out: whether a platform has a way to obtain an engine, and
// who owns the copy that is already here, both live in the manifest and in the
// filesystem. So they are computed here, beside the code that would have performed
// the two operations, and travels with the state.
//
// Both answers are deliberately recomputed from the filesystem rather than cached.
// They are read a few times per engine per status check, which is the same handful
// of stat calls that detecting the engine already costs.

// The owners `installOwner` reports. "pair" is an install this service put where
// it can remove it again; "other" is a copy it merely found, whatever made it.
// Anything else is a named package manager, which is also how its own removal
// command is spelled.
const (
	installOwnerPAIR  = "pair"
	installOwnerOther = "other"
)

// installOwner says whose copy of the engine is on this machine, and "" when
// nothing is. It answers the same question Uninstall asks before it removes
// anything — is this ours to remove? — so the interface that reads this field hides
// exactly the buttons that would refuse.
//
// An engine found only by its port has no path to inspect and is reported as
// "other": whatever is answering there, PAIR did not install it, and it cannot be
// taken back out either way.
func installOwner(st *engineState) string {
	if !st.installed || st.plat == nil {
		return ""
	}
	binPath := st.binPath
	if binPath == "" {
		return installOwnerOther
	}
	if isManagedInstallPath(binPath, st.installDir) {
		return installOwnerPAIR
	}
	if manager := packageManagerOwning(st, binPath); manager != "" {
		return manager
	}
	if st.plat.Runtime.modeOrDefault() == "command" && installedByPAIR(st.installDir) {
		// A command-mode engine picks its own destination, so the directory check
		// above cannot vouch for one and the install marker is the only record.
		return installOwnerPAIR
	}
	return installOwnerOther
}

// canInstallHere reports whether there is an install this service could carry out
// for an engine that is not here: a platform that declares one, a package manager
// to do the work if it needs one, and no copy of the engine already sitting in a
// folder it named.
func canInstallHere(st *engineState) bool {
	if st.installed || st.plat == nil {
		return false
	}
	install := st.plat.Install
	if install == nil || install.ModeOrDefault() == "admin" {
		// No declared way in, or a way this user-mode service refuses.
		return false
	}
	if conflictingEnginePath(st) != "" {
		// The engine is already here by a route PAIR does not own, and a second
		// copy beside it is not an improvement.
		return false
	}
	if pkg := st.plat.pkgInstall(); pkg != nil {
		kind, known := packageManagerKinds[pkg.Manager]
		if !known {
			return false
		}
		// A package mode with no package manager is not an install; it is a
		// message about a missing command. Offer nothing instead.
		return findPackageManagerBinary(kind) != ""
	}
	return len(install.Script) > 0 || install.Fetch != nil ||
		len(install.Artifacts) > 0 || len(install.Run) > 0
}

// packageManagerOwning names the package manager whose tree a detected engine
// binary sits in, or "" when no known manager owns that path. A package manager
// installs into its own prefix and nowhere else, so the prefix is what its
// ownership looks like on disk.
func packageManagerOwning(st *engineState, binPath string) string {
	pkg := st.plat.pkgInstall()
	if pkg == nil {
		return ""
	}
	kind, known := packageManagerKinds[pkg.Manager]
	if !known {
		return ""
	}
	clean := filepath.Clean(binPath)
	for _, prefix := range pkgManagerPrefixes {
		rel, err := filepath.Rel(prefix, clean)
		if err != nil {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			continue
		}
		return kind.label
	}
	return ""
}
