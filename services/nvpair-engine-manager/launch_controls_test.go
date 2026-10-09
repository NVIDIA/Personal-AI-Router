// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"

	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	settings "nvpair-shared/enginesettings"
)

func TestGenericControlSourcesAndLaunchConstruction(t *testing.T) {
	for _, definition := range []string{
		`[{"value":"{server.port}","env":["SERVER_PORT","PORT_ALIAS"]},{"value":"{server.host}","env":["SERVER_HOST","HOST_ALIAS"]},{"value":"{cors.enabled}","flags":["--browser-access","-c"],"env":["BROWSER_ACCESS","CORS_ALIAS"]}]`,
		`[{"value":"{server.host}:{server.port}","flags":["--listen","-l"],"env":["LISTEN","LISTEN_ALIAS"]},{"value":"{cors.origins}","flags":["--origins","-o"],"env":["ORIGINS","ORIGIN_ALIAS"]}]`,
	} {
		t.Run(definition, func(t *testing.T) {
			e := settingsExecutor(t, true)
			rt := &settingsState(t, e).plat.Runtime
			require.NoError(t, json.Unmarshal([]byte(definition), &rt.EditableLaunch.Controls))
			require.NoError(t, rt.EditableLaunch.validateControls())
			request := settingsRequest(t, e)
			request.Resolution = "launch"
			if rt.EditableLaunch.Controls[0].Value == "{server.port}" {
				request.Settings.LaunchText = "PORT_ALIAS=23456 HOST_ALIAS=127.0.0.1 CORS_ALIAS=false --future opaque"
			} else {
				request.Settings.LaunchText = "LISTEN_ALIAS=127.0.0.1:23456 ORIGIN_ALIAS=https://example.test -l127.0.0.1:23456 --future opaque"
			}
			preview := previewSettings(t, e, request)
			require.Empty(t, preview.Errors, " (%v)", preview)
			require.Nil(t, preview.Conflict, " (%v)", preview)
			rt.LaunchArgs, rt.LaunchEnv = &preview.Args, &preview.Env
			launch, err := launchForState(settingsState(t, e), preview.Settings.ServerPort)
			require.NoError(t, err)
			require.NoError(t, validateEffectiveLaunch(*rt, launch, "127.0.0.1", "23456"))
			for _, control := range rt.EditableLaunch.Controls {
				if value, managed := control.managedValue("127.0.0.1", "23456"); managed {
					for _, env := range control.Env {
						require.Equal(t, value, launch.Env[env], "managed alias (%v)", env)
					}
				}
			}
			assert.Equal(t, []string{"--future", "opaque"}, launch.Args[len(launch.Args)-2:], "opaque arguments changed")
			request.Settings.LaunchText = "BROWSER_ACCESS=true CORS_ALIAS=false"
			if rt.EditableLaunch.Controls[0].Value == "{server.host}:{server.port}" {
				request.Settings.LaunchText = "LISTEN=127.0.0.1:23456 LISTEN_ALIAS=0.0.0.0:23456"
			}
			require.NotEmpty(t, previewSettings(t, e, request).Errors, "contradictory source aliases accepted")
		})
	}
}

func TestSavedControlsCannotBypassLaunchValidation(t *testing.T) {
	for _, command := range []bool{false, true} {
		e := settingsExecutor(t, command)
		rt := settingsState(t, e).plat.Runtime
		request := settingsRequest(t, e)
		launch, err := launchForState(settingsState(t, e), request.Settings.ServerPort)
		require.NoError(t, err)
		if command {
			launch.Args = append(launch.Args, "-p0")
		} else {
			launch.Env["OLLAMA_ORIGINS"] = `"*"`
		}
		require.Error(t, validateEffectiveLaunch(rt, launch, "127.0.0.1", fmt.Sprint(request.Settings.ServerPort)), "unsafe saved launch accepted")
		if command {
			args := []string{"-p0"}
			rt.LaunchArgs = &args
		} else {
			env := []string{`OLLAMA_ORIGINS="*"`}
			rt.LaunchEnv = &env
		}
		settingsState(t, e).plat.Runtime = rt
		state, err := e.LaunchSettings("fake")
		require.NoError(t, err, "saved settings must remain available for repair (%v, %v)", state, err)
		require.True(t, state.Editable, "saved settings must remain available for repair (%v, %v)", state, err)
		err = e.Start(context.Background(), "fake")
		require.Error(t, err, "Start did not reject the unsafe saved networking control")
		require.True(t, strings.Contains(err.Error(), "server port") || strings.Contains(err.Error(), "CORS origins"), "Start did not reject the unsafe saved networking control (%v)", err)
	}
}

