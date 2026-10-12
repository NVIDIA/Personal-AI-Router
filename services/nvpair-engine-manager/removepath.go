// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
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
// The relationship between target and the store decides what happens:
//
//   - preserve empty: nothing to keep, so a plain removal.
//   - disjoint: a plain removal, as for Ollama, whose models sit in ~/.ollama
//     while the uninstall removes the install directory.
//   - the store below target: descend, keeping the store.
//   - target is the store, or inside it: refused. Both ask to delete the store
//     or part of it, which is a manifest bug, and widening to a full removal
//     would destroy exactly what this function exists to protect.
//
// The store is recognised by file identity, not by how its path is spelled. A
// models_dir that is a symlink keeps both the link and the directory it leads
// to, even when that directory is another child of target; and a spelling that
// differs only in case is the store exactly when the filesystem says so, which
// no rule based on the operating system can tell.
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
	store := locateStore(absPreserve)
	if store.is(info) {
		return fmt.Errorf("remove: %q is the preserved model store", absTarget)
	}
	if store.contains(absTarget) {
		return fmt.Errorf("remove: %q is inside the preserved model store %q", absTarget, absPreserve)
	}
	if !store.under(info) {
		return os.RemoveAll(absTarget)
	}
	entries, err := os.ReadDir(absTarget)
	if err != nil {
		return fmt.Errorf("remove: read %q: %w", absTarget, err)
	}
	var failures []error
	for _, entry := range entries {
		child := filepath.Join(absTarget, entry.Name())
		childInfo, err := os.Lstat(child)
		if err != nil {
			failures = append(failures, fmt.Errorf("remove: stat %q: %w", child, err))
			continue
		}
		switch {
		case store.is(childInfo):
			// The store, by the name it was given or the directory it leads to.
		case store.under(childInfo):
			// On the way to the store. Recurse so siblings deeper down still go.
			if err := removeTreePreserving(child, absPreserve); err != nil {
				failures = append(failures, err)
			}
		default:
			if err := os.RemoveAll(child); err != nil {
				failures = append(failures, fmt.Errorf("remove: %q: %w", child, err))
			}
		}
	}
	return errors.Join(failures...)
}

// modelStore is a model store as the filesystem identifies it.
type modelStore struct {
	// self is the store as named and as resolved: the link itself when its
	// path is a symlink, and the directory that link leads to.
	self []os.FileInfo
	// above is every directory over the store, along the path as written and
	// along the path its links resolve to.
	above []os.FileInfo
}

func locateStore(path string) modelStore {
	var s modelStore
	if fi, err := os.Lstat(path); err == nil {
		s.self = append(s.self, fi)
	}
	if fi, err := os.Stat(path); err == nil {
		s.self = append(s.self, fi)
	}
	s.above = directoriesAbove(path)
	return s
}

// is reports whether fi is the store.
func (s modelStore) is(fi os.FileInfo) bool {
	for _, own := range s.self {
		if os.SameFile(fi, own) {
			return true
		}
	}
	return false
}

// under reports whether fi is a directory the store sits below. Anything else,
// a link to one included, is not: removal descends only into real directories.
func (s modelStore) under(fi os.FileInfo) bool {
	if !fi.IsDir() {
		return false
	}
	for _, dir := range s.above {
		if os.SameFile(fi, dir) {
			return true
		}
	}
	return false
}

// contains reports whether path sits below the store.
func (s modelStore) contains(path string) bool {
	for _, dir := range directoriesAbove(path) {
		if s.is(dir) {
			return true
		}
	}
	return false
}

// directoriesAbove stats each existing directory over path, along the path as
// written and, when a link on it leads elsewhere, along the resolved path.
func directoriesAbove(path string) []os.FileInfo {
	var out []os.FileInfo
	walk := func(p string) {
		for dir := filepath.Dir(p); ; dir = filepath.Dir(dir) {
			if fi, err := os.Stat(dir); err == nil {
				out = append(out, fi)
			}
			if filepath.Dir(dir) == dir {
				return
			}
		}
	}
	walk(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
		walk(resolved)
	}
	return out
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
