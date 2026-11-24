// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// A package-manager install has no artifact for PAIR to download and no
// checksum to verify: Homebrew fetches its own formula, its Python, and the MLX
// stack, and the only evidence of a good install is that the installed command
// then runs. These tests stand Homebrew up as a shell script in a throwaway
// prefix, so the whole flow travels through a real command with no real package
// manager. Every test replaces pkgManagerPrefixes rather than adding to it, so
// one can never reach the Homebrew installed on the machine running the tests.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testMtplxTap     = "youssofal/mtplx"
	testMtplxFormula = "mtplx"
)

// brewRecordArgs logs one transcript line per call, holding the arguments brew
// was given. $TRANSCRIPT is set by the test and reaches the script because the
// runner passes the parent environment through.
const brewRecordArgs = `printf 'brew %s\n' "$*" >> "$TRANSCRIPT"`

// brewRecordArgsAndEnv also logs the Homebrew settings it was handed and
// whether it had a terminal, which is how the test below reads what the runner
// set up rather than trusting that it did.
const brewRecordArgsAndEnv = `printf 'brew %s | %s | tty=%s\n' "$*" "$(env | grep '^HOMEBREW' | sort | tr '\n' ' ')" "$([ -t 0 ] && echo yes || echo no)" >> "$TRANSCRIPT"`

// brewLeavesWorkingMtplx is the fake's answer to a finished install: it puts an
// `mtplx` command in the same bin directory, and that command logs its own
// arguments, so a test can see whether the runner ran it afterwards. This is
// what Homebrew's own post_install step produces.
const brewLeavesWorkingMtplx = `if [ "$1" = install ]; then
  bindir=$(dirname "$0")
  printf '%s\n' '#!/bin/sh' 'echo "mtplx $*" >> "$TRANSCRIPT"' > "$bindir/mtplx"
  chmod +x "$bindir/mtplx"
fi
exit 0`

// brewLeavesRuntimeToFirstLaunch is the other answer a real Homebrew can give:
// it reports success while leaving an `mtplx` that still has to download its
// Python stack, which fails here because the fixture has no network. The
// launcher's deferred bootstrap and this are the same shape to PAIR — exit 0
// from brew, a command that cannot run yet.
const brewLeavesRuntimeToFirstLaunch = `if [ "$1" = install ]; then
  bindir=$(dirname "$0")
  printf '%s\n' '#!/bin/sh' 'echo "MTPLX runtime is not installed. Bootstrapping with pip..."' 'echo "ERROR: Could not find a version that satisfies the requirement mlx==0.32.2"' 'exit 1' > "$bindir/mtplx"
  chmod +x "$bindir/mtplx"
fi
exit 0`

// skipWithoutShell skips a test whose stand-ins are shell scripts: Windows CI
// has no /bin/sh to run them with, and no Homebrew to stand in for.
func skipWithoutShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in package manager and engine commands are shell scripts")
	}
}

// writeFakeBrew puts a stand-in brew at <prefix>/bin/brew. body is the whole
// script body; it runs with no arguments of its own beyond what the runner
// passes, exactly as the real command would.
func writeFakeBrew(t *testing.T, prefix, body string) {
	t.Helper()
	skipWithoutShell(t)
	path := filepath.Join(prefix, "bin", "brew")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755))
}

// useFakeHomebrew restricts the package-manager search to the throwaway prefix.
func useFakeHomebrew(t *testing.T, prefix string) {
	t.Helper()
	skipWithoutShell(t)
	original := pkgManagerPrefixes
	pkgManagerPrefixes = []string{prefix}
	t.Cleanup(func() { pkgManagerPrefixes = original })
}

// mtplxLikeManifest describes an engine whose only install path is Homebrew:
// nothing is detected until the install puts the command in the fixture prefix,
// and the manifest's own CLI literal names a different place entirely, as the
// real MTPLX manifest does.
func mtplxLikeManifest(t *testing.T, engine, prefix string) *Manifest {
	t.Helper()
	manifest := testEngineManifest(fakeEngineBin)
	manifest.Engine = engine
	manifest.DisplayName = "MTPLX-like"
	platform := manifest.Platforms[hostKey()]
	platform.Detect = []string{filepath.Join(prefix, "bin", testMtplxFormula)}
	platform.Install = &Install{Pkg: &PkgInstall{
		Manager: "homebrew",
		Tap:     testMtplxTap,
		Formula: testMtplxFormula,
	}}
	manifest.Platforms[hostKey()] = platform
	require.NoError(t, manifest.Validate(), "validate the package-install fixture")
	return manifest
}

