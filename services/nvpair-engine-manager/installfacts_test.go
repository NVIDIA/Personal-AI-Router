// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// The engine card decides what to offer from two facts this service reports: where
// the copy that is already here came from, and whether there is any way to obtain
// one where there is nothing. Both are answers about the machine, not about the
// interface, so these tests stand the machine up: a throwaway package-manager
// prefix holding a shell-script `brew`, a fake application bundle, and engine
// binaries placed in the two places that matter — inside NVPAIR's own install
// directory, and outside it.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newEngineState builds the state the install facts read. The platform is copied,
// so a table of cases never shares one through the back door.
func newEngineState(t *testing.T, installed bool, binPath, installDir, mode string, plat *Platform) *engineState {
	t.Helper()
	platform := *plat
	platform.Runtime.Mode = mode
	return &engineState{
		installed:  installed,
		binPath:    binPath,
		installDir: installDir,
		plat:       &platform,
	}
}

// homebrewPlatform is a platform whose only way in is Homebrew, and whose formula
// would land under the prefix the test names.
func homebrewPlatform() *Platform {
	plat := &Platform{}
	plat.Install = &Install{Pkg: &PkgInstall{Manager: "homebrew", Formula: testMtplxFormula}}
	return plat
}

// downloadPlatform is a platform PAIR installs itself: it downloads a verified
// archive and unpacks it into its own install directory.
func downloadPlatform() *Platform {
	plat := &Platform{}
	plat.Install = &Install{Fetch: &Fetch{URL: "https://example.invalid/thing.tar.gz"}}
	return plat
}

func TestInstallOwner_NamesWhoOwnsTheCopyThatIsHere(t *testing.T) {
	// Each case gets its own install directory: the install marker that makes an
	// engine PAIR's own is a file, and one case writing it would otherwise be
	// the next case's answer.
	brewPrefix := t.TempDir()
	home := t.TempDir()
	managedDir := filepath.Join(home, "managed")
	unmanagedDir := filepath.Join(home, "elsewhere")
	commandOwnedDir := filepath.Join(home, "command-owned")
	commandPlainDir := filepath.Join(home, "command-plain")
	for _, dir := range []string{managedDir, unmanagedDir, commandOwnedDir, commandPlainDir} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	useFakeHomebrew(t, brewPrefix)

	for _, claim := range []struct {
		name      string
		pairMade  bool // NVPAIR recorded an install marker in its own directory
		state     *engineState
		wantOwner string
	}{
		{
			name:      "nothing is installed",
			state:     newEngineState(t, false, "", "", "process", downloadPlatform()),
			wantOwner: "",
		},
		{
			name:      "NVPAIR put this copy in its own install directory",
			state:     newEngineState(t, true, filepath.Join(managedDir, testMtplxFormula), managedDir, "process", downloadPlatform()),
			wantOwner: installOwnerPAIR,
		},
		{
			name:      "Homebrew put this copy in its own prefix",
			state:     newEngineState(t, true, filepath.Join(brewPrefix, "bin", testMtplxFormula), managedDir, "process", homebrewPlatform()),
			wantOwner: "Homebrew",
		},
		{
			name:      "the user made this copy themselves, outside any prefix",
			state:     newEngineState(t, true, filepath.Join(home, ".mtplx", "bin", testMtplxFormula), managedDir, "process", homebrewPlatform()),
			wantOwner: installOwnerOther,
		},
		{
			name:     "a command-mode engine NVPAIR installed still carries its marker",
			pairMade: true,
			state:    newEngineState(t, true, filepath.Join(unmanagedDir, testMtplxFormula), commandOwnedDir, "command", downloadPlatform()),
			// A command-mode engine picks its own destination, so the marker is
			// the only record of who put it there.
			wantOwner: installOwnerPAIR,
		},
		{
			name:      "a command-mode engine with no marker was not installed here",
			state:     newEngineState(t, true, filepath.Join(unmanagedDir, testMtplxFormula), commandPlainDir, "command", downloadPlatform()),
			wantOwner: installOwnerOther,
		},
		{
			name:      "only a process answering on the port is here",
			state:     newEngineState(t, true, "", unmanagedDir, "process", downloadPlatform()),
			wantOwner: installOwnerOther,
		},
	} {
		t.Run(claim.name, func(t *testing.T) {
			if claim.pairMade {
				require.NoError(t, writeInstallMarker(claim.state.installDir, "mtplx-fixture"))
			}
			assert.Equal(t, claim.wantOwner, installOwner(claim.state),
				"Uninstall is offered only where Uninstall would work")
		})
	}
}

