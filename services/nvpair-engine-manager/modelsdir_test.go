// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
			if !assert.NotEmpty(t, platform.ModelsDir, "%s/%s: declare the engine's model store so uninstall knows what to keep", name, key) {
				continue
			}
			resolved := strings.ToLower(filepath.ToSlash(expandPath(platform.ModelsDir)))
			for _, segment := range forbidden {
				if !assert.NotContains(t, resolved, segment, "%s/%s: models_dir is inside the app data root; the app uninstall would delete the user's models", name, key) {
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
		assert.False(t, pathWithinRoot(root, expandPath(platform.ModelsDir)), "%s: models_dir %q resolves under the app data root %q", name, platform.ModelsDir, root)
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
				assert.NotEqual(t, filepath.Clean(expandPath(platform.ModelsDir)), filepath.Clean(expandPath(target)), "%s/%s: uninstall.remove targets the model store", name, key)
			}
			for _, arg := range platform.Uninstall.Run {
				assert.Empty(t, destructiveToken(arg), "%s/%s: uninstall.run deletes files; use uninstall.remove so the model store is preserved", name, key)
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
	test := func(name, argument string, destructive bool) {
		t.Run(name, func(t *testing.T) {
			if destructive {
				assert.NotEmpty(t, destructiveToken(argument), "the original LM Studio uninstall was not recognised as deleting files")
			} else {
				assert.Empty(t, destructiveToken(argument), "harmless argument must not be treated as deleting files")
			}
		})
	}
	test("posix uninstall", `pkill -x lms 2>/dev/null; pkill -x llmster 2>/dev/null; sleep 2; rm -rf "$HOME/.lmstudio"; sleep 1; [ -d "$HOME/.lmstudio" ] && exit 1; exit 0`, true)
	test("windows uninstall", `$root = Join-Path $env:USERPROFILE '.lmstudio'; Start-Sleep -Seconds 3; Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue`, true)
	test("stop processes", "pkill -x lms 2>/dev/null; pkill -x llmster 2>/dev/null; sleep 2; exit 0", false)
	test("server command", "server", false)
	test("extraction flag", "--strip-components=1", false)
}

// TestValidateRejectsRemovalsThatReachTheModelStore pins the load-time
// rejections the manifest reference promises. Each of these was accepted before,
// and each deletes part or all of a user's model library at uninstall time.
func TestValidateRejectsRemovalsThatReachTheModelStore(t *testing.T) {
	test := func(name, modelsDir string, remove []string) {
		t.Run(name, func(t *testing.T) {
			platform := Platform{
				Detect:    []string{"{install_dir}/engine"},
				ModelsDir: modelsDir,
				Uninstall: &Uninstall{Remove: remove},
				Runtime:   Runtime{Bin: "{install_dir}/engine"},
			}
			assert.Error(t, platform.validate(runtime.GOOS+"/"+runtime.GOARCH), "uninstall.remove must preserve the model store")
		})
	}
	test("no store to preserve", "", []string{"~/.engine"})
	test("the store itself", "~/.engine/models", []string{"~/.engine/models"})
	test("inside the store", "~/.engine/models", []string{"~/.engine/models/publisher"})
	test("the store, templated", "~/.engine/models", []string{"{models_dir}"})
	test("inside it, templated", "~/.engine/models", []string{"{models_dir}/publisher"})
	test("the store, mixed case", "~/.engine/models", []string{"~/.engine/Models"})
}

func TestRemoveTreePreservingKeepsNestedStore(t *testing.T) {
	root := t.TempDir()
	engineRoot := filepath.Join(root, ".lmstudio")
	models := filepath.Join(engineRoot, "models", "publisher", "repo")
	binary := filepath.Join(engineRoot, "bin", "lms")
	internal := filepath.Join(engineRoot, ".internal", "state.json")
	for _, dir := range []string{models, filepath.Dir(binary), filepath.Dir(internal)} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	weights := filepath.Join(models, "model.gguf")
	for _, file := range []string{weights, binary, internal} {
		require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	}

	require.NoError(t, removeTreePreserving(engineRoot, filepath.Join(engineRoot, "models")), "removeTreePreserving")

	assert.FileExists(t, weights, "model weights were removed")
	for _, gone := range []string{binary, internal, filepath.Join(engineRoot, "bin")} {
		_, err := os.Stat(gone)
		assert.ErrorIs(t, err, os.ErrNotExist, "%q survived the uninstall", gone)
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
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
		require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	}
	store := filepath.Join(engineRoot, "models")
	if err := os.Symlink(weights, store); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	require.NoError(t, removeTreePreserving(engineRoot, store), "removeTreePreserving")

	assert.FileExists(t, model, "deleted the directory the store links to")
	_, err := os.Lstat(store)
	assert.NoError(t, err, "removed the store's link")
	_, err = os.Stat(filepath.Dir(binary))
	assert.ErrorIs(t, err, os.ErrNotExist, "the engine's own files survived")
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
		require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
		require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	}

	require.NoError(t, removeTreePreserving(engineRoot, filepath.Join(engineRoot, "data", "models")), "removeTreePreserving")

	assert.FileExists(t, model, "model weights were removed")
	for _, gone := range []string{filepath.Join(engineRoot, "bin"), filepath.Join(engineRoot, "data", "cache")} {
		_, err := os.Stat(gone)
		assert.ErrorIs(t, err, os.ErrNotExist, "%q survived the uninstall", gone)
	}
}

