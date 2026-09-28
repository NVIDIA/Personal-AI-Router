// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package appdir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersistentModelsSurviveApplicationDirectoryRemoval(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "LOCALAPPDATA"} {
		t.Setenv(key, root)
	}
	app, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	models, err := ModelsDir()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{app, models} {
		if !strings.HasPrefix(path, root+string(filepath.Separator)) {
			t.Fatal("fixture escaped its isolated root")
		}
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(models, "retained.gguf")
	if err := os.WriteFile(file, []byte("GGUFretained"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(app); err != nil {
		t.Fatal(err)
	}
	if bytes, err := os.ReadFile(file); err != nil || string(bytes) != "GGUFretained" {
		t.Fatalf("application removal deleted persistent model data: %v", err)
	}
}
