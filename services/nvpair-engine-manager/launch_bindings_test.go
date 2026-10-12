// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strconv"
	"testing"
)

// These synthetic engines exercise different syntax, not per-engine code paths.
// The same fixtures are also usable with the authoring JSON Schema.
func TestDeclarativeLaunchBindings(t *testing.T) {
	data, err := os.ReadFile("testdata/launch-bindings.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name       string
		Host       string
		Definition EditableLaunch
		Input      string
		Policy     []string
		Invalid    []string
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			if err := tc.Definition.validateControls(); err != nil {
				t.Fatal(err)
			}
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
			if len(preview.Errors) != 0 || preview.Conflict != nil || preview.Settings.ServerPort != 23456 {
				t.Fatalf("%+v", preview)
			}
			policy, err := launchCORSAssignments(preview.Settings.LaunchText, rt.EditableLaunch)
			if err != nil || !slices.Equal(policy, tc.Policy) {
				t.Fatalf("policy=%v, want %v: %v", policy, tc.Policy, err)
			}
			request.Settings = preview.Settings
			if again := previewSettings(t, e, request); !reflect.DeepEqual(preview, again) {
				t.Fatal("preview does not round-trip")
			}
			rt.LaunchArgs, rt.LaunchEnv = &preview.Args, &preview.Env
			launch, err := launchForState(settingsState(t, e), 23456)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateEffectiveLaunch(*rt, launch, tc.Host, "23456"); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(launch.Args[len(launch.Args)-2:], []string{"--unrelated", "literal"}) {
				t.Fatal("unrelated arguments changed")
			}
			for _, invalid := range tc.Invalid {
				request.Settings.LaunchText = invalid
				if result := previewSettings(t, e, request); len(result.Errors) == 0 {
					t.Fatalf("accepted %q", invalid)
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
			if _, err := NewRegistry().addManifest("fixture.json", []byte(manifest)); err == nil {
				t.Fatal("invalid binding survived manifest loading")
			}
		})
	}
}

func TestManagedBindingRoundTripsWithOverlappingSeparators(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1"} {
		for _, format := range []string{"{server.host}:{server.port}", "{server.port}:{server.host}", "{server.port}0{server.host}", "http://[{server.host}]:{server.port}"} {
			control := LaunchControl{Env: []string{"ENDPOINT"}, Value: format}
			rendered, managed := control.managedValue(host, "23456")
			if !managed {
				t.Fatalf("invalid binding %q", format)
			}
			values := launchValues{host: host}
			normalized, err := values.accept(&control, rendered)
			if err != nil || normalized != rendered || values.serverPort() != 23456 {
				t.Fatalf("format %q host %q: %q (%v)", format, host, normalized, err)
			}
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
		if err != nil || again != normalized {
			t.Fatalf("unstable binding normalization: %q -> %q (%v)", normalized, again, err)
		}
		if managed, ok := control.managedValue(values.host, strconv.Itoa(values.serverPort())); ok && managed != normalized {
			t.Fatalf("preview and launch disagree: %q / %q", normalized, managed)
		}
	})
}
