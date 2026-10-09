// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInstallMarkerRoundTrip(t *testing.T) {
	installDir := filepath.Join(t.TempDir(), "lmstudio")
	require.False(t, installedByPAIR(installDir), "an install directory that does not exist cannot be ours")
	require.NoError(t, writeInstallMarker(installDir, "lmstudio"), "writeInstallMarker")
	assert.True(t, installedByPAIR(installDir), "marker written but not recognized")
	clearInstallMarker(installDir)
	assert.False(t, installedByPAIR(installDir), "marker survived clearInstallMarker; a stale claim lets PAIR remove a user's own install")
}

// vendorEngineManifest describes one vendor-script engine whose files land
// outside the install directory, which is the LM Studio shape the install
// marker exists for. removeTargets empty leaves the engine in place, which is
// how a failing uninstall is reproduced.
func vendorEngineManifest(vendorRoot, modelsDir string, removeTargets []string) *Manifest {
	ok := []string{"true"}
	fail := []string{"false"}
	if runtime.GOOS == "windows" {
		ok = []string{"cmd", "/c", "exit", "0"}
		fail = []string{"cmd", "/c", "exit", "1"}
	}
	uninstall := &Uninstall{Remove: removeTargets}
	if len(removeTargets) == 0 {
		uninstall = &Uninstall{Run: fail}
	}
	return &Manifest{
		Engine:          "vendor",
		DisplayName:     "Vendor Engine",
		ManifestVersion: 1,
		Platforms: map[string]Platform{
			runtime.GOOS + "/" + runtime.GOARCH: {
				Detect:    []string{filepath.Join(vendorRoot, "bin", "engine")},
				ModelsDir: modelsDir,
				Uninstall: uninstall,
				Runtime: Runtime{
					Mode:  "command",
					Port:  45999,
					Start: [][]string{ok},
					Stop:  &StopSpec{Cmd: ok},
				},
			},
		},
	}
}

// vendorEngineOnDisk lays down an engine binary and a downloaded model, and
// returns both paths.
func vendorEngineOnDisk(t *testing.T, vendorRoot string) (binary, weights string) {
	t.Helper()
	binary = filepath.Join(vendorRoot, "bin", "engine")
	weights = filepath.Join(vendorRoot, "models", "publisher", "model.gguf")
	for _, file := range []string{binary, weights} {
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
		require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	}
	return binary, weights
}

func managedExecutor(t *testing.T, manifest *Manifest) *Executor {
	t.Helper()
	ex := newTestExecutor(t, manifest)
	ex.detectTimeout = 100 * time.Millisecond
	return ex
}

func TestUninstallManagedRemovesOnlyWhatPAIRInstalled(t *testing.T) {
	vendorRoot := t.TempDir()
	binary, weights := vendorEngineOnDisk(t, vendorRoot)
	ex := managedExecutor(t, vendorEngineManifest(vendorRoot, filepath.Join(vendorRoot, "models"), []string{vendorRoot}))

	// No marker: the user's own install, which PAIR must leave alone and must
	// not report as a failure.
	results := ex.UninstallManaged(context.Background())
	require.Len(t, results, 1, "unmarked engine outcome")
	require.False(t, results[0].Removed, "an unmarked engine should be skipped")
	require.Empty(t, results[0].Error, "an unmarked engine should be skipped without error")
	require.FileExists(t, binary, "removed an engine PAIR did not install")

	installDir := filepath.Join(ex.baseDir, "vendor")
	require.NoError(t, writeInstallMarker(installDir, "vendor"))
	results = ex.UninstallManaged(context.Background())

	require.Len(t, results, 1, "marked engine outcome")
	assert.True(t, results[0].Removed, "expected the marked engine to be removed")
	assert.Empty(t, results[0].Error, "marked engine removal")
	_, err := os.Stat(binary)
	assert.ErrorIs(t, err, os.ErrNotExist, "engine binary survived")
	assert.FileExists(t, weights, "removal deleted downloaded models")
	assert.False(t, installedByPAIR(installDir), "install marker not cleared after a successful removal")
}

// TestUninstallManagedKeepsTheMarkerWhenRemovalFails is the regression guard for
// losing ownership. The marker is the only record that an engine is PAIR's, so
// clearing it after a failure would leave the engine installed and every future
// uninstall refusing it.
func TestUninstallManagedKeepsTheMarkerWhenRemovalFails(t *testing.T) {
	oldRetries, oldBackoff := uninstallRetries, uninstallBackoff
	uninstallRetries, uninstallBackoff = 1, time.Millisecond
	defer func() { uninstallRetries, uninstallBackoff = oldRetries, oldBackoff }()

	vendorRoot := t.TempDir()
	binary, _ := vendorEngineOnDisk(t, vendorRoot)
	// No remove targets, so the engine is still detected afterwards.
	ex := managedExecutor(t, vendorEngineManifest(vendorRoot, filepath.Join(vendorRoot, "models"), nil))
	installDir := filepath.Join(ex.baseDir, "vendor")
	require.NoError(t, writeInstallMarker(installDir, "vendor"))

	results := ex.UninstallManaged(context.Background())

	require.Len(t, results, 1, "expected a reported failure")
	assert.False(t, results[0].Removed, "failed removal must not report success")
	assert.NotEmpty(t, results[0].Error, "expected a reported failure")
	assert.FileExists(t, binary, "engine binary vanished despite the reported failure")
	assert.True(t, installedByPAIR(installDir), "marker cleared after a failed removal; the engine is now unremovable")
}

