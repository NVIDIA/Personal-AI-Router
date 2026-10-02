// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A status snapshot reads an ownership record without the PATH lock. A write
// that lands during that read must wait it out, not fail.
func TestReplaceFileWaitsOutAnOpenReader(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "record.json")
	tmp := filepath.Join(dir, "record.json.tmp")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(4 * replaceBackoff)
		_ = reader.Close()
		close(released)
	}()
	if err := replaceFile(tmp, target); err != nil {
		t.Fatalf("replace while a reader held the file: %v", err)
	}
	<-released
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "new" {
		t.Fatalf("target = %q, want the replacement", data)
	}
}