func TestCanInstallHere_OffersOnlyAnInstallThatCouldSucceed(t *testing.T) {
	brewPrefix := t.TempDir()
	appBundle := filepath.Join(t.TempDir(), "Applications", "MTPLX.app")
	require.NoError(t, os.MkdirAll(appBundle, 0o755))

	useFakeHomebrew(t, brewPrefix)
	withBrew := func() *Platform {
		writeFakeBrew(t, brewPrefix, brewRecordArgs)
		return homebrewPlatform()
	}

	for _, claim := range []struct {
		name  string
		state *engineState
		want  bool
	}{
		{
			name:  "nothing on this platform can install it",
			state: newEngineState(t, false, "", "", "process", &Platform{}),
			want:  false,
		},
		{
			name:  "a verified download PAIR can fetch itself",
			state: newEngineState(t, false, "", "", "process", downloadPlatform()),
			want:  true,
		},
		{
			name:  "Homebrew is here to install the formula",
			state: newEngineState(t, false, "", "", "process", withBrew()),
			want:  true,
		},
		{
			name: "the engine is already here, in the vendor's own application",
			state: newEngineState(t, false, "", "", "process", func() *Platform {
				plat := withBrew()
				plat.Conflicts = []string{appBundle}
				return plat
			}()),
			want: false,
		},
		{
			name: "an admin install, which this user-mode service refuses",
			state: newEngineState(t, false, "", "", "process", func() *Platform {
				plat := downloadPlatform()
				plat.Install.Mode = "admin"
				return plat
			}()),
			want: false,
		},
		{
			name:  "the engine is already installed",
			state: newEngineState(t, true, "", "", "process", downloadPlatform()),
			want:  false,
		},
	} {
		t.Run(claim.name, func(t *testing.T) {
			assert.Equal(t, claim.want, canInstallHere(claim.state))
		})
	}
}

func TestCanInstallHere_RefusesWithoutThePackageManager(t *testing.T) {
	brewPrefix := t.TempDir() // a prefix with no brew in it
	useFakeHomebrew(t, brewPrefix)

	state := newEngineState(t, false, "", "", "process", homebrewPlatform())

	assert.False(t, canInstallHere(state),
		"an install that can only end in \"this Mac does not have Homebrew\" is not an install")
}

// TestStatus_ReportsTheInstallFactsForThisMachine walks one engine from nothing
// installed, through a Homebrew install, to a copy the machine owns. It also pins
// the names the desktop reads, because a renamed field reads as a missing one.
func TestStatus_ReportsTheInstallFactsForThisMachine(t *testing.T) {
	prefix := t.TempDir()
	transcript := filepath.Join(t.TempDir(), "transcript.txt")
	require.NoError(t, os.WriteFile(transcript, nil, 0o644))
	t.Setenv("TRANSCRIPT", transcript)
	writeFakeBrew(t, prefix, brewRecordArgs+"\n"+brewLeavesWorkingMtplx)
	useFakeHomebrew(t, prefix)

	executor := newTestExecutor(t, mtplxLikeManifest(t, "mtplx-fixture", prefix))

	before, err := executor.Status("mtplx-fixture")
	require.NoError(t, err)
	assert.False(t, before.Installed)
	assert.True(t, before.CanInstall,
		"Homebrew is in the fixture prefix, so this engine is installable here")
	assert.Empty(t, before.Owner, "nothing is installed, so nobody owns anything yet")
	rawBefore, err := json.Marshal(before)
	require.NoError(t, err)
	assert.Contains(t, string(rawBefore), `"can_install":true`)

	require.NoError(t, executor.Install(context.Background(), "mtplx-fixture"))

	after, err := executor.Status("mtplx-fixture")
	require.NoError(t, err)
	require.True(t, after.Installed, "the fixture installs into the detected path")
	assert.Equal(t, "Homebrew", after.Owner,
		"the files are Homebrew's, so PAIR must not offer to remove them")
	assert.False(t, after.CanInstall, "it is installed")
	rawAfter, err := json.Marshal(after)
	require.NoError(t, err)
	assert.Contains(t, string(rawAfter), `"owner":"Homebrew"`)
}
