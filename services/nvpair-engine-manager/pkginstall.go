// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// A package-manager install is a different problem from a fetch-and-run install.
// There is no artifact here for PAIR to download and checksum: the package
// manager fetches its own formula, its own Python, and its own dependency
// stack, and it can report success while the engine it installed still cannot
// run. So this file locates the package manager, refuses plainly when the machine
// has none, and only calls an install done once the installed command actually
// answers.
//
// Homebrew installs also stay outside PAIR's ownership: the files land in
// Homebrew's tree, not in a PAIR install directory, so PAIR neither records an
// install marker nor offers an uninstall for them. Removing them means
// `brew uninstall`, which is the user's command to run.

// pkgManagerPrefixes are the prefixes searched for a package manager, in order.
// PATH is deliberately not consulted: a `brew` found on PATH is not necessarily
// one that can fetch prebuilt packages, and a Finder-launched PAIR may not have
// /opt/homebrew/bin on its PATH at all. It is a var so a test can replace the
// whole list with a fixture prefix and never reach the package manager really
// installed on the machine running the tests.
var pkgManagerPrefixes = []string{"/opt/homebrew", "/usr/local"}

// pkgNameRe accepts a package name only. A path or URL is the mistake worth
// rejecting at load: the runner passes this value as one argument to the package
// manager, so anything else resolves to a different command than the manifest
// declares.
var pkgNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// pkgTapRe accepts a package repository written "<user>/<repository>", which is
// how both a tap and its full formula names are spelled.
var pkgTapRe = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

// packageManagerKind is one supported package manager: what to call it, where its
// command lives, and what to ask it for.
type packageManagerKind struct {
	// label names the manager in the refusal and the log.
	label string
	// binary is the command's file name under each prefix.
	binary string
	// docsURL is where someone with no package manager is pointed.
	docsURL string
	// env is added to every command this manager runs.
	env map[string]string
	// steps is what an install asks the manager to do, in order, before the
	// installed engine is checked.
	steps func(pkg *PkgInstall) []pkgStep
	// checkArgs runs the installed formula afterwards, from the same bin
	// directory the manager put its own command in.
	checkArgs []string
	// removeCmd is the command that would undo the install. PAIR never runs it;
	// it is named to the user, because this is the one case where declining to
	// remove something is the right answer and they still need a way to do it.
	removeCmd string
}

// pkgStep is one command in a package-mode install.
type pkgStep struct {
	// argv follows the manager binary, so {"tap","x"} runs `brew tap x`.
	argv []string
	// tolerated means a non-zero exit here does not end the install. Only the
	// trust grant is: a package manager older than the trust gate has no trust
	// command at all and installs anyway, and where the grant does matter the
	// install step then refuses, so nothing is quietly skipped.
	tolerated bool
}

// fullFormulaName is how a package manager addresses the formula. A tap
// qualifies it; a formula from the manager's own repository does not.
func fullFormulaName(pkg *PkgInstall) string {
	if pkg.Tap == "" {
		return pkg.Formula
	}
	return pkg.Tap + "/" + pkg.Formula
}

// homebrewSteps is one Homebrew install. Homebrew 7 will not load a formula from
// a tap the user has not trusted, and a trusted formula is keyed by its full
// name, so the tap has to exist before either later step means anything.
func homebrewSteps(pkg *PkgInstall) []pkgStep {
	name := fullFormulaName(pkg)
	if pkg.Tap == "" {
		return []pkgStep{{argv: []string{"install", name}}}
	}
	return []pkgStep{
		{argv: []string{"tap", pkg.Tap}},
		{argv: []string{"trust", "--formula", name}, tolerated: true},
		{argv: []string{"install", name}},
	}
}

var packageManagerKinds = map[string]packageManagerKind{
	"homebrew": {
		label:   "Homebrew",
		binary:  "brew",
		docsURL: "https://brew.sh",
		env: map[string]string{
			// An auto-update would spend the install's budget refreshing every
			// other formula on the machine first, and the environment hints brew
			// prints are noise nobody reads out of a progress notification.
			"HOMEBREW_NO_AUTO_UPDATE": "1",
			"HOMEBREW_NO_ENV_HINTS":   "1",
		},
		steps:     homebrewSteps,
		checkArgs: []string{"--version"},
		removeCmd: "brew uninstall",
	},
}

// pkgInstall reports the package mode of a platform, if it has one. Installing
// an engine and removing it both ask the same question, so the answer is taken
// from one place: what installed an engine also says who owns it.
func (p *Platform) pkgInstall() *PkgInstall {
	if p == nil || p.Install == nil {
		return nil
	}
	return p.Install.Pkg
}

// packageManagerOwnershipRefusal explains that an engine installed through a
// package manager is not PAIR's to remove, and names the command that is. The
// alternative is PAIR deleting files it never created, out of a tree that also
// holds the model library the engine shares with the user's own copy of it.
func packageManagerOwnershipRefusal(engine string, pkg *PkgInstall) string {
	label, removal := pkg.Manager, pkg.Manager+" uninstall "+fullFormulaName(pkg)
	if kind, known := packageManagerKinds[pkg.Manager]; known {
		label, removal = kind.label, kind.removeCmd+" "+fullFormulaName(pkg)
	}
	return fmt.Sprintf("cannot uninstall engine %q: it was installed through %s, which owns where it lives. Remove it with \"%s\".",
		engine, label, removal)
}

