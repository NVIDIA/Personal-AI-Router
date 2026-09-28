// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type llamaTarEntry struct {
	name, body, link string
	kind             byte
}

func llamaTarFixture(t *testing.T, entries []llamaTarEntry) string {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.kind, Mode: 0700, Linkname: e.link}
		if e.kind == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.body != "" {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "official.tar.gz")
	if err := os.WriteFile(file, data.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return file
}
func TestLlamaTarOfficialLayoutAndLinkChain(t *testing.T) {
	archive := llamaTarFixture(t, []llamaTarEntry{{name: "llama-b10826", kind: tar.TypeDir}, {name: "llama-b10826/llama", body: "app", kind: tar.TypeReg}, {name: "llama-b10826/libggml.dylib", link: "libggml.0.dylib", kind: tar.TypeSymlink}, {name: "llama-b10826/libggml.0.dylib", link: "libggml.0.23.0.dylib", kind: tar.TypeSymlink}, {name: "llama-b10826/libggml.0.23.0.dylib", body: "library", kind: tar.TypeReg}})
	stage := t.TempDir()
	remaining := int64(100000)
	count := 0
	if err := extractLlamaTar(context.Background(), archive, stage, "llama-b10826", &remaining, &count); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"libggml.dylib", "libggml.0.dylib", "libggml.0.23.0.dylib"} {
		p := filepath.Join(stage, name)
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("not a materialized regular library: %s", name)
		}
		data, _ := os.ReadFile(p)
		if string(data) != "library" {
			t.Fatal("library changed")
		}
	}
	if count != 5 {
		t.Fatalf("entries=%d", count)
	}
}
func TestLlamaTarRejectsUnsafeOrIncompleteEntries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []llamaTarEntry
	}{
		{"outside", []llamaTarEntry{{name: "other/llama", body: "x", kind: tar.TypeReg}}},
		{"traversal", []llamaTarEntry{{name: "llama-b10826/../escape", body: "x", kind: tar.TypeReg}}},
		{"link-escape", []llamaTarEntry{{name: "llama-b10826/lib", link: "../escape", kind: tar.TypeSymlink}}},
		{"link-absolute", []llamaTarEntry{{name: "llama-b10826/lib", link: "/tmp/escape", kind: tar.TypeSymlink}}},
		{"missing", []llamaTarEntry{{name: "llama-b10826/lib", link: "absent", kind: tar.TypeSymlink}}},
		{"cycle", []llamaTarEntry{{name: "llama-b10826/a", link: "b", kind: tar.TypeSymlink}, {name: "llama-b10826/b", link: "a", kind: tar.TypeSymlink}}},
		{"duplicate", []llamaTarEntry{{name: "llama-b10826/a", body: "x", kind: tar.TypeReg}, {name: "llama-b10826/a", body: "y", kind: tar.TypeReg}}},
		{"hardlink", []llamaTarEntry{{name: "llama-b10826/a", link: "b", kind: tar.TypeLink}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remaining := int64(100000)
			count := 0
			if err := extractLlamaTar(context.Background(), llamaTarFixture(t, tc.entries), t.TempDir(), "llama-b10826", &remaining, &count); err == nil {
				t.Fatal("accepted unsafe tar")
			}
		})
	}
}
func TestLlamaTarLimitsCancellationAndExistingFiles(t *testing.T) {
	archive := llamaTarFixture(t, []llamaTarEntry{{name: "llama-b10826/llama", body: "new", kind: tar.TypeReg}})
	for _, name := range []string{"bytes", "entries", "cancel", "collision", "gzip-trailer"} {
		t.Run(name, func(t *testing.T) {
			stage := t.TempDir()
			remaining := int64(100000)
			count := 0
			ctx := context.Background()
			input := archive
			switch name {
			case "bytes":
				remaining = 2
			case "entries":
				count = maxLlamaArchiveEntries
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "collision":
				if err := os.WriteFile(filepath.Join(stage, "llama"), []byte("old"), 0600); err != nil {
					t.Fatal(err)
				}
			case "gzip-trailer":
				data, _ := os.ReadFile(archive)
				input = filepath.Join(t.TempDir(), "bad.gz")
				if err := os.WriteFile(input, data[:len(data)-5], 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := extractLlamaTar(ctx, input, stage, "llama-b10826", &remaining, &count)
			if err == nil {
				t.Fatal("expected refusal")
			}
			if name == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if name == "collision" {
				data, _ := os.ReadFile(filepath.Join(stage, "llama"))
				if string(data) != "old" {
					t.Fatal("overwrote existing file")
				}
			}
		})
	}
}
