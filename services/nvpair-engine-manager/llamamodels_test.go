// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

const llamaTestCommit = "0123456789abcdef0123456789abcdef01234567"

func TestLlamaDownloadValidatesReturnedModelData(t *testing.T) {
	root := t.TempDir()
	header := make([]byte, 24)
	copy(header, "GGUF")
	binary.LittleEndian.PutUint32(header[4:8], 3)
	binary.LittleEndian.PutUint64(header[8:16], 1)
	for _, tc := range []struct {
		name  string
		body  []byte
		valid bool
	}{
		{"model-Q4_K_M.gguf", header, true},
		{"preset.ini", []byte("[model]\nname=fixture\n"), false},
		{"configuration.gguf", []byte("[model]\nname=fixture\n"), false},
		{"empty.gguf", nil, false},
		{"truncated.gguf", []byte("GGUF"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.name)
			if err := os.WriteFile(path, tc.body, 0600); err != nil {
				t.Fatal(err)
			}
			if err := validateLlamaDownload(root, path+"\n"); (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
		})
	}
	outside := filepath.Join(t.TempDir(), "other.gguf")
	if err := os.WriteFile(outside, header, 0600); err != nil {
		t.Fatal(err)
	}
	if err := validateLlamaDownload(root, outside); err == nil {
		t.Fatal("accepted external model")
	}
}

func TestLlamaCacheImportListDelete(t *testing.T) {
	root := filepath.Join(t.TempDir(), "models")
	source := filepath.Join(t.TempDir(), "tiny-Q4_K_M.gguf")
	if err := os.WriteFile(source, []byte("GGUFfixture"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := llamaImport(context.Background(), root, source)
	if err != nil {
		t.Fatal(err)
	}
	if id != "local/tiny-Q4_K_M:Q4_K_M" {
		t.Fatal(id)
	}
	models, err := llamaCacheModels(root)
	if err != nil || len(models) != 1 || models[0].ID != id {
		t.Fatalf("models=%v err=%v", models, err)
	}
	if _, err := llamaImport(context.Background(), root, source); err == nil {
		t.Fatal("duplicate overwrote model")
	}
	e := &Executor{}
	st := &engineState{installDir: filepath.Dir(root)}
	params, _ := json.Marshal(map[string]string{"model": id})
	if _, err := e.llamaModelAction(context.Background(), st, "delete_model", params); err != nil {
		t.Fatal(err)
	}
	models, err = llamaCacheModels(root)
	if err != nil || len(models) != 0 {
		t.Fatalf("models=%v err=%v", models, err)
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("source file modified", err)
	}
	if _, err := llamaImport(context.Background(), root, source); err != nil {
		t.Fatal("reimport after delete", err)
	}
}

func TestLlamaCacheIncompleteAndCancellation(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "models--owner--repo")
	if err := os.MkdirAll(filepath.Join(base, "refs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "refs", "main"), []byte(llamaTestCommit), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(base, "snapshots", llamaTestCommit)
	if err := os.MkdirAll(snapshot, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(snapshot, name), []byte("GGUFfixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("tiny-Q4_K_M-00001-of-00002.gguf")
	write("mmproj-F16.gguf")
	write("tiny-Q8_0.gguf.downloadInProgress")
	models, err := llamaCacheModels(root)
	if err != nil || len(models) != 0 {
		t.Fatalf("incomplete listed: %v %v", models, err)
	}
	write("tiny-Q4_K_M-00002-of-00002.gguf")
	models, err = llamaCacheModels(root)
	if err != nil || len(models) != 1 || len(models[0].Files) != 2 {
		t.Fatalf("complete missing: %v %v", models, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	source := filepath.Join(t.TempDir(), "cancel-Q8_0.gguf")
	if err := os.WriteFile(source, []byte("GGUFfixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := llamaImport(ctx, root, source); err != context.Canceled {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "models--local--cancel-Q8_0")); !os.IsNotExist(err) {
		t.Fatal("canceled import published")
	}
	outside := filepath.Join(t.TempDir(), "other.gguf")
	if err := os.WriteFile(outside, []byte("GGUF"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := llamaOwnedFile(root, outside); err == nil {
		t.Fatal("outside path admitted")
	}
}

func TestLlamaCacheSharedBlobDeletion(t *testing.T) {
	install := t.TempDir()
	root := filepath.Join(install, "models")
	base := filepath.Join(root, "models--owner--repo")
	snapshot := filepath.Join(base, "snapshots", llamaTestCommit)
	for _, dir := range []string{snapshot, filepath.Join(base, "refs"), filepath.Join(base, "blobs")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "refs", "main"), []byte(llamaTestCommit), 0600); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(base, "blobs", "content")
	if err := os.WriteFile(blob, []byte("GGUFfixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tiny-Q4_K_M.gguf", "tiny-Q8_0.gguf"} {
		if err := os.Symlink(blob, filepath.Join(snapshot, name)); err != nil {
			t.Skipf("symlink fixture unavailable: %v", err)
		}
	}
	e := &Executor{}
	st := &engineState{installDir: install}
	deleteModel := func(id string) {
		t.Helper()
		params, _ := json.Marshal(map[string]string{"model": id})
		if _, err := e.llamaModelAction(context.Background(), st, "delete_model", params); err != nil {
			t.Fatal(err)
		}
	}
	deleteModel("owner/repo:Q4_K_M")
	if _, err := os.Stat(blob); err != nil {
		t.Fatal("shared blob removed", err)
	}
	deleteModel("owner/repo:Q8_0")
	if _, err := os.Stat(blob); !os.IsNotExist(err) {
		t.Fatal("orphaned blob retained", err)
	}
}