func TestBundledLlamaCPPDisablesCORSByDefault(t *testing.T) {
	reg := loadWithOverrides(t, t.TempDir())
	manifest, ok := reg.Get("llamacpp")
	require.True(t, ok, "llama.cpp manifest not loaded")
	for platform, config := range manifest.Platforms {
		t.Run(platform, func(t *testing.T) {
			e := settingsExecutor(t, false)
			graftPlatform(t, e, config)
			state, err := e.LaunchSettings("fake")
			require.NoError(t, err)
			got, err := launchCORSAssignments(state.LaunchText, config.Runtime.EditableLaunch)
			require.NoError(t, err)
			require.Equal(t, []string{"cors.origins="}, got, "default CORS policy")
		})
	}
}

func TestBundledNetworkingControls(t *testing.T) {
	reg := loadWithOverrides(t, t.TempDir())
	// Adding a bundled engine requires an explicit networking review and cases.
	wantEngines := []string{"llamacpp", "lmstudio", "ollama"}
	names := reg.Names()
	slices.Sort(names)
	assert.Equal(t, wantEngines, names, "review networking controls for every bundled engine")
	for _, name := range names {
		manifest, _ := reg.Get(name)
		for platform, config := range manifest.Platforms {
			t.Run(name+"/"+platform, func(t *testing.T) {
				e := settingsExecutor(t, config.Runtime.modeOrDefault() == "command")
				graftPlatform(t, e, config)
				policy := config.Runtime.EditableLaunch
				require.NotNil(t, policy, "missing reviewed networking controls")
				var valid, invalid []string
				switch name {
				case "lmstudio":
					require.Equal(t, []LaunchControl{{Value: "{server.port}", Flags: []string{"--port", "-p"}}, {Value: "{server.host}", Flags: []string{"--bind"}, Env: []string{"LMS_SERVER_HOST"}}, {Value: "{cors.enabled}", Implicit: implicitLaunchValue("true"), Flags: []string{"--cors"}}}, policy.Controls, "incomplete LM Studio controls")
					valid = []string{"--port 23456", "--port=23456", "-p 23456", "-p23456", "-p=23456", `"-p" "23456"`, "--port 23456 -p23456", "-- -p23456"}
					invalid = []string{"-p0", "-p65536", "-p", "-pno", "--port 23456 -p23457", "-vp23456", "-vp=23456", "--bind 0.0.0.0", "--bind=::", "LMS_SERVER_HOST=0.0.0.0", "--cors=false", "--cors=true", "--cors=", "-- --bind 0.0.0.0"}
				case "llamacpp":
					require.Equal(t, []string{"--sleep-idle-seconds", "300"}, policy.FixedArgs, "llama.cpp idle sleep policy is not fixed")
					require.Equal(t, []LaunchControl{{Value: "{server.host}", Flags: []string{"--host"}}, {Value: "{server.port}", Flags: []string{"--port"}}, {Value: "{cors.origins}", Flags: []string{"--cors-origins"}}}, policy.Controls, "incomplete llama.cpp controls")
					valid = []string{"--host 127.0.0.1 --port 23456", "--host=127.0.0.1 --port=23456", "--port 23456 --cors-origins https://example.test"}
					invalid = []string{"--host 0.0.0.0", "--host=::", "--port 0", "--port 65536", "--port", "--cors-origins=*", "--port 23456 --port 23457", "-- --host 0.0.0.0"}
				default:
					require.Equal(t, []LaunchControl{{Value: "{server.host}:{server.port}", Env: []string{"OLLAMA_HOST"}}, {Value: "{cors.origins}", Env: []string{"OLLAMA_ORIGINS"}}}, policy.Controls, "incomplete Ollama controls")
					valid = []string{`OLLAMA_HOST="127.0.0.1:23456"`, `OLLAMA_HOST='127.0.0.1:23456'`, "OLLAMA_HOST=127.0.0.1:23456"}
					invalid = []string{"OLLAMA_HOST=0.0.0.0:23456", "OLLAMA_HOST=127.0.0.1:0", "OLLAMA_HOST=127.0.0.1:65536", `OLLAMA_ORIGINS='"*"'`, `OLLAMA_ORIGINS="'*'"`, `OLLAMA_ORIGINS='"http://*"'`, "OLLAMA_ORIGINS=http://localhost,*", "OLLAMA_HOST=127.0.0.1:23456 OLLAMA_HOST=0.0.0.0:23456"}
				}
				for _, text := range valid {
					p := settingsRequest(t, e)
					p.Settings.LaunchText, p.Resolution = text, "launch"
					preview := previewSettings(t, e, p)
					require.Empty(t, preview.Errors, " (%v, %v)", text, preview)
					require.Nil(t, preview.Conflict, " (%v, %v)", text, preview)
					require.Equal(t, 23456, preview.Settings.ServerPort, " (%v, %v)", text, preview)
					p.Settings = preview.Settings
					again := previewSettings(t, e, p)
					require.Equal(t, again, preview, "normalization not stable for (%v)", text)
					require.NotContains(t, preview.Args, "-p", "managed alias escaped into user args")
					require.NotContains(t, preview.Args, "-p23456", "managed alias escaped into user args")
				}
				for _, text := range invalid {
					p := settingsRequest(t, e)
					p.Settings.LaunchText = text
					result := previewSettings(t, e, p)
					require.NotEmpty(t, result.Errors, "accepted (%v, %v)", text, result)
				}
				assertNoSettingsOverride(t, e)
			})
		}
	}
}