func readTranscript(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "nothing was recorded, so nothing ran")
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return lines
}

func brewCalls(lines []string) []string {
	var calls []string
	for _, line := range lines {
		if strings.HasPrefix(line, "brew ") {
			calls = append(calls, line)
		}
	}
	return calls
}

// TestInstall_PackageModeRunsTapThenTrustThenInstallThenProvesTheCommand is the
// shape of a Homebrew install: the tap has to exist, the formula has to be
// trusted, only then can it install, and PAIR checks the installed command
// answers instead of reading brew's exit code as proof.
func TestInstall_PackageModeRunsTapThenTrustThenInstallThenProvesTheCommand(t *testing.T) {
	prefix := t.TempDir()
	transcript := filepath.Join(t.TempDir(), "transcript.txt")
	require.NoError(t, os.WriteFile(transcript, nil, 0o644))
	t.Setenv("TRANSCRIPT", transcript)
	writeFakeBrew(t, prefix, brewRecordArgs+"\n"+brewLeavesWorkingMtplx)
	useFakeHomebrew(t, prefix)

	executor := newTestExecutor(t, mtplxLikeManifest(t, "mtplx-fixture", prefix))
	progress := &installProgressRecorder{}
	executor.emit = progress.emit

	require.NoError(t, executor.Install(context.Background(), "mtplx-fixture"), "package-mode install")
	progress.assertSingleTerminal(t, "done")

	assert.Equal(t, []string{
		"brew tap youssofal/mtplx",
		"brew trust --formula youssofal/mtplx/mtplx",
		"brew install youssofal/mtplx/mtplx",
		"mtplx --version",
	}, readTranscript(t, transcript), "the commands the install ran, in order")
}

// TestInstall_PackageModeGivesBrewNoAutoUpdateAndNoTerminal keeps a brew
// auto-update from spending the install's 30-minute budget on other formulas,
// and keeps a command that wants a password or a confirmation from waiting on a
// terminal nobody is watching.
func TestInstall_PackageModeGivesBrewNoAutoUpdateAndNoTerminal(t *testing.T) {
	prefix := t.TempDir()
	transcript := filepath.Join(t.TempDir(), "transcript.txt")
	require.NoError(t, os.WriteFile(transcript, nil, 0o644))
	t.Setenv("TRANSCRIPT", transcript)
	writeFakeBrew(t, prefix, brewRecordArgsAndEnv+"\n"+brewLeavesWorkingMtplx)
	useFakeHomebrew(t, prefix)

	executor := newTestExecutor(t, mtplxLikeManifest(t, "mtplx-env", prefix))

	require.NoError(t, executor.Install(context.Background(), "mtplx-env"), "package-mode install")

	calls := brewCalls(readTranscript(t, transcript))
	require.Len(t, calls, 3, "recorded Homebrew calls: %v", readTranscript(t, transcript))
	for _, call := range calls {
		assert.Contains(t, call, "HOMEBREW_NO_AUTO_UPDATE=1", "auto-update stayed on for %q", call)
		assert.Contains(t, call, "HOMEBREW_NO_ENV_HINTS=1", "environment hints stayed on for %q", call)
		assert.Contains(t, call, "tty=no", "brew was given a terminal it might wait on: %q", call)
	}
}

// TestInstall_PackageMode_ReportsMissingPackageManager is the reason a package
// mode exists rather than a one-line script: without a package manager there is
// nothing to run, and the user needs to be told that, not handed a shell error.
func TestInstall_PackageMode_ReportsMissingPackageManager(t *testing.T) {
	prefix := t.TempDir()
	transcript := filepath.Join(t.TempDir(), "transcript.txt")
	require.NoError(t, os.WriteFile(transcript, nil, 0o644))
	t.Setenv("TRANSCRIPT", transcript)
	useFakeHomebrew(t, filepath.Join(prefix, "empty"))
	t.Setenv("PATH", t.TempDir())

	executor := newTestExecutor(t, mtplxLikeManifest(t, "mtplx-nobrew", prefix))
	progress := &installProgressRecorder{}
	executor.emit = progress.emit

	err := executor.Install(context.Background(), "mtplx-nobrew")

	require.Error(t, err, "an install with no package manager must not report success")
	assert.Contains(t, err.Error(), "Homebrew", "the error should name what is missing: %v", err)
	assert.Contains(t, err.Error(), "https://brew.sh", "the error should point at the fix: %v", err)
	assert.NotContains(t, err.Error(), "no such file or directory", "the user should not be handed a shell error: %v", err)
	progress.assertSingleTerminal(t, "failed")
	_, statErr := os.Stat(filepath.Join(prefix, "bin", testMtplxFormula))
	assert.ErrorIs(t, statErr, os.ErrNotExist, "nothing should have been installed")
}

