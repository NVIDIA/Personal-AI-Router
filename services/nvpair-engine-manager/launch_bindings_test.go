// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These synthetic engines exercise different syntax, not per-engine code paths.
// The same fixtures are also usable with the authoring JSON Schema.
func TestDeclarativeLaunchBindings(t *testing.T) {
	data, err := os.ReadFile("testdata/launch-bindings.json")
	require.NoError(t, err)
	var cases []struct {
		Name       string
		Host       string
		Definition EditableLaunch
		Input      string
		Policy     []string
		Invalid    []string
	}
	require.NoError(t, json.Unmarshal(data, &cases))
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			require.NoError(t, tc.Definition.validateControls())
			e := settingsExecutor(t, true)
			rt := &settingsState(t, e).plat.Runtime
			rt.EditableLaunch = &tc.Definition
			if tc.Host == "" {
				tc.Host = "127.0.0.1"
			}
			rt.Bind = tc.Host
			request := settingsRequest(t, e)
			request.Settings.LaunchText, request.Resolution = tc.Input+" --unrelated literal", "launch"
			preview := previewSettings(t, e, request)
			require.Empty(t, preview.Errors, " (%v)", preview)
			require.Nil(t, preview.Conflict, " (%v)", preview)
			require.Equal(t, 23456, preview.Settings.ServerPort, " (%v)", preview)
			policy, err := launchCORSAssignments(preview.Settings.LaunchText, rt.EditableLaunch)
			require.NoError(t, err, "policy (%v, %v)", policy, err)
			assert.Equal(t, tc.Policy, policy)
			request.Settings = preview.Settings
			require.Equal(t, previewSettings(t, e, request), preview, "preview does not round-trip")
			rt.LaunchArgs, rt.LaunchEnv = &preview.Args, &preview.Env
			launch, err := launchForState(settingsState(t, e), 23456)
			require.NoError(t, err)
			require.NoError(t, validateEffectiveLaunch(*rt, launch, tc.Host, "23456"))
			assert.Equal(t, []string{"--unrelated", "literal"}, launch.Args[len(launch.Args)-2:], "unrelated arguments changed")
			for _, invalid := range tc.Invalid {
				request.Settings.LaunchText = invalid
				{
					result := previewSettings(t, e, request)
					require.NotEmpty(t, result.Errors, "accepted (%v)", invalid)
				}
			}
			assertNoSettingsOverride(t, e)
		})
	}
}

func TestManifestLoadRejectsInvalidLaunchBindings(t *testing.T) {
	for _, control := range []string{
		`{"flags":["--x"],"value":"{unknown}"}`,
		`{"flags":["--x"],"value":"{proxy.port}"}`,
		`{"flags":["--x"],"value":"{server.port}{server.host}"}`,
		`{"flags":["--x"],"value":"{server.port}:{server.port}"}`,
		`{"flags":["--x"],"value":"{server.port}:{cors.enabled}"}`,
		`{"flags":["--x"],"value":"{server.port}","implicit":"1234"}`,
		`{"flags":["--x"],"value":"{cors.enabled}","implicit":"maybe"}`,
		`{"env":["X"],"value":"{cors.enabled}","implicit":"true"}`,
		`{"flags":["--x"],"value":"{server.port}","kind":"port"}`,
		`{"flags":["--x"],"value":"{server.port}","values":"typo"}`,
		`{"flags":["--x"],"value":"static"}`,
		`{"flags":["--x"],"value":"{{server.port}}"}`,
	} {
		t.Run(control, func(t *testing.T) {
			// Include otherwise complete bindings so rejection cannot be caused
			// merely by missing managed fields. Exercise the real manifest loader.
			manifest := `{"engine":"fixture","display_name":"Fixture","manifest_version":1,"platforms":{"linux/amd64":{"runtime":{"bin":"fixture","args":[],"editable_launch":{"controls":[{"env":["ADDRESS"],"value":"{server.host}:{server.port}"},` + control + `]}}}}}`
			{
				_, err := NewRegistry().addManifest("fixture.json", []byte(manifest))
				require.Error(t, err, "invalid binding survived manifest loading")
			}
		})
	}
}

func TestManagedBindingRoundTripsWithOverlappingSeparators(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1"} {
		for _, format := range []string{"{server.host}:{server.port}", "{server.port}:{server.host}", "{server.port}0{server.host}", "http://[{server.host}]:{server.port}"} {
			control := LaunchControl{Env: []string{"ENDPOINT"}, Value: format}
			rendered, managed := control.managedValue(host, "23456")
			require.True(t, managed, "invalid binding (%v)", format)
			values := launchValues{host: host}
			normalized, err := values.accept(&control, rendered)
			require.NoError(t, err, "format (%v, %v, %v, %v)", format, host, normalized, err)
			require.Equal(t, rendered, normalized, "format (%v, %v)", format, host)
			require.Equal(t, 23456, values.serverPort(), "format (%v, %v, %v, %v)", format, host, normalized, err)
		}
	}
}

func FuzzLaunchBindings(f *testing.F) {
	for _, seed := range [][2]string{
		{"{server.port}", "23456"},
		{"tcp://{server.host}:{server.port}", "tcp://127.0.0.1:23456"},
		{"port={server.port};host={server.host}", "port=23456;host=127.0.0.1"},
		{"{cors.enabled}", "false"},
		{"{cors.origins}", `"*"`},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, format, input string) {
		control := LaunchControl{Env: []string{"VALUE"}, Value: format}
		values := launchValues{host: "127.0.0.1"}
		normalized, err := values.accept(&control, input)
		if err != nil {
			return
		}
		again, err := values.accept(&control, normalized)
		require.NoError(t, err, "unstable binding normalization (%v, %v, %v)", normalized, again, err)
		require.Equal(t, normalized, again, "unstable binding normalization")
		{
			managed, ok := control.managedValue(values.host, strconv.Itoa(values.serverPort()))
			require.False(t, ok && managed != normalized, "preview and launch disagree (%v, %v)", normalized, managed)
		}
	})
}
