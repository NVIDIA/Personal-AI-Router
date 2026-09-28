// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const maxLlamaArchiveEntries = 10000

// Merge the pinned official app and dependency bundles into a fresh owned stage.
// The actual Windows bundles are flat; relative directories are preserved, never flattened.
func (e *Executor) stageLlamaArchives(ctx context.Context, st *engineState, candidate string, archives []Fetch) error {
	if err := validateLlamaOwnedPaths(st.installDir); err != nil {
		return err
	}
	if !isManagedInstallPath(candidate, st.installDir) {
		return fmt.Errorf("llama archive stage is outside the managed install")
	}
	if err := validateLlamaPath(candidate); err != nil {
		return err
	}
	if err := os.MkdirAll(candidate, 0700); err != nil {
		return err
	}
	remaining, entries := maxDownloadBytes, 0
	for _, fetch := range archives {
		if err := ctx.Err(); err != nil {
			return err
		}
		if strings.TrimSpace(fetch.SHA256) == "" {
			return fmt.Errorf("llama archive requires a pinned checksum")
		}
		archive, err := e.download(ctx, "llamacpp", &fetch)
		if err != nil {
			return err
		}
		if st.plat.Install.ArchiveRoot != "" {
			err = extractLlamaTar(ctx, archive, candidate, st.plat.Install.ArchiveRoot, &remaining, &entries)
		} else {
			err = extractLlamaArchive(ctx, archive, candidate, &remaining, &entries)
		}
		_ = os.Remove(archive) // Failed extracted stages remain available for diagnosis.
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

func extractLlamaArchive(ctx context.Context, archive, candidate string, remaining *int64, entries *int) error {
	z, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer z.Close()
	if len(z.File) > maxLlamaArchiveEntries-*entries {
		return fmt.Errorf("llama archives exceed the entry limit")
	}
	*entries += len(z.File)
	for _, entry := range z.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := strings.TrimSuffix(entry.Name, "/")
		if name == "" || path.IsAbs(name) || path.Clean(name) != name || strings.ContainsAny(name, "\\:\x00") {
			return fmt.Errorf("llama archive has an unsafe entry name")
		}
		for _, part := range strings.Split(name, "/") {
			if part == "." || part == ".." || strings.TrimRight(part, ". ") != part || !filepath.IsLocal(part) {
				return fmt.Errorf("llama archive has an unsafe path component")
			}
		}
		mode := entry.Mode()
		if !mode.IsRegular() && !mode.IsDir() {
			return fmt.Errorf("llama archive entry is not a regular file or directory")
		}
		dest := filepath.Join(candidate, filepath.FromSlash(name))
		rel, err := filepath.Rel(candidate, dest)
		if err != nil || !filepath.IsLocal(rel) {
			return fmt.Errorf("llama archive entry escapes its stage")
		}
		if err := validateLlamaPath(dest); err != nil {
			return err
		}
		if mode.IsDir() {
			if err := os.MkdirAll(dest, 0700); err != nil {
				return err
			}
			continue
		}
		if *remaining < 0 || entry.UncompressedSize64 > uint64(*remaining) {
			return fmt.Errorf("llama archives exceed the uncompressed byte limit")
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		src, err := entry.Open()
		if err != nil {
			return err
		}
		dst, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
		if err != nil {
			src.Close()
			return err
		}
		n, copyErr := io.Copy(llamaArchiveWriter{ctx, dst}, io.LimitReader(src, *remaining+1))
		closeErr := dst.Close()
		src.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n > *remaining {
			return fmt.Errorf("llama archives exceed the uncompressed byte limit")
		}
		*remaining -= n
	}
	return ctx.Err()
}

type llamaArchiveWriter struct {
	ctx context.Context
	io.Writer
}

func (w llamaArchiveWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.Writer.Write(p)
}
