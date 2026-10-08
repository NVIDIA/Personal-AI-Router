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
			for _, arg := range platform.Uninstall.Run {
				if token := destructiveToken(arg); token != "" {
					t.Errorf("%s/%s: uninstall.run deletes files with %q — use uninstall.remove so the model store is preserved", name, key, token)
				}
			}
		}
	}
}

// destructiveToken reports the file-deleting construct in a single uninstall.run
// argument, or "" when there is none.
//
// Scanned as a substring rather than by executable name. A manifest's deletion
// almost never arrives as argv[0]: the LM Studio uninstall this guard exists for
// was a whole shell script in one `sh -c` argument, whose base name was the tail
// of the script, and the Windows form buried Remove-Item inside a PowerShell
// -Command string the same way.
func destructiveToken(arg string) string {
	lowered := strings.ToLower(arg)
	for _, token := range []string{"rm -r", "rm -f", "rmdir", "rd /s", "remove-item", "del /", "unlink "} {
		if strings.Contains(lowered, token) {
			return token
		}
	}
	// Bare `rm` as the executable, which the substring forms above miss.
	if strings.ToLower(filepath.Base(arg)) == "rm" {
		return "rm"
	}
	return ""
}

// TestDestructiveTokenCatchesTheOriginalUninstalls is the regression case for
// the guard above: both spellings of the uninstall that deleted users' model
// libraries have to be recognised, or the guard documents a protection it does
// not provide.
func TestDestructiveTokenCatchesTheOriginalUninstalls(t *testing.T) {
	originals := map[string]string{
		"posix": `pkill -x lms 2>/dev/null; pkill -x llmster 2>/dev/null; sleep 2; rm -rf "$HOME/.lmstudio"; sleep 1; [ -d "$HOME/.lmstudio" ] && exit 1; exit 0`,
		"windows": `$root = Join-Path $env:USERPROFILE '.lmstudio'; Start-Sleep -Seconds 3; ` +
			`Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue`,
	}
	for name, argument := range originals {
		if destructiveToken(argument) == "" {
			t.Errorf("%s: the original LM Studio uninstall was not recognised as deleting files", name)
		}
	}
	for _, harmless := range []string{
		"pkill -x lms 2>/dev/null; pkill -x llmster 2>/dev/null; sleep 2; exit 0",
		"server",
		"--strip-components=1",
	} {
		if token := destructiveToken(harmless); token != "" {
			t.Errorf("%q flagged on %q, but it deletes nothing", token, harmless)
		}
	}
}

