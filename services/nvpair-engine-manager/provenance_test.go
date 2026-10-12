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
)

func TestInstallMarkerRoundTrip(t *testing.T) {
	installDir := filepath.Join(t.TempDir(), "lmstudio")
	if installedByPAIR(installDir) {
		t.Fatal("an install directory that does not exist cannot be ours")
	}
	if err := writeInstallMarker(installDir, "lmstudio"); err != nil {
		t.Fatalf("writeInstallMarker: %v", err)
	}
	if !installedByPAIR(installDir) {
		t.Error("marker written but not recognized")
	}
	clearInstallMarker(installDir)
	if installedByPAIR(installDir) {
		t.Error("marker survived clearInstallMarker; a stale claim lets PAIR remove a user's own install")
	}
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
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
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
	if len(results) != 1 || results[0].Removed || results[0].Error != "" {
		t.Fatalf("an unmarked engine should be skipped without error, got %+v", results)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("removed an engine PAIR did not install: %v", err)
	}

	installDir := filepath.Join(ex.baseDir, "vendor")
	if err := writeInstallMarker(installDir, "vendor"); err != nil {
		t.Fatal(err)
	}
	results = ex.UninstallManaged(context.Background())

	if len(results) != 1 || !results[0].Removed || results[0].Error != "" {
		t.Fatalf("expected the marked engine to be removed, got %+v", results)
	}
	if _, err := os.Stat(binary); !os.IsNotExist(err) {
		t.Errorf("engine binary survived (err=%v)", err)
	}
	if _, err := os.Stat(weights); err != nil {
		t.Errorf("removal deleted downloaded models: %v", err)
	}
	if installedByPAIR(installDir) {
		t.Error("install marker not cleared after a successful removal")
	}
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
	if err := writeInstallMarker(installDir, "vendor"); err != nil {
		t.Fatal(err)
	}

	results := ex.UninstallManaged(context.Background())

	if len(results) != 1 || results[0].Removed || results[0].Error == "" {
		t.Fatalf("expected a reported failure, got %+v", results)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Errorf("engine binary vanished despite the reported failure: %v", err)
	}
	if !installedByPAIR(installDir) {
		t.Error("marker cleared after a failed removal; the engine is now unremovable")
	}
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
	if err := os.MkdirAll(filepath.Dir(cache), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The engine's bin directory cannot be emptied, so the binary survives and
	// the engine is still detected. Its cache can be, and goes.
	if err := os.Chmod(filepath.Dir(stuckBinary), 0o500); err != nil {
		t.Fatal(err)
	}
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
		if err := writeInstallMarker(filepath.Join(ex.baseDir, engine), engine); err != nil {
			t.Fatal(err)
		}
	}

	outcomes := map[string]ManagedUninstall{}
	for _, outcome := range ex.UninstallManaged(context.Background()) {
		outcomes[outcome.Engine] = outcome
	}

	if got := outcomes[stuck.Engine]; got.Removed || got.Error == "" {
		t.Errorf("the partly removed engine reported %+v, want a failure", got)
	}
	if !installedByPAIR(filepath.Join(ex.baseDir, stuck.Engine)) {
		t.Error("the partly removed engine lost its marker, so nothing may finish removing it")
	}
	if _, err := os.Stat(stuckBinary); err != nil {
		t.Errorf("the undeletable binary is gone, so this did not exercise a partial removal: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(cache)); !os.IsNotExist(err) {
		t.Errorf("the deletable part of the engine survived (err=%v)", err)
	}
	if _, err := os.Stat(stuckWeights); err != nil {
		t.Errorf("a failed removal deleted downloaded models: %v", err)
	}

	if got := outcomes[clean.Engine]; !got.Removed || got.Error != "" {
		t.Errorf("the engine after the failure reported %+v, want it removed", got)
	}
	if _, err := os.Stat(cleanBinary); !os.IsNotExist(err) {
		t.Errorf("the engine after the failure survived (err=%v)", err)
	}
}

// TestUninstallManagedWithoutDataDirRemovesNothing covers losing the app data
// directory, which is where ownership is recorded. With no way to tell whose
// install an engine is, the safe answer is to remove none of them.
func TestUninstallManagedWithoutDataDirRemovesNothing(t *testing.T) {
	vendorRoot := t.TempDir()
	binary, _ := vendorEngineOnDisk(t, vendorRoot)
	ex := managedExecutor(t, vendorEngineManifest(vendorRoot, filepath.Join(vendorRoot, "models"), []string{vendorRoot}))
	ex.baseDir = ""

	if results := ex.UninstallManaged(context.Background()); results != nil {
		t.Errorf("expected no results without a data directory, got %+v", results)
	}
	if _, err := os.Stat(binary); err != nil {
		t.Errorf("removed an engine with no ownership records available: %v", err)
	}
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
	if err == nil {
		t.Fatal("expected uninstall to decline an engine PAIR has no record of installing")
	}
	if _, statErr := os.Stat(filepath.Join(vendorRoot, "bin", "engine")); statErr != nil {
		t.Errorf("declined but still removed files: %v", statErr)
	}
}

// TestUninstallClearsTheMarkerWhenAlreadyGone covers the engine removed by its
// own uninstaller. Leaving the claim behind would authorise removing a copy the
// user installs later.
func TestUninstallClearsTheMarkerWhenAlreadyGone(t *testing.T) {
	vendorRoot := t.TempDir() // nothing laid down, so detection fails
	ex := managedExecutor(t, vendorEngineManifest(vendorRoot, filepath.Join(vendorRoot, "models"), []string{vendorRoot}))
	installDir := filepath.Join(ex.baseDir, "vendor")
	if err := writeInstallMarker(installDir, "vendor"); err != nil {
		t.Fatal(err)
	}

	if err := ex.Uninstall(context.Background(), "vendor"); err != nil {
		t.Fatalf("uninstalling an absent engine should succeed: %v", err)
	}
	if installedByPAIR(installDir) {
		t.Error("stale marker left behind for an engine that is already gone")
	}
}