// TestInstall_PackageMode_FailsWhenTheInstalledCommandCannotRun covers the
// deferred-runtime case: brew finishes, so a runner that trusts exit 0 declares
// victory, and the same minutes of downloading then fail inside a 600-second
// Start instead. The real reason also has to reach the user.
func TestInstall_PackageMode_FailsWhenTheInstalledCommandCannotRun(t *testing.T) {
	prefix := t.TempDir()
	transcript := filepath.Join(t.TempDir(), "transcript.txt")
	require.NoError(t, os.WriteFile(transcript, nil, 0o644))
	t.Setenv("TRANSCRIPT", transcript)
	writeFakeBrew(t, prefix, brewRecordArgs+"\n"+brewLeavesRuntimeToFirstLaunch)
	useFakeHomebrew(t, prefix)

	executor := newTestExecutor(t, mtplxLikeManifest(t, "mtplx-deferred", prefix))
	progress := &installProgressRecorder{}
	executor.emit = progress.emit

	err := executor.Install(context.Background(), "mtplx-deferred")

	require.Error(t, err, "a command that cannot run is not an installed engine")
	assert.Contains(t, err.Error(), "Could not find a version that satisfies the requirement mlx==0.32.2",
		"the installer's own reason must reach the user: %v", err)
	progress.assertSingleTerminal(t, "failed")
	assert.False(t, installedByPAIR(filepath.Join(executor.baseDir, "mtplx-deferred")),
		"a Homebrew install is not PAIR's to remove")
}

// TestInstall_PackageMode_ContinuesWhenBrewHasNoTrustCommand: a Homebrew older
// than the trust gate has no trust subcommand at all, and its formulae load
// without one. Failing there would refuse a Mac that needs nothing.
func TestInstall_PackageMode_ContinuesWhenBrewHasNoTrustCommand(t *testing.T) {
	prefix := t.TempDir()
	transcript := filepath.Join(t.TempDir(), "transcript.txt")
	require.NoError(t, os.WriteFile(transcript, nil, 0o644))
	t.Setenv("TRANSCRIPT", transcript)
	writeFakeBrew(t, prefix, brewRecordArgs+`
if [ "$1" = trust ]; then
  echo "Error: Unknown command: trust"
  exit 1
fi
`+brewLeavesWorkingMtplx)
	useFakeHomebrew(t, prefix)

	executor := newTestExecutor(t, mtplxLikeManifest(t, "mtplx-oldbrew", prefix))

	require.NoError(t, executor.Install(context.Background(), "mtplx-oldbrew"),
		"a missing trust command means no trust is needed, not a failed install")

	assert.Equal(t, []string{
		"brew tap youssofal/mtplx",
		"brew trust --formula youssofal/mtplx/mtplx",
		"brew install youssofal/mtplx/mtplx",
		"mtplx --version",
	}, readTranscript(t, transcript), "the refused trust step must not stop the install")
}

// TestInstall_PackageMode_ReportsBrewRefusalOnAnUntrustedTap is the other half
// of tolerating the trust step: when the grant does not take, brew still refuses
// to install, and that refusal is the message the user needs.
func TestInstall_PackageMode_ReportsBrewRefusalOnAnUntrustedTap(t *testing.T) {
	prefix := t.TempDir()
	transcript := filepath.Join(t.TempDir(), "transcript.txt")
	require.NoError(t, os.WriteFile(transcript, nil, 0o644))
	t.Setenv("TRANSCRIPT", transcript)
	writeFakeBrew(t, prefix, brewRecordArgs+`
if [ "$1" = install ]; then
  echo "Error: Refusing to load formula youssofal/mtplx/mtplx from untrusted tap youssofal/mtplx"
  exit 1
fi
exit 0`)
	useFakeHomebrew(t, prefix)

	executor := newTestExecutor(t, mtplxLikeManifest(t, "mtplx-refused", prefix))
	progress := &installProgressRecorder{}
	executor.emit = progress.emit

	err := executor.Install(context.Background(), "mtplx-refused")

	require.Error(t, err, "a refused formula is not an installed engine")
	assert.Contains(t, err.Error(), "Refusing to load formula", "brew's own reason must be reported: %v", err)
	progress.assertSingleTerminal(t, "failed")
}