// TestValidateRejectsRemovalsThatReachTheModelStore pins the load-time
// rejections the manifest reference promises. Each of these was accepted before,
// and each deletes part or all of a user's model library at uninstall time.
func TestValidateRejectsRemovalsThatReachTheModelStore(t *testing.T) {
	for name, uninstall := range map[string]struct {
		modelsDir string
		remove    []string
	}{
		"no store to preserve":  {modelsDir: "", remove: []string{"~/.engine"}},
		"the store itself":      {modelsDir: "~/.engine/models", remove: []string{"~/.engine/models"}},
		"inside the store":      {modelsDir: "~/.engine/models", remove: []string{"~/.engine/models/publisher"}},
		"the store, templated":  {modelsDir: "~/.engine/models", remove: []string{"{models_dir}"}},
		"inside it, templated":  {modelsDir: "~/.engine/models", remove: []string{"{models_dir}/publisher"}},
		"the store, mixed case": {modelsDir: "~/.engine/models", remove: []string{"~/.engine/Models"}},
	} {
		platform := Platform{
			Detect:    []string{"{install_dir}/engine"},
			ModelsDir: uninstall.modelsDir,
			Uninstall: &Uninstall{Remove: uninstall.remove},
			Runtime:   Runtime{Bin: "{install_dir}/engine"},
		}
		err := platform.validate(runtime.GOOS + "/" + runtime.GOARCH)
		if err == nil {
			t.Errorf("%s: accepted uninstall.remove %v with models_dir %q", name, uninstall.remove, uninstall.modelsDir)
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

// TestRemoveTreePreservingKeepsASymlinkedStore is the guard for a model library
// moved and linked back. With ~/.lmstudio/models a symlink to a sibling
// directory, a removal that matched the store by path kept the link and deleted
// the sibling, which is where the models were.
func TestRemoveTreePreservingKeepsASymlinkedStore(t *testing.T) {
	engineRoot := filepath.Join(t.TempDir(), ".lmstudio")
	weights := filepath.Join(engineRoot, "weights")
	model := filepath.Join(weights, "model.gguf")
	binary := filepath.Join(engineRoot, "bin", "lms")
	for _, file := range []string{model, binary} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store := filepath.Join(engineRoot, "models")
	if err := os.Symlink(weights, store); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	if err := removeTreePreserving(engineRoot, store); err != nil {
		t.Fatalf("removeTreePreserving: %v", err)
	}

	if _, err := os.Stat(model); err != nil {
		t.Errorf("deleted the directory the store links to: %v", err)
	}
	if _, err := os.Lstat(store); err != nil {
		t.Errorf("removed the store's link: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(binary)); !os.IsNotExist(err) {
		t.Errorf("the engine's own files survived (err=%v)", err)
	}
}

// TestRemoveTreePreservingKeepsADeeperStore covers a store more than one level
// below the removal, where the removal recurses: the siblings at every level on
// the way go, and the store stays.
func TestRemoveTreePreservingKeepsADeeperStore(t *testing.T) {
	engineRoot := filepath.Join(t.TempDir(), ".engine")
	model := filepath.Join(engineRoot, "data", "models", "model.gguf")
	binary := filepath.Join(engineRoot, "bin", "engine")
	cache := filepath.Join(engineRoot, "data", "cache", "blob")
	for _, file := range []string{model, binary, cache} {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := removeTreePreserving(engineRoot, filepath.Join(engineRoot, "data", "models")); err != nil {
		t.Fatalf("removeTreePreserving: %v", err)
	}

	if _, err := os.Stat(model); err != nil {
		t.Errorf("model weights were removed: %v", err)
	}
	for _, gone := range []string{filepath.Join(engineRoot, "bin"), filepath.Join(engineRoot, "data", "cache")} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%q survived the uninstall (err=%v)", gone, err)
		}
	}
}

// TestRemoveTreePreservingKeepsAStoreSpelledInAnotherCase checks a store named
// in a different case is kept wherever the filesystem treats both spellings as
// one directory. That is a property of the filesystem, not the operating
// system: a macOS volume can be case-sensitive.
func TestRemoveTreePreservingKeepsAStoreSpelledInAnotherCase(t *testing.T) {
	engineRoot := filepath.Join(t.TempDir(), ".engine")
	model := filepath.Join(engineRoot, "Models", "model.gguf")
	if err := os.MkdirAll(filepath.Dir(model), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(model, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(engineRoot, "models")); err != nil {
		t.Skip("this filesystem is case-sensitive, so models and Models are different directories")
	}

	if err := removeTreePreserving(engineRoot, filepath.Join(engineRoot, "models")); err != nil {
		t.Fatalf("removeTreePreserving: %v", err)
	}

	if _, err := os.Stat(model); err != nil {
		t.Errorf("model weights were removed: %v", err)
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

// TestRemoveTreePreservingRefusesInsideTheStore covers a manifest asking to
// remove part of the model library. Refusing matters more than it looks:
// falling through to a plain removal here would delete the models.
func TestRemoveTreePreservingRefusesInsideTheStore(t *testing.T) {
	store := filepath.Join(t.TempDir(), "models")
	inside := filepath.Join(store, "publisher")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := removeTreePreserving(inside, store); err == nil {
		t.Error("expected an error when the target is inside the model store")
	}
	if _, err := os.Stat(inside); err != nil {
		t.Errorf("a refused removal still deleted %q: %v", inside, err)
	}
}

// TestRemoveTreePreservingUnlinksSymlinkedTarget checks a symlinked target is
// unlinked rather than read. Reading it would follow the link and delete the
// real directory's contents. Go reports a Windows junction differently from a
// symlink, so modelsdir_windows_test.go covers that case with a real junction.
func TestRemoveTreePreservingUnlinksSymlinkedTarget(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim")
	if err := os.MkdirAll(filepath.Join(victim, "keep-me"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "engine-home")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	// models is nested under the link, so this is the descend branch.
	if err := removeTreePreserving(link, filepath.Join(link, "models")); err != nil {
		t.Fatalf("removeTreePreserving: %v", err)
	}

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("the link survived (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(victim, "keep-me")); err != nil {
		t.Errorf("followed the link and deleted the real directory's contents: %v", err)
	}
}

// TestRemoveTreePreservingIsBestEffort pins that one undeletable entry does not
// stop the rest. A vendor daemon holding a file open used to leave the engine
// binary in place, so detection kept reporting the engine and the uninstall
// failed having half-removed it.
func TestRemoveTreePreservingIsBestEffort(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	engineRoot := t.TempDir()
	locked := filepath.Join(engineRoot, "locked")
	if err := os.MkdirAll(filepath.Join(locked, "child"), 0o755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(engineRoot, "bin", "engine")
	if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Read-only parent: the child cannot be unlinked, so "locked" fails while
	// "bin" — which sorts after it — must still go.
	if err := os.Chmod(locked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	err := removeTreePreserving(engineRoot, filepath.Join(engineRoot, "models"))
	if err == nil {
		t.Error("expected the undeletable entry to be reported")
	}
	if _, statErr := os.Stat(binary); !os.IsNotExist(statErr) {
		t.Errorf("stopped early: the engine binary survived (err=%v, remove err=%v)", statErr, err)
	}
}

func TestRemoveTreePreservingMissingTargetIsNoOp(t *testing.T) {
	if err := removeTreePreserving(filepath.Join(t.TempDir(), "absent"), ""); err != nil {
		t.Errorf("removing an absent path should be a no-op, got %v", err)
	}
}
