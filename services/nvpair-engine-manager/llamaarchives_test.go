// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func llamaArchiveFixture(t *testing.T, name, body string, mode os.FileMode) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	h.SetMode(mode)
	w, err := z.CreateHeader(h)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, body); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestLlamaArchivesMergeAndRefuseCollision(t *testing.T) {
	for _, collision := range []bool{false, true} {
		t.Run(map[bool]string{false: "merge", true: "collision"}[collision], func(t *testing.T) {
			root := t.TempDir()
			candidate := filepath.Join(root, "stage")
			second := "deps/runtime.dll"
			if collision {
				second = "llama.exe"
			}
			bundles := [][]byte{llamaArchiveFixture(t, "llama.exe", "app", 0600), llamaArchiveFixture(t, second, "dependency", 0600)}
			st := &engineState{installDir: root, plat: &Platform{Install: &Install{}}}
			for _, bundle := range bundles {
				digest := sha256.Sum256(bundle)
				st.plat.Install.Archives = append(st.plat.Install.Archives, Fetch{URL: "https://example.invalid/pinned.zip", SHA256: hex.EncodeToString(digest[:])})
			}
			e := NewExecutor(buildRegistry(""), NewReporter(nil), nil, root)
			index := 0
			e.client = &http.Client{Transport: llamaFixtureTransport(func(*http.Request) (*http.Response, error) {
				data := bundles[index]
				index++
				return &http.Response{StatusCode: 200, ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data))}, nil
			})}
			err := e.stageLlamaArchives(context.Background(), st, candidate)
			if (err != nil) != collision {
				t.Fatalf("collision=%v: %v", collision, err)
			}
			body, err := os.ReadFile(filepath.Join(candidate, "llama.exe"))
			if err != nil || string(body) != "app" {
				t.Fatalf("first bundle overwritten: %q %v", body, err)
			}
			if !collision {
				body, err = os.ReadFile(filepath.Join(candidate, "deps", "runtime.dll"))
				if err != nil || string(body) != "dependency" {
					t.Fatalf("relative directory lost: %q %v", body, err)
				}
			}
		})
	}
}

func TestLlamaArchivesRejectUnsafeEntriesAndBounds(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    os.FileMode
		budget  int64
		entries int
	}{
		{"../outside", 0600, 100, 0}, {"/absolute", 0600, 100, 0}, {`dir\file`, 0600, 100, 0},
		{"file:stream", 0600, 100, 0}, {"dir/../file", 0600, 100, 0}, {"file.", 0600, 100, 0},
		{"link", os.ModeSymlink | 0600, 100, 0}, {"pipe", os.ModeNamedPipe | 0600, 100, 0},
		{"large", 0600, 2, 0}, {"entry", 0600, 100, maxLlamaArchiveEntries},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			archive := filepath.Join(root, "fixture.zip")
			if err := os.WriteFile(archive, llamaArchiveFixture(t, tc.name, "data", tc.mode), 0600); err != nil {
				t.Fatal(err)
			}
			if err := extractLlamaArchive(context.Background(), archive, filepath.Join(root, "stage"), &tc.budget, &tc.entries); err == nil {
				t.Fatal("entry/bound accepted")
			}
		})
	}
}

func TestLlamaArchiveCopyHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var output bytes.Buffer
	w := llamaArchiveWriter{ctx, &output}
	if _, err := w.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := w.Write([]byte("later")); !errors.Is(err, context.Canceled) {
		t.Fatalf("copy after cancellation: %v", err)
	}
	if output.String() != "first" {
		t.Fatal("wrote bytes after cancellation")
	}
}
