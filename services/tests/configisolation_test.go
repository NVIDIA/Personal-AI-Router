// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tests

import (
	"os"
	"testing"
)

// isolatedConfigEnv is the environment for a child that must not touch the
// developer's real PAIR data.
//
// Every variable os.UserConfigDir and appdir consult is set, not only the ones
// the current platform happens to read. A missed variable is not weaker
// isolation — it is no isolation at all on that platform. macOS derives
// ~/Library/Application Support from HOME and ignores the other three, which is
// how a unit test came to rewrite a real proxy port setting and then fail on
// every later run by reading its own leftovers back.
//
// This matters most for the broker: it loads and rewrites workloads-history.json
// and spills a stderr log through appdir regardless of --cluster-dir, so an
// unisolated spawn does not just read real state, it overwrites it.
func isolatedConfigEnv(t *testing.T, extra ...string) []string {
	t.Helper()
	dir := t.TempDir()
	env := append(os.Environ(),
		"HOME="+dir,
		"XDG_CONFIG_HOME="+dir,
		"APPDATA="+dir,
		"LOCALAPPDATA="+dir,
	)
	return append(env, extra...)
}
