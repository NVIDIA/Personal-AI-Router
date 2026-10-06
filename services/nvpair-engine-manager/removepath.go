// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// removeTreePreserving deletes target, except for preserve and whatever leads
// to it. This is what keeps an engine uninstall from taking the user's model
// library: LM Studio installs into ~/.lmstudio and keeps its downloads in
// ~/.lmstudio/models, so removing the engine means removing every other child
// of that directory and stopping there.
//
// preserve may be empty (nothing to keep), outside target (a plain removal, as
// for Ollama, whose models live in ~/.ollama), or below it (the descend case).
// Equal paths are refused: the caller is asking to delete the model store under
// the guise of preserving it, which is a manifest bug rather than a no-op.
func removeTreePreserving(target, preserve string) error {
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("remove: target is required")
	}
	absTarget, err := filepath.Abs(filepath.Clean(expandPath(target)))
	if err != nil {
		return fmt.Errorf("remove: target: %w", err)
	}
	if _, err := os.Lstat(absTarget); err != nil {
		if os.IsNotExist(err) {
			return nil // already gone; uninstall is idempotent
		}
		return fmt.Errorf("remove: stat %q: %w", absTarget, err)
	}
	if strings.TrimSpace(preserve) == "" {
		return os.RemoveAll(absTarget)
	}
	absPreserve, err := filepath.Abs(filepath.Clean(expandPath(preserve)))
	if err != nil {
		return fmt.Errorf("remove: preserve: %w", err)
	}
	if absPreserve == absTarget {
		return fmt.Errorf("remove: %q is the preserved model store", absTarget)
	}
	if !pathWithinRoot(absTarget, absPreserve) {
		return os.RemoveAll(absTarget)
	}
	entries, err := os.ReadDir(absTarget)
	if err != nil {
		return fmt.Errorf("remove: read %q: %w", absTarget, err)
	}
	for _, entry := range entries {
		child := filepath.Join(absTarget, entry.Name())
		if pathWithinRoot(child, absPreserve) {
			// On the path to the model store. Recurse so siblings deeper down
			// still go, and stop once the store itself is the next step.
			if child == absPreserve {
				continue
			}
			if err := removeTreePreserving(child, absPreserve); err != nil {
				return err
			}
			continue
		}
		if err := os.RemoveAll(child); err != nil {
			return fmt.Errorf("remove: %q: %w", child, err)
		}
	}
	return nil
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
	if target == root {
		return true
	}
	prefix := root + string(os.PathSeparator)
	return strings.HasPrefix(target, prefix)
}
