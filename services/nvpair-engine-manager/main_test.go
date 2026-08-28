// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Built once for the whole package: a pure-Go fake engine (stand-in for
// a real inference engine) and the engine-manager binary itself (for
// the cross-process e2e test).
var (
	fakeEngineBin string
	managerBin    string
)

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "nvpair-em-test-*")
	if err != nil {
		panic(err)
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}

	fakeEngineBin = filepath.Join(tmp, "fake-engine"+suffix)
	if out, err := exec.Command("go", "build", "-o", fakeEngineBin, "./testdata/fakeengine").CombinedOutput(); err != nil {
		panic("build fake-engine: " + err.Error() + "\n" + string(out))
	}

	managerBin = filepath.Join(tmp, "nvpair-engine-manager"+suffix)
	if out, err := exec.Command("go", "build", "-o", managerBin, ".").CombinedOutput(); err != nil {
		panic("build engine-manager: " + err.Error() + "\n" + string(out))
	}

	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}