// findPackageManagerBinary looks for the manager command in each known prefix,
// and only there.
func findPackageManagerBinary(kind packageManagerKind) string {
	for _, prefix := range pkgManagerPrefixes {
		candidate := filepath.Join(prefix, "bin", kind.binary)
		if info, err := os.Stat(candidate); err == nil &&
			info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return candidate
		}
	}
	return ""
}

// installWithPackageManager performs a package-mode install: locate the package
// manager, run its steps, then prove that the engine it installed can run.
func (e *Executor) installWithPackageManager(ctx context.Context, engine, displayName string, pkg *PkgInstall) error {
	kind, known := packageManagerKinds[pkg.Manager]
	if !known {
		return fmt.Errorf("install.pkg.manager %q is not a package manager this build can run", pkg.Manager)
	}
	manager := findPackageManagerBinary(kind)
	if manager == "" {
		// Naming what is missing and where to get it is the whole reason a
		// package mode exists; the alternative is a shell error about a command
		// that was never there to find.
		return fmt.Errorf("%s installs through %s, which this Mac does not have. Install %s from %s and start again.",
			displayName, kind.label, kind.label, kind.docsURL)
	}

	e.emitInstallProgress(engine, "installing", 0)
	for _, step := range kind.steps(pkg) {
		out, err := runPackageManagerCommand(ctx, append([]string{manager}, step.argv...), kind.env)
		if err == nil {
			continue
		}
		if step.tolerated {
			slog.Info("the optional package-manager step failed; continuing",
				"engine", engine, "step", strings.Join(step.argv, " "), "output", messageTail(out, 400))
			continue
		}
		// The message carries the manager's own words: "Refusing to load formula
		// … from untrusted tap" names the real fix, "exit status 1" does not.
		return fmt.Errorf("%s %s: %s", kind.label, strings.Join(step.argv, " "), messageTail(out, 400))
	}
	return e.checkInstalledPackageManagerResult(ctx, engine, displayName, kind, manager, pkg)
}

// checkInstalledPackageManagerResult proves the install instead of accepting the
// package manager's exit code. A formula can finish successfully while leaving
// the engine's Python runtime to be built by its own launcher on first use; the
// alternative to checking here is paying for that inside a Start, which has 600
// seconds rather than the install's thirty minutes.
func (e *Executor) checkInstalledPackageManagerResult(ctx context.Context, engine, displayName string, kind packageManagerKind, manager string, pkg *PkgInstall) error {
	installed := filepath.Join(filepath.Dir(manager), pkg.Formula)
	if info, err := os.Stat(installed); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%s was installed by %s, but there is no %s command to check", displayName, kind.label, pkg.Formula)
	}
	out, err := runPackageManagerCommand(ctx, append([]string{installed}, kind.checkArgs...), nil)
	if err != nil {
		return fmt.Errorf("%s was installed but cannot run yet: %s", displayName, messageTail(out, 400))
	}
	slog.Info("package-manager install verified by running the installed engine command", "engine", engine)
	return nil
}

// runPackageManagerCommand runs one package-manager command and keeps its
// combined output. The output is drained into memory rather than piped onward: a
// reader that stops early takes the install down with it, and a package manager
// that wants a password or a confirmation must find no terminal rather than wait
// on one nobody is watching.
func runPackageManagerCommand(ctx context.Context, argv []string, env map[string]string) (string, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	for key, value := range env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	configureSysProcAttr(cmd) // hide the console window on Windows
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// messageTail keeps the end of a command's output for a message to the user.
// Package managers and pip print thousands of lines; a notification needs the
// reason, which lands at the end, not a transcript. It cuts on runes so a
// truncated tail cannot begin mid-character.
func messageTail(out string, limit int) string {
	trimmed := strings.TrimSpace(out)
	runes := []rune(trimmed)
	if len(runes) <= limit {
		return trimmed
	}
	return "…" + string(runes[len(runes)-limit:])
}

// validatePkgInstall rejects a package-mode install the runner could not
// perform: a package mode declared alongside a download it would not use, an
// unknown manager, or a formula and tap the manager cannot resolve.
func validatePkgInstall(key string, install *Install) error {
	pkg := install.Pkg
	if len(install.Script) > 0 || install.Fetch != nil || len(install.Artifacts) > 0 || len(install.Run) > 0 {
		return fmt.Errorf("platform %q: install.pkg is mutually exclusive with script/fetch/artifacts/run (a package manager does its own downloading)", key)
	}
	kind, known := packageManagerKinds[pkg.Manager]
	if !known {
		return fmt.Errorf("platform %q: unknown install package manager %q (this build can run %s)",
			key, pkg.Manager, strings.Join(knownPackageManagerNames(), ", "))
	}
	if !pkgNameRe.MatchString(pkg.Formula) {
		return fmt.Errorf("platform %q: install.pkg.formula %q must be a package name, not a path or URL", key, pkg.Formula)
	}
	if pkg.Tap != "" && !pkgTapRe.MatchString(pkg.Tap) {
		return fmt.Errorf("platform %q: install.pkg.tap %q must be written \"<user>/<repository>\"", key, pkg.Tap)
	}
	if len(kind.steps(pkg)) == 0 {
		return fmt.Errorf("platform %q: install.pkg declares no command to run", key)
	}
	return nil
}

// knownPackageManagerNames lists the managers this build can drive, for the
// message shown when a manifest names another. Sorted so the message is stable.
func knownPackageManagerNames() []string {
	names := make([]string, 0, len(packageManagerKinds))
	for name := range packageManagerKinds {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}
