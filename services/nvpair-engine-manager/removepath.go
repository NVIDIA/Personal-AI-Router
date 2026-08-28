// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// lmstudioModelsDir is the default on-disk model cache for LM Studio.
func lmstudioModelsDir() string {
	return expandPath("~/.lmstudio/models")
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