func TestNetworkingControlAliasesAreEngineIndependent(t *testing.T) {
	e := settingsExecutor(t, true)
	p := settingsState(t, e).plat.Runtime.EditableLaunch
	p.Controls = []LaunchControl{
		{Value: "{server.port}", Flags: []string{"--listener-port", "--http-port", "-x"}},
		{Value: "{server.host}", Flags: []string{"--listener-address", "--address", "-b"}},
		{Value: "{cors.origins}", Flags: []string{"--origins", "--browser-origins", "-o"}},
		{Value: "{cors.enabled}", Implicit: implicitLaunchValue("true"), Flags: []string{"--browser-access", "--allow-browser", "-c"}},
	}

	for _, text := range []string{"--http-port=23456 -b127.0.0.1 -ohttps://example.test", "-x23456 --address=127.0.0.1 --browser-origins=https://example.test"} {
		request := settingsRequest(t, e)
		request.Settings.LaunchText = text
		request.Resolution = "launch"
		result := previewSettings(t, e, request)
		require.Empty(t, result.Errors, " (%v, %v)", text, result)
		require.Equal(t, 23456, result.Settings.ServerPort, " (%v, %v)", text, result)
		assert.Equal(t, []string{"--origins", "https://example.test"}, result.Args, " (%v)", text)
	}
	for _, text := range []string{"--address=0.0.0.0", "-b0.0.0.0", "--browser-origins=*", "-o*", "-vc", "-vohttps://example.test", "-ohttps://one.test --origins=https://two.test"} {
		request := settingsRequest(t, e)
		request.Settings.LaunchText = text
		require.NotEmpty(t, previewSettings(t, e, request).Errors, "accepted (%v)", text)
	}
}

func TestCORSCanonicalization(t *testing.T) {
	for _, text := range []string{`"*"`, `'*'`, `" * "`, `"http://*"`, "http://*:80", "*://*", "https://example.test,*", "https://foo*", "https://*.*", "https://example.test/path", "https://user@example.test", "https://example.test?query", "https://example.test#fragment", "https://example.test:65536", `https://example.test\anything`} {
		_, err := normalizeCORSOrigins(text)
		assert.Error(t, err, "accepted (%v)", text)
	}
	for _, value := range []struct{ input, want string }{
		{`"https://Example.Test"`, "https://example.test"},
		{" https://example.test/ , http://localhost:* ", "https://example.test,http://localhost:*"},
		{"https://*.example.test", "https://*.example.test"},
		{"http://[::1]:1234", "http://[::1]:1234"},
		{"", ""},
	} {
		got, err := normalizeCORSOrigins(value.input)
		assert.NoError(t, err, " (%v, %v)", got, err)
		assert.Equal(t, value.want, got)
	}
}

