// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"nvpair-shared/appdir"
)

// TestBundledModelStoresOutliveTheAppDataRoot is the regression guard for the
// uninstall that deleted downloaded models. The app-level "remove all data"
// uninstall deletes the whole app data root, so a model store anywhere inside it
// is destroyed even though no engine uninstall touched it — which is exactly
// what llama.cpp's cache did when it sat beside the install directory.
//
// The app data root always ends in the vendor/product segments, so this holds
// the line for every target platform rather than only the test host's.
func TestBundledModelStoresOutliveTheAppDataRoot(t *testing.T) {
	forbidden := []string{"nvidia corporation", "personal ai router"}
	for name, manifest := range bundledManifestSet(t) {
		for key, platform := range manifest.Platforms {
			if platform.ModelsDir == "" {
				t.Errorf("%s/%s: no models_dir — declare the engine's model store so uninstall knows what to keep", name, key)
				continue
			}
			resolved := strings.ToLower(filepath.ToSlash(expandPath(platform.ModelsDir)))
			for _, segment := range forbidden {
				if strings.Contains(resolved, segment) {
					t.Errorf("%s/%s: models_dir %q is inside the app data root; the app uninstall would delete the user's models", name, key, platform.ModelsDir)
					break
				}
			}
		}
		platform, ok := manifest.HostPlatform()
		if !ok {
			continue
		}
		root, err := appdir.Dir()
		if err != nil {
			t.Skipf("no app data dir on %s: %v", runtime.GOOS, err)
		}
		if pathWithinRoot(root, expandPath(platform.ModelsDir)) {
			t.Errorf("%s: models_dir %q resolves under the app data root %q", name, platform.ModelsDir, root)
		}
	}
}

// TestBundledUninstallsKeepTheModelStore checks the other removal path: an
// engine uninstall may remove a directory that contains the model store (LM
// Studio keeps both under ~/.lmstudio), but never the store itself.
func TestBundledUninstallsKeepTheModelStore(t *testing.T) {
	for name, manifest := range bundledManifestSet(t) {
		for key, platform := range manifest.Platforms {
			if platform.Uninstall == nil {
				continue
			}
			for _, target := range platform.Uninstall.Remove {
				if filepath.Clean(expandPath(target)) == filepath.Clean(expandPath(platform.ModelsDir)) {
					t.Errorf("%s/%s: uninstall.remove %q is the model store", name, key, target)
				}
			}
			// An rm/rmdir left in Run is unreviewable: the runner cannot tell
			// what it deletes, so the preserve guarantee would not apply to it.
			for _, arg := range platform.Uninstall.Run {
				if base := strings.ToLower(filepath.Base(arg)); base == "rm" || base == "rmdir" {
					t.Errorf("%s/%s: uninstall.run shells out to %q — use uninstall.remove so the model store is preserved", name, key, arg)
				}
			}
		}
	}
}

func TestRemoveTreePreservingKeepsNestedStore(t *testing.T) {
	root := t.TempDir()
	engineRoot := filepath.Join(root, ".lmstudio")
	models := filepath.Join(engineRoot, "models", "publisher", "repo")
	binary := filepath.Join(engineRoot, "bin", "lms")
	internal := filepath.Join(engineRoot, ".internal", "state.json")
	for _, dir := range []string{models, filepath.Dir(binary), filepath.Dir(internal)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	weights := filepath.Join(models, "model.gguf")
	for _, file := range []string{weights, binary, internal} {
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := removeTreePreserving(engineRoot, filepath.Join(engineRoot, "models")); err != nil {
		t.Fatalf("removeTreePreserving: %v", err)
	}

	if _, err := os.Stat(weights); err != nil {
		t.Errorf("model weights were removed: %v", err)
	}
	for _, gone := range []string{binary, internal, filepath.Join(engineRoot, "bin")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%q survived the uninstall (err=%v)", gone, err)
		}
	}
}

func TestRemoveTreePreservingRemovesUnrelatedTree(t *testing.T) {
	root := t.TempDir()
	installDir := filepath.Join(root, "engine-bin", "ollama")
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "ollama"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Ollama's models live outside the install dir, so this is a plain removal.
	if err := removeTreePreserving(installDir, filepath.Join(root, ".ollama")); err != nil {
		t.Fatalf("removeTreePreserving: %v", err)
	}
	if _, err := os.Stat(installDir); !os.IsNotExist(err) {
		t.Errorf("install dir survived (err=%v)", err)
	}
}

func TestRemoveTreePreservingRefusesTheStoreItself(t *testing.T) {
	root := t.TempDir()
	if err := removeTreePreserving(root, root); err == nil {
		t.Error("expected an error when the target is the preserved model store")
	}
}

func TestRemoveTreePreservingMissingTargetIsNoOp(t *testing.T) {
	if err := removeTreePreserving(filepath.Join(t.TempDir(), "absent"), ""); err != nil {
		t.Errorf("removing an absent path should be a no-op, got %v", err)
	}
}
