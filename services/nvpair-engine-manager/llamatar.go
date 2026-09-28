// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

func safeLlamaTarName(name string) bool {
	if name == "" || path.IsAbs(name) || path.Clean(name) != name || strings.ContainsAny(name, "\\:\x00") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." || strings.TrimRight(part, ". ") != part || !filepath.IsLocal(part) {
			return false
		}
	}
	return true
}

// Official Intel Mac bundles contain a fixed root and versioned dylib links.
// Materialize links as regular files: no symlink survives into the managed tree.
func extractLlamaTar(ctx context.Context, archive, candidate, prefix string, remaining *int64, entries *int) error {
	if !safeLlamaTarName(prefix) || strings.Contains(prefix, "/") {
		return fmt.Errorf("unsafe llama tar prefix")
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	files := map[string]bool{}
	links := map[string]string{}
	write := func(name string, src io.Reader, size int64) error {
		if size < 0 || size > *remaining {
			return fmt.Errorf("llama archives exceed the uncompressed byte limit")
		}
		dest := filepath.Join(candidate, filepath.FromSlash(name))
		if err := validateLlamaPath(dest); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		dst, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(llamaArchiveWriter{ctx, dst}, io.LimitReader(src, size+1))
		closeErr := dst.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n != size {
			return fmt.Errorf("llama tar size mismatch")
		}
		*remaining -= n
		files[name] = true
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		*entries++
		if *entries > maxLlamaArchiveEntries {
			return fmt.Errorf("llama archives exceed the entry limit")
		}
		full := strings.TrimSuffix(h.Name, "/")
		if !safeLlamaTarName(full) || seen[full] {
			return fmt.Errorf("unsafe or duplicate llama tar entry")
		}
		seen[full] = true
		if full == prefix && h.Typeflag == tar.TypeDir {
			continue
		}
		if !strings.HasPrefix(full, prefix+"/") {
			return fmt.Errorf("llama tar entry outside declared prefix")
		}
		name := strings.TrimPrefix(full, prefix+"/")
		switch h.Typeflag {
		case tar.TypeReg, tar.TypeRegA:
			if err := write(name, tr, h.Size); err != nil {
				return err
			}
		case tar.TypeDir:
			dest := filepath.Join(candidate, filepath.FromSlash(name))
			if err := validateLlamaPath(dest); err != nil {
				return err
			}
			if err := os.MkdirAll(dest, 0700); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if !safeLlamaTarName(h.Linkname) {
				return fmt.Errorf("unsafe llama library link")
			}
			links[name] = path.Join(path.Dir(name), h.Linkname)
		default:
			return fmt.Errorf("unsupported llama tar entry type")
		}
	}
	// Consume bounded padding/trailer so gzip integrity errors are not hidden by
	// tar's earlier end marker. Extra compressed members share the same budget.
	n, err := io.Copy(llamaArchiveWriter{ctx, io.Discard}, io.LimitReader(gz, *remaining+1))
	if err != nil {
		return err
	}
	if n > *remaining {
		return fmt.Errorf("llama archives exceed the uncompressed byte limit")
	}
	*remaining -= n
	for len(links) > 0 {
		progress := false
		for name, target := range links {
			if !files[target] {
				continue
			}
			src, err := os.Open(filepath.Join(candidate, filepath.FromSlash(target)))
			if err != nil {
				return err
			}
			info, err := src.Stat()
			if err == nil && !info.Mode().IsRegular() {
				err = fmt.Errorf("llama link target is not a regular file")
			}
			if err == nil {
				err = write(name, src, info.Size())
			}
			src.Close()
			if err != nil {
				return err
			}
			delete(links, name)
			progress = true
		}
		if !progress {
			return fmt.Errorf("missing or cyclic llama library link")
		}
	}
	return ctx.Err()
}
