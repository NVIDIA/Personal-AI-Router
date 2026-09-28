// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

func llamaModelDir(st *engineState) string {
	if st.modelDir != "" {
		return st.modelDir
	}
	return filepath.Join(st.installDir, "models")
}

// Move the owned cache once, atomically on the same platform data volume.
// Never merge or replace user content, and never move a live engine's cache.
func migrateLlamaCache(st *engineState) error {
	legacy, target := filepath.Join(st.installDir, "models"), llamaModelDir(st)
	if legacy == target {
		return nil
	}
	info, err := os.Lstat(legacy)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("legacy model cache is not an owned directory")
	}
	for _, path := range []string{legacy, target} {
		if err := validateLlamaPath(path); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(target); err == nil {
		return fmt.Errorf("both legacy and persistent model caches exist; neither was changed")
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := filepath.WalkDir(legacy, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink == 0 {
			return nil
		}
		link, err := os.Readlink(path)
		if err != nil {
			return err
		}
		if filepath.IsAbs(link) {
			return fmt.Errorf("legacy cache has an absolute link; migration left it unchanged")
		}
		_, err = llamaOwnedFile(legacy, path)
		return err
	}); err != nil {
		return err
	}
	if st.port > 0 && st.plat.Runtime.Ready != nil {
		ctx, cancel := context.WithTimeout(context.Background(), presenceRefusalWindow)
		defer cancel()
		if probeListener(ctx, st.plat.Runtime.Ready, st.port) != listenerProbeRefused {
			return fmt.Errorf("stop the listener on llama's configured port before migrating its model cache")
		}
	}
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return err
	}
	return os.Rename(legacy, target)
}
