// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
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

// managedEngineRegistry builds a registry with one vendor-script engine whose
// files land outside the install directory, which is the LM Studio shape the
// install marker exists for.
func managedEngineRegistry(t *testing.T, vendorRoot, modelsDir string) *Registry {
	t.Helper()
	dir := t.TempDir()
	stop := []string{"true"}
	if runtime.GOOS == "windows" {
		stop = []string{"cmd", "/c", "exit", "0"}
	}
	writeManifest(t, dir, "vendor.json", Manifest{
		Engine:          "vendor",
		DisplayName:     "Vendor Engine",
		ManifestVersion: 1,
		Platforms: map[string]Platform{
			runtime.GOOS + "/" + runtime.GOARCH: {
				Detect:    []string{filepath.Join(vendorRoot, "bin", "engine")},
				ModelsDir: modelsDir,
				Uninstall: &Uninstall{Remove: []string{vendorRoot}},
				Runtime: Runtime{
					Mode:  "command",
					Start: [][]string{stop},
					Stop:  &StopSpec{Cmd: stop},
				},
			},
		},
	})
	reg := NewRegistry()
	if err := reg.LoadDir(dir); err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	return reg
}

func TestUninstallManagedEnginesRemovesOnlyWhatPAIRInstalled(t *testing.T) {
	root := t.TempDir()
	vendorRoot := filepath.Join(root, "vendor-home")
	modelsDir := filepath.Join(vendorRoot, "models")
	installBase := filepath.Join(root, "engine-bin")

	binary := filepath.Join(vendorRoot, "bin", "engine")
	weights := filepath.Join(modelsDir, "model.gguf")
	for _, file := range []string{binary, weights} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg := managedEngineRegistry(t, vendorRoot, modelsDir)

	// No marker: this is the user's own install and PAIR must leave it alone.
	uninstallManagedEngines(context.Background(), reg, installBase)
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("removed an engine PAIR did not install: %v", err)
	}

	if err := writeInstallMarker(filepath.Join(installBase, "vendor"), "vendor"); err != nil {
		t.Fatal(err)
	}
	uninstallManagedEngines(context.Background(), reg, installBase)

	if _, err := os.Stat(binary); !os.IsNotExist(err) {
		t.Errorf("engine binary survived (err=%v)", err)
	}
	if _, err := os.Stat(weights); err != nil {
		t.Errorf("app uninstall deleted downloaded models: %v", err)
	}
	if installedByPAIR(filepath.Join(installBase, "vendor")) {
		t.Error("install marker not cleared after removal")
	}
}

// TestUninstallManagedEnginesWithoutDataDirRemovesNothing covers losing the app
// data directory, which is where ownership is recorded. With no way to tell
// whose install an engine is, the safe answer is to remove none of them.
func TestUninstallManagedEnginesWithoutDataDirRemovesNothing(t *testing.T) {
	vendorRoot := t.TempDir()
	binary := filepath.Join(vendorRoot, "bin", "engine")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := managedEngineRegistry(t, vendorRoot, filepath.Join(vendorRoot, "models"))

	uninstallManagedEngines(context.Background(), reg, "")

	if _, err := os.Stat(binary); err != nil {
		t.Errorf("removed an engine with no ownership records available: %v", err)
	}
}
