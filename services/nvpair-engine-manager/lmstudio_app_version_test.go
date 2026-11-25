// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLMStudioAppVersion_UsesBuildFromMatchingRunningDesktopRelease(t *testing.T) {
	test := func(name, running, history, expected string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			internal := filepath.Join(root, ".internal")
			require.NoError(t, os.MkdirAll(internal, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(internal, "historical-version-info.json"), []byte(history), 0o600))

			version, err := lmStudioAppVersion(root, running)
			require.NoError(t, err)
			assert.Equal(t, expected, version)
		})
	}
	test("running build is authoritative", "0.4.28+2", "{}", "0.4.28-2")
	test("desktop and daemon share a home", "0.4.27", `{"targetHistories":[
		{"target":"daemon","lastRecordedAppVersion":"0.4.27","lastRecordedAppBuildVersion":"8"},
		{"target":"lmstudio","lastRecordedAppVersion":"0.4.27","lastRecordedAppBuildVersion":"2"}
	]}`, "0.4.27-2")
	test("earlier desktop format", "0.3.37", `{"lastRecorderdAppVersion":"0.3.37","lastRecordedAppBuildVersion":"1"}`, "0.3.37-1")
	test("previous desktop release", "0.4.27", `{"targetHistories":[
		{"target":"lmstudio","lastRecordedAppVersion":"0.4.26","lastRecordedAppBuildVersion":"5"}
	]}`, "0.4.27")
	test("daemon history only", "0.4.27", `{"targetHistories":[
		{"target":"daemon","lastRecordedAppVersion":"0.4.27","lastRecordedAppBuildVersion":"8"}
	]}`, "0.4.27")
}

func TestLMStudioAppVersion_ReadsStoppedDesktopPackage(t *testing.T) {
	test := func(name, executable, packagePath string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			exe := filepath.Join(root, executable)
			require.NoError(t, os.MkdirAll(filepath.Dir(exe), 0o755))
			require.NoError(t, os.WriteFile(exe, nil, 0o755))
			internal := filepath.Join(root, ".internal")
			require.NoError(t, os.MkdirAll(internal, 0o755))
			marker, err := json.Marshal(map[string]string{"path": exe})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(internal, "app-install-location.json"), marker, 0o600))
			manifest := filepath.Join(root, packagePath)
			require.NoError(t, os.MkdirAll(filepath.Dir(manifest), 0o755))
			require.NoError(t, os.WriteFile(manifest, []byte(`{"name":"lm-studio","version":"0.4.28+2"}`), 0o600))
			// The user updated the desktop app without opening the new build yet.
			require.NoError(t, os.WriteFile(filepath.Join(internal, "historical-version-info.json"),
				[]byte(`{"lastRecorderdAppVersion":"0.4.28","lastRecordedAppBuildVersion":"1"}`), 0o600))

			version, err := lmStudioAppVersion(root, "")
			require.NoError(t, err)
			assert.Equal(t, "0.4.28-2", version)
		})
	}
	test("unpacked application", "desktop/lm-studio", "desktop/resources/app/package.json")
	test("macOS application bundle", "LM Studio.app/Contents/MacOS/LM Studio", "LM Studio.app/Contents/Resources/app/package.json")
}

func TestLMStudioAppVersion_IgnoresRemovedDesktopInstall(t *testing.T) {
	root := t.TempDir()
	internal := filepath.Join(root, ".internal")
	require.NoError(t, os.MkdirAll(internal, 0o755))
	marker, err := json.Marshal(map[string]string{"path": filepath.Join(root, "uninstalled", "lm-studio")})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(internal, "app-install-location.json"), marker, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(internal, "historical-version-info.json"),
		[]byte(`{"lastRecorderdAppVersion":"0.4.27","lastRecordedAppBuildVersion":"2"}`), 0o600))

	version, err := lmStudioAppVersion(root, "")
	require.NoError(t, err)
	assert.Empty(t, version)
}