// TestUninstallManagedCarriesOnPastAPartialRemoval covers an engine whose files
// are only partly deleted. Its uninstall fails, so it keeps the install marker
// and reports the failure, and the engine after it is still removed: one stuck
// engine must not leave the others behind, and the caller decides what a
// failure means for the rest of its work.
func TestUninstallManagedCarriesOnPastAPartialRemoval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	if runtime.GOOS == "windows" {
		t.Skip("a read-only directory does not stop a delete on Windows")
	}
	oldRetries, oldBackoff := uninstallRetries, uninstallBackoff
	uninstallRetries, uninstallBackoff = 1, time.Millisecond
	defer func() { uninstallRetries, uninstallBackoff = oldRetries, oldBackoff }()

	stuckRoot := t.TempDir()
	stuckBinary, stuckWeights := vendorEngineOnDisk(t, stuckRoot)
	cache := filepath.Join(stuckRoot, "cache", "blob")
	require.NoError(t, os.MkdirAll(filepath.Dir(cache), 0o755))
	require.NoError(t, os.WriteFile(cache, []byte("x"), 0o644))
	// The engine's bin directory cannot be emptied, so the binary survives and
	// the engine is still detected. Its cache can be, and goes.
	require.NoError(t, os.Chmod(filepath.Dir(stuckBinary), 0o500))
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(stuckBinary), 0o700) })
	stuck := vendorEngineManifest(stuckRoot, filepath.Join(stuckRoot, "models"), []string{stuckRoot})
	stuck.Engine = "stuck"

	cleanRoot := t.TempDir()
	cleanBinary, _ := vendorEngineOnDisk(t, cleanRoot)
	clean := vendorEngineManifest(cleanRoot, filepath.Join(cleanRoot, "models"), []string{cleanRoot})
	clean.Engine = "clean"

	reg := NewRegistry()
	reg.engines[stuck.Engine] = stuck
	reg.engines[clean.Engine] = clean
	ex := NewExecutor(reg, NewReporter(nil), func(string, any) {}, t.TempDir())
	ex.detectTimeout = 100 * time.Millisecond
	for _, engine := range []string{stuck.Engine, clean.Engine} {
		require.NoError(t, writeInstallMarker(filepath.Join(ex.baseDir, engine), engine))
	}

	outcomes := map[string]ManagedUninstall{}
	for _, outcome := range ex.UninstallManaged(context.Background()) {
		outcomes[outcome.Engine] = outcome
	}

	assert.False(t, outcomes[stuck.Engine].Removed, "the partly removed engine must report a failure")
	assert.NotEmpty(t, outcomes[stuck.Engine].Error, "the partly removed engine must report a failure")
	assert.True(t, installedByPAIR(filepath.Join(ex.baseDir, stuck.Engine)), "the partly removed engine lost its marker, so nothing may finish removing it")
	assert.FileExists(t, stuckBinary, "the undeletable binary is gone, so this did not exercise a partial removal")
	_, err := os.Stat(filepath.Dir(cache))
	assert.ErrorIs(t, err, os.ErrNotExist, "the deletable part of the engine survived")
	assert.FileExists(t, stuckWeights, "a failed removal deleted downloaded models")

	assert.True(t, outcomes[clean.Engine].Removed, "the engine after the failure must be removed")
	assert.Empty(t, outcomes[clean.Engine].Error, "the engine after the failure must be removed")
	_, err = os.Stat(cleanBinary)
	assert.ErrorIs(t, err, os.ErrNotExist, "the engine after the failure survived")
}

// TestUninstallManagedWithoutDataDirRemovesNothing covers losing the app data
// directory, which is where ownership is recorded. With no way to tell whose
// install an engine is, the safe answer is to remove none of them.
func TestUninstallManagedWithoutDataDirRemovesNothing(t *testing.T) {
	vendorRoot := t.TempDir()
	binary, _ := vendorEngineOnDisk(t, vendorRoot)
	ex := managedExecutor(t, vendorEngineManifest(vendorRoot, filepath.Join(vendorRoot, "models"), []string{vendorRoot}))
	ex.baseDir = ""

	assert.Empty(t, ex.UninstallManaged(context.Background()), "expected no results without a data directory")
	assert.FileExists(t, binary, "removed an engine with no ownership records available")
}

// TestUninstallDeclinesUnmarkedCommandEngine pins the interactive refusal. A
// vendor script picks its own destination, so the managed-path test can never
// vouch for a command-mode engine; without a marker this could be the user's
// own install, holding the model library they built up in it.
func TestUninstallDeclinesUnmarkedCommandEngine(t *testing.T) {
	vendorRoot := t.TempDir()
	vendorEngineOnDisk(t, vendorRoot)
	ex := managedExecutor(t, vendorEngineManifest(vendorRoot, filepath.Join(vendorRoot, "models"), []string{vendorRoot}))

	err := ex.Uninstall(context.Background(), "vendor")
	require.Error(t, err, "expected uninstall to decline an engine PAIR has no record of installing")
	assert.FileExists(t, filepath.Join(vendorRoot, "bin", "engine"), "declined but still removed files")
}

// TestUninstallClearsTheMarkerWhenAlreadyGone covers the engine removed by its
// own uninstaller. Leaving the claim behind would authorise removing a copy the
// user installs later.
func TestUninstallClearsTheMarkerWhenAlreadyGone(t *testing.T) {
	vendorRoot := t.TempDir() // nothing laid down, so detection fails
	ex := managedExecutor(t, vendorEngineManifest(vendorRoot, filepath.Join(vendorRoot, "models"), []string{vendorRoot}))
	installDir := filepath.Join(ex.baseDir, "vendor")
	require.NoError(t, writeInstallMarker(installDir, "vendor"))

	require.NoError(t, ex.Uninstall(context.Background(), "vendor"), "uninstalling an absent engine should succeed")
	assert.False(t, installedByPAIR(installDir), "stale marker left behind for an engine that is already gone")
}