// TestInstall_PackageMode_LeavesTheInstallOwnedByHomebrew: the installed engine
// lives in Homebrew's tree, so PAIR neither records ownership nor offers to
// remove it. A marker here would let a later reset delete files PAIR does not
// own, next to the model library that lives beside them.
func TestInstall_PackageMode_LeavesTheInstallOwnedByHomebrew(t *testing.T) {
	prefix := t.TempDir()
	transcript := filepath.Join(t.TempDir(), "transcript.txt")
	require.NoError(t, os.WriteFile(transcript, nil, 0o644))
	t.Setenv("TRANSCRIPT", transcript)
	writeFakeBrew(t, prefix, brewRecordArgs+"\n"+brewLeavesWorkingMtplx)
	useFakeHomebrew(t, prefix)

	executor := newTestExecutor(t, mtplxLikeManifest(t, "mtplx-owned", prefix))
	require.NoError(t, executor.Install(context.Background(), "mtplx-owned"), "package-mode install")
	installDir := filepath.Join(executor.baseDir, "mtplx-owned")
	require.True(t, fileExists(filepath.Join(prefix, "bin", testMtplxFormula)), "the engine should still be installed")

	assert.False(t, installedByPAIR(installDir), "PAIR claimed ownership of a Homebrew-owned install")

	err := executor.Uninstall(context.Background(), "mtplx-owned")
	require.ErrorContains(t, err, "installed through Homebrew", "PAIR must not remove what Homebrew owns")
	require.ErrorContains(t, err, "brew uninstall youssofal/mtplx/mtplx",
		"the refusal has to leave the user a way to do it: %v", err)
	_, statErr := os.Stat(filepath.Join(prefix, "bin", testMtplxFormula))
	assert.NoError(t, statErr, "the engine survived an uninstall PAIR refused")
}

// TestManifestValidate_PackageModeDeclinesUninstall keeps the refusal at load
// rather than at the click: a manifest that names removal paths for a
// package-managed engine would point them at Homebrew's own tree, which is where
// the engine's shared model library lives too.
func TestManifestValidate_PackageModeDeclinesUninstall(t *testing.T) {
	prefix := t.TempDir()
	manifest := mtplxLikeManifest(t, "pkg-uninstall", prefix)
	platform := manifest.Platforms[hostKey()]
	platform.Uninstall = &Uninstall{Remove: []string{filepath.Join(prefix, "bin")}}
	manifest.Platforms[hostKey()] = platform

	err := manifest.Validate()

	require.ErrorContains(t, err, "uninstall is not allowed with install.pkg")
}

func TestManifestValidate_RejectsUnusablePackageInstall(t *testing.T) {
	prefix := t.TempDir()
	newManifest := func(pkg *PkgInstall, mutate func(*Install)) *Manifest {
		manifest := mtplxLikeManifest(t, "pkg-fixture", prefix)
		platform := manifest.Platforms[hostKey()]
		platform.Install = &Install{Pkg: pkg}
		if mutate != nil {
			mutate(platform.Install)
		}
		manifest.Platforms[hostKey()] = platform
		return manifest
	}
	usable := &PkgInstall{Manager: "homebrew", Tap: testMtplxTap, Formula: testMtplxFormula}

	require.NoError(t, newManifest(usable, nil).Validate(), "the declared package install should be accepted")

	for _, reject := range []struct {
		name     string
		pkg      *PkgInstall
		mutate   func(*Install)
		wantText string
	}{
		{name: "unknown manager", pkg: &PkgInstall{Manager: "apt", Formula: "mtplx"}, wantText: `unknown install package manager "apt"`},
		{name: "missing formula", pkg: &PkgInstall{Manager: "homebrew", Tap: testMtplxTap}, wantText: "install.pkg.formula"},
		{name: "malformed tap", pkg: &PkgInstall{Manager: "homebrew", Tap: "youssofal", Formula: "mtplx"}, wantText: "install.pkg.tap"},
		{name: "with a fetch", pkg: usable, mutate: func(i *Install) {
			i.Fetch = &Fetch{URL: "https://example.com/mtplx.tar.gz", SHA256: strings.Repeat("a", 64)}
		}, wantText: "mutually exclusive"},
		{name: "with a run command", pkg: usable, mutate: func(i *Install) {
			i.Run = []string{"/bin/sh", "-c", "brew install mtplx"}
		}, wantText: "mutually exclusive"},
	} {
		t.Run(reject.name, func(t *testing.T) {
			err := newManifest(reject.pkg, reject.mutate).Validate()
			assert.ErrorContains(t, err, reject.wantText)
		})
	}
}