func TestRemoteCORSUsesCanonicalPolicyAndAuthoritativePreview(t *testing.T) {
	for _, command := range []bool{false, true} {
		t.Run(fmt.Sprint(command), func(t *testing.T) {
			e := settingsExecutor(t, command)
			request := settingsRequest(t, e)
			changed := "OLLAMA_ORIGINS=https://example.test"
			if command {
				changed = "--cors"
			}
			request.Settings.LaunchText = changed
			request.PreserveCORS = true
			_, err := e.PreviewLaunch(request)
			require.Error(t, err, "authoritative preview allowed a remote CORS change")
			request.PreserveCORS = false
			_, err = e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func() error { return nil })
			require.NoError(t, err)
			request = settingsRequest(t, e)
			if command {
				request.Settings.LaunchText = "--cors --cors"
			} else {
				request.Settings.LaunchText = `OLLAMA_ORIGINS='"https://EXAMPLE.test/"'`
			}
			request.PreserveCORS = true
			_, err = e.PreviewLaunch(request)
			require.NoError(t, err, "equivalent policy rejected")
			request.Settings.LaunchText = ""
			_, err = e.PreviewLaunch(request)
			require.Error(t, err, "remote removal changed policy")
		})
	}
}

// Removing a disabling override may restore permissive inherited/default values.
func TestRemoteCORSCannotRemoveExplicitDisablingValues(t *testing.T) {
	for _, kind := range []string{"cors.enabled", "cors.origins"} {
		t.Run(kind, func(t *testing.T) {
			e := settingsExecutor(t, true)
			policy := settingsState(t, e).plat.Runtime.EditableLaunch
			policy.Controls = append(policy.Controls, LaunchControl{Value: "{" + kind + "}", Env: []string{"BROWSER_POLICY"}})
			request := settingsRequest(t, e)
			request.Settings.LaunchText = "BROWSER_POLICY=false"
			if kind == "cors.origins" {
				request.Settings.LaunchText = "BROWSER_POLICY="
			}
			_, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func() error { return nil })
			require.NoError(t, err)
			request = settingsRequest(t, e)
			request.PreserveCORS = true
			_, err = e.PreviewLaunch(request)
			require.NoError(t, err, "unchanged policy rejected")
			request.Settings.LaunchText = ""
			_, err = e.PreviewLaunch(request)
			require.Error(t, err, "remote removal of disabling override accepted")
		})
	}
}

func TestNetworkingManifestRejectsAmbiguousDeclarations(t *testing.T) {
	for _, controls := range [][]LaunchControl{
		{{Value: "{server.port}"}},
		{{Value: "{server.port}", Flags: []string{"--port"}}, {Value: "{server.host}", Flags: []string{"--port"}}},
		{{Value: "{server.port}", Flags: []string{"--port", "--port"}}},
		{{Value: "{server.port}", Flags: []string{"--port", "-p"}}, {Value: "{cors.enabled}", Implicit: implicitLaunchValue("true"), Flags: []string{"-p"}}},
		{{Value: "{server.port}", Env: []string{"PORT"}}, {Value: "{server.host}", Env: []string{"PORT"}}},
		{{Value: "{cors.enabled}", Implicit: implicitLaunchValue("true"), Flags: []string{"--cors=true"}}},
		{{Value: "{unknown}", Flags: []string{"--x"}}},
		{{Value: "{cors.enabled}", Implicit: implicitLaunchValue("true"), Env: []string{"CORS"}}},
		{{Value: "{server.port}", Flags: []string{"--port"}}},
	} {
		policy := EditableLaunch{Controls: controls}
		assert.Error(t, policy.validateControls(), "accepted (%v)", policy)
	}

}

func FuzzCORSNormalization(f *testing.F) {
	for _, seed := range []string{`"*"`, "https://*.example.test", "http://localhost:*", "https://example.test"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		got, err := normalizeCORSOrigins(value)
		if err != nil {
			return
		}
		again, err := normalizeCORSOrigins(got)
		require.NoError(t, err, "unstable normalization (%v, %v, %v)", value, got, again)
		require.Equal(t, got, again, "unstable normalization of %q", value)
		require.NotContains(t, got, "\"", "literal quote survives CORS validation")
		require.NotContains(t, got, "'", "literal quote survives CORS validation")
	})
}

func implicitLaunchValue(value string) *string { return &value }
