// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command fakehf removes an empty model repository for portable cache tests.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if len(os.Args) != 7 || os.Args[1] != "cache" || os.Args[2] != "rm" || !strings.HasPrefix(os.Args[3], "model/") || os.Args[4] != "--cache-dir" || os.Args[6] != "--yes" {
		fmt.Fprintln(os.Stderr, "expected cache rm model/<id> --cache-dir <dir> --yes")
		os.Exit(2)
	}
	id := strings.TrimPrefix(os.Args[3], "model/")
	repo := filepath.Join(os.Args[5], "models--"+strings.ReplaceAll(id, "/", "--"))
	if err := os.Remove(repo); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