// TestInstall_PackageMode_LeavesTheVendorsOwnCopyAlone is the reason a conflict
// path is not just another detect path: the application bundle is MTPLX, but it is
// not a command PAIR can run, so the engine reads as not installed and the card
// offers an install. Running Homebrew there would put a second MTPLX beside the
// first, in a tree PAIR cannot clean up, sharing the model library the first one
// already uses.
func TestInstall_PackageMode_LeavesTheVendorsOwnCopyAlone(t *testing.T) {
	prefix := t.TempDir()
	transcript := filepath.Join(t.TempDir(), "transcript.txt")
	require.NoError(t, os.WriteFile(transcript, nil, 0o644))
	t.Setenv("TRANSCRIPT", transcript)
	writeFakeBrew(t, prefix, brewRecordArgs+"\n"+brewLeavesWorkingMtplx)
	useFakeHomebrew(t, prefix)

	app := filepath.Join(t.TempDir(), "Applications", "MTPLX.app")
	require.NoError(t, os.MkdirAll(app, 0o755))

	manifest := mtplxLikeManifest(t, "mtplx-gui", prefix)
	platform := manifest.Platforms[hostKey()]
	platform.Conflicts = []string{app}
	manifest.Platforms[hostKey()] = platform

	executor := newTestExecutor(t, manifest)
	progress := &installProgressRecorder{}
	executor.emit = progress.emit

	err := executor.Install(context.Background(), "mtplx-gui")

	require.Error(t, err, "a second copy beside the user's own MTPLX is not an install")
	assert.Contains(t, err.Error(), app, "the refusal has to name what it found: %v", err)
	progress.assertSingleTerminal(t, "failed")
	assert.Empty(t, brewCalls(readTranscript(t, transcript)), "Homebrew ran beside the user's own copy")
	_, statErr := os.Stat(filepath.Join(prefix, "bin", testMtplxFormula))
	assert.ErrorIs(t, statErr, os.ErrNotExist, "nothing should have been installed")
}

// TestManifestValidate_RejectsUnusableConflictPaths keeps a conflict list that
// cannot mean anything out of a shipped manifest. A blank path matches nothing, and
// a path already in detect makes the engine read as installed — where no install is
// offered — so neither entry could ever do the one job a conflict is for.
func TestManifestValidate_RejectsUnusableConflictPaths(t *testing.T) {
	prefix := t.TempDir()
	detected := filepath.Join(prefix, "bin", testMtplxFormula)
	for _, reject := range []struct{ name, conflict, wantText string }{
		{name: "already a detect path", conflict: detected, wantText: "which detect already answers for"},
		{name: "blank", conflict: "   ", wantText: "blank path"},
	} {
		t.Run(reject.name, func(t *testing.T) {
			manifest := mtplxLikeManifest(t, "pkg-conflict", prefix)
			platform := manifest.Platforms[hostKey()]
			platform.Conflicts = []string{reject.conflict}
			manifest.Platforms[hostKey()] = platform

			assert.ErrorContains(t, manifest.Validate(), reject.wantText)
		})
	}
}

// TestMtplxManifest_InstallsThroughHomebrewOnAppleSilicon pins the shipped
// manifest: the vendor's own non-GUI channel, the trust grant that Homebrew 7
// requires first, the app bundle that means MTPLX is already here, and a start
// that waits for a model because a freshly installed MTPLX has none.
func TestMtplxManifest_InstallsThroughHomebrewOnAppleSilicon(t *testing.T) {
	registry := loadWithOverrides(t, t.TempDir())
	manifest, ok := registry.Get("mtplx")
	require.True(t, ok, "mtplx manifest not loaded")
	platform, ok := manifest.Platforms["darwin/arm64"]
	require.True(t, ok, "mtplx manifest has no darwin/arm64 platform")

	require.NotNil(t, platform.Install, "mtplx should declare an install for Apple Silicon Macs")
	assert.Equal(t, &PkgInstall{Manager: "homebrew", Tap: "youssofal/mtplx", Formula: "mtplx"}, platform.Install.Pkg)
	assert.True(t, platform.Runtime.WaitForModel,
		"a new MTPLX install has no model, so a start must not fail for that reason")
	assert.Equal(t, []string{"/Applications/MTPLX.app"}, platform.Conflicts,
		"a downloaded MTPLX should stop PAIR installing a second one")
	// The app bundle is a folder, not a command: it has to stay out of detect,
	// whose first hit becomes the binary PAIR launches and owns.
	assert.NotContains(t, platform.Detect, "/Applications/MTPLX.app")
}