// TestRemoveTreePreservingKeepsAStoreSpelledInAnotherCase checks a store named
// in a different case is kept wherever the filesystem treats both spellings as
// one directory. That is a property of the filesystem, not the operating
// system: a macOS volume can be case-sensitive.
func TestRemoveTreePreservingKeepsAStoreSpelledInAnotherCase(t *testing.T) {
	engineRoot := filepath.Join(t.TempDir(), ".engine")
	model := filepath.Join(engineRoot, "Models", "model.gguf")
	require.NoError(t, os.MkdirAll(filepath.Dir(model), 0o755))
	require.NoError(t, os.WriteFile(model, []byte("x"), 0o644))
	if _, err := os.Stat(filepath.Join(engineRoot, "models")); err != nil {
		t.Skip("this filesystem is case-sensitive, so models and Models are different directories")
	}

	require.NoError(t, removeTreePreserving(engineRoot, filepath.Join(engineRoot, "models")), "removeTreePreserving")

	assert.FileExists(t, model, "model weights were removed")
}

func TestRemoveTreePreservingRemovesUnrelatedTree(t *testing.T) {
	root := t.TempDir()
	installDir := filepath.Join(root, "engine-bin", "ollama")
	require.NoError(t, os.MkdirAll(installDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(installDir, "ollama"), []byte("x"), 0o644))

	// Ollama's models live outside the install dir, so this is a plain removal.
	require.NoError(t, removeTreePreserving(installDir, filepath.Join(root, ".ollama")), "removeTreePreserving")
	_, err := os.Stat(installDir)
	assert.ErrorIs(t, err, os.ErrNotExist, "install dir survived")
}

func TestRemoveTreePreservingRefusesTheStoreItself(t *testing.T) {
	root := t.TempDir()
	assert.Error(t, removeTreePreserving(root, root), "expected an error when the target is the preserved model store")
}

// TestRemoveTreePreservingRefusesInsideTheStore covers a manifest asking to
// remove part of the model library. Refusing matters more than it looks:
// falling through to a plain removal here would delete the models.
func TestRemoveTreePreservingRefusesInsideTheStore(t *testing.T) {
	store := filepath.Join(t.TempDir(), "models")
	inside := filepath.Join(store, "publisher")
	require.NoError(t, os.MkdirAll(inside, 0o755))
	assert.Error(t, removeTreePreserving(inside, store), "expected an error when the target is inside the model store")
	_, err := os.Stat(inside)
	assert.NoError(t, err, "a refused removal still deleted %q", inside)
}

// TestRemoveTreePreservingUnlinksSymlinkedTarget checks a symlinked target is
// unlinked rather than read. Reading it would follow the link and delete the
// real directory's contents. Go reports a Windows junction differently from a
// symlink, so modelsdir_windows_test.go covers that case with a real junction.
func TestRemoveTreePreservingUnlinksSymlinkedTarget(t *testing.T) {
	root := t.TempDir()
	victim := filepath.Join(root, "victim")
	require.NoError(t, os.MkdirAll(filepath.Join(victim, "keep-me"), 0o755))
	link := filepath.Join(root, "engine-home")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	// models is nested under the link, so this is the descend branch.
	require.NoError(t, removeTreePreserving(link, filepath.Join(link, "models")), "removeTreePreserving")

	_, err := os.Lstat(link)
	assert.ErrorIs(t, err, os.ErrNotExist, "the link survived")
	_, err = os.Stat(filepath.Join(victim, "keep-me"))
	assert.NoError(t, err, "followed the link and deleted the real directory's contents")
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
	require.NoError(t, os.MkdirAll(filepath.Join(locked, "child"), 0o755))
	binary := filepath.Join(engineRoot, "bin", "engine")
	require.NoError(t, os.MkdirAll(filepath.Dir(binary), 0o755))
	require.NoError(t, os.WriteFile(binary, []byte("x"), 0o644))
	// Read-only parent: the child cannot be unlinked, so "locked" fails while
	// "bin" — which sorts after it — must still go.
	require.NoError(t, os.Chmod(locked, 0o500))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(locked, 0o700), "restore directory permissions") })

	err := removeTreePreserving(engineRoot, filepath.Join(engineRoot, "models"))
	assert.Error(t, err, "expected the undeletable entry to be reported")
	_, statErr := os.Stat(binary)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "stopped early: the engine binary survived (remove err=%v)", err)
}

func TestRemoveTreePreservingMissingTargetIsNoOp(t *testing.T) {
	assert.NoError(t, removeTreePreserving(filepath.Join(t.TempDir(), "absent"), ""), "removing an absent path should be a no-op")
}
