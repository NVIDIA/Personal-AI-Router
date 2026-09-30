// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"os"
	"testing"
)

// configEnvKeys are the variables os.UserConfigDir, os.UserHomeDir and appdir
// read on some platform. All of them are set, not only the ones the current
// platform reads: macOS roots its config under HOME, Linux uses
// XDG_CONFIG_HOME, and Windows uses APPDATA and LOCALAPPDATA, with USERPROFILE
// as the home an engine manifest can name, such as LM Studio's
// %USERPROFILE%\.lmstudio.
var configEnvKeys = []string{"HOME", "XDG_CONFIG_HOME", "APPDATA", "LOCALAPPDATA", "USERPROFILE"}

// configEnv is this process's environment with every configEnvKeys variable
// pointed at dir, followed by extra. os/exec keeps the last of a duplicated
// variable, so these override the inherited values.
func configEnv(dir string, extra ...string) []string {
	env := os.Environ()
	for _, key := range configEnvKeys {
		env = append(env, key+"="+dir)
	}
	return append(env, extra...)
}

// isolatedConfigEnv gives one child a config directory of its own.
//
// TestMain already points this process, and so every child, at a private root,
// which keeps the developer's data out of reach. A directory per child keeps
// children apart from each other: brokers persist workloads-history.json there,
// and the remote-engine tests run two engine managers as separate nodes, which
// must not share engines, engine-bin, or desired state.
func isolatedConfigEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	return configEnv(t.TempDir(), extra...)
}
