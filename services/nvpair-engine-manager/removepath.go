// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// removeTreePreserving deletes target, except for preserve and whatever leads
// to it. This is what keeps an engine uninstall from taking the user's model
// library: LM Studio installs into ~/.lmstudio and keeps its downloads in
// ~/.lmstudio/models, so removing the engine means removing every other child
// of that directory and stopping there.
//
// The relationship between the two paths decides what happens:
//
//   - preserve empty: nothing to keep, so a plain removal.
//   - disjoint: a plain removal, as for Ollama, whose models sit in ~/.ollama
//     while the uninstall removes the install directory.
//   - preserve strictly inside target: descend, keeping the store.
//   - equal, or target inside preserve: refused. Both ask to delete the store
//     or part of it, which is a manifest bug, and widening to a full removal
//     would destroy exactly what this function exists to protect.
//
// Removal is best-effort within the tree: an engine holding one file open must
// not stop the rest of it from going, or the binary survives, detection keeps
// reporting the engine, and the uninstall fails having half-removed it. Errors
// are collected and returned together, and the caller judges the outcome by
// whether the engine is still detected.
//
// Only a real directory is descended into. A symlink, or on Windows a junction,
// is removed as a link: deleting what it points at is not what the manifest
// asked for, and a junction followed here would let the elevated uninstaller be
// steered into an arbitrary tree. Go reports a junction as an irregular file
// rather than a symlink, so the test is "not a directory", not "a symlink".
func removeTreePreserving(target, preserve string) error {
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("remove: target is required")
	}
	absTarget, err := filepath.Abs(filepath.Clean(expandPath(target)))
	if err != nil {
		return fmt.Errorf("remove: target: %w", err)
	}
	info, err := os.Lstat(absTarget)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // already gone; uninstall is idempotent
		}
		return fmt.Errorf("remove: stat %q: %w", absTarget, err)
	}
	if strings.TrimSpace(preserve) == "" {
		return os.RemoveAll(absTarget)
	}
	// Unlink rather than descend. On the descend path below, ReadDir would
	// follow a link to the real directory and delete its contents.
	if !info.IsDir() {
		return os.Remove(absTarget)
	}
	absPreserve, err := filepath.Abs(filepath.Clean(expandPath(preserve)))
	if err != nil {
		return fmt.Errorf("remove: preserve: %w", err)
	}
	if samePath(absTarget, absPreserve) {
		return fmt.Errorf("remove: %q is the preserved model store", absTarget)
	}
	if pathWithinRoot(absPreserve, absTarget) {
		return fmt.Errorf("remove: %q is inside the preserved model store %q", absTarget, absPreserve)
	}
	if !pathWithinRoot(absTarget, absPreserve) {
		return os.RemoveAll(absTarget)
	}
	entries, err := os.ReadDir(absTarget)
	if err != nil {
		return fmt.Errorf("remove: read %q: %w", absTarget, err)
	}
	var failures []error
	for _, entry := range entries {
		child := filepath.Join(absTarget, entry.Name())
		if pathWithinRoot(child, absPreserve) {
			// On the path to the model store. Recurse so siblings deeper down
			// still go, and stop once the store itself is the next step.
			if samePath(child, absPreserve) {
				continue
			}
			if err := removeTreePreserving(child, absPreserve); err != nil {
				failures = append(failures, err)
			}
			continue
		}
		if err := os.RemoveAll(child); err != nil {
			failures = append(failures, fmt.Errorf("remove: %q: %w", child, err))
		}
	}
	return errors.Join(failures...)
}

// samePath reports whether two cleaned absolute paths name the same file.
//
// Case-folded on Windows and macOS, whose default filesystems are
// case-insensitive: there, a casing difference between a manifest's remove
// target and its models_dir would make the model store look like an unrelated
// sibling and widen the removal to include it.
func samePath(a, b string) bool {
	if caseInsensitiveFS() {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func caseInsensitiveFS() bool {
	return runtime.GOOS == "windows" || runtime.GOOS == "darwin"
}

// safeRemoveUnderRoot deletes target after verifying it resolves under root.
// Both paths are cleaned; symlinks on target are evaluated before the confinement
// check so a path cannot escape the allowed root via symlink tricks.
func safeRemoveUnderRoot(root, target string) error {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(target) == "" {
		return fmt.Errorf("remove_path: root and path are required")
	}
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return fmt.Errorf("remove_path: root: %w", err)
	}
	absTarget, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return fmt.Errorf("remove_path: target: %w", err)
	}
	evalRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("remove_path: root symlink: %w", err)
		}
		evalRoot = absRoot
	}
	evalTarget, err := filepath.EvalSymlinks(absTarget)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("remove_path: target symlink: %w", err)
		}
		evalTarget = absTarget
	}
	if !pathWithinRoot(evalRoot, evalTarget) {
		return fmt.Errorf("remove_path: %q escapes allowed root %q", evalTarget, evalRoot)
	}
	if _, err := os.Stat(evalTarget); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("remove_path: %q does not exist", evalTarget)
		}
		return fmt.Errorf("remove_path: stat %q: %w", evalTarget, err)
	}
	if err := os.RemoveAll(evalTarget); err != nil {
		return fmt.Errorf("remove_path: %w", err)
	}
	return nil
}

func pathWithinRoot(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if samePath(root, target) {
		return true
	}
	prefix := root + string(os.PathSeparator)
	if caseInsensitiveFS() {
		return strings.HasPrefix(strings.ToLower(target), strings.ToLower(prefix))
	}
	return strings.HasPrefix(target, prefix)
}
