// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

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
			if err := json.Unmarshal([]byte(definition), &rt.EditableLaunch.Controls); err != nil {
				t.Fatal(err)
			}
			if err := rt.EditableLaunch.validateControls(); err != nil {
				t.Fatal(err)
			}
			request := settingsRequest(t, e)
			request.Resolution = "launch"
			if rt.EditableLaunch.Controls[0].Value == "{server.port}" {
				request.Settings.LaunchText = "PORT_ALIAS=23456 HOST_ALIAS=127.0.0.1 CORS_ALIAS=false --future opaque"
			} else {
				request.Settings.LaunchText = "LISTEN_ALIAS=127.0.0.1:23456 ORIGIN_ALIAS=https://example.test -l127.0.0.1:23456 --future opaque"
			}
			preview := previewSettings(t, e, request)
			if len(preview.Errors) > 0 || preview.Conflict != nil {
				t.Fatalf("%+v", preview)
			}
			rt.LaunchArgs, rt.LaunchEnv = &preview.Args, &preview.Env
			launch, err := launchForState(settingsState(t, e), preview.Settings.ServerPort)
			if err != nil {
				t.Fatal(err)
			}
			if err := validateEffectiveLaunch(*rt, launch, "127.0.0.1", "23456"); err != nil {
				t.Fatal(err)
			}
			for _, control := range rt.EditableLaunch.Controls {
				if value, managed := control.managedValue("127.0.0.1", "23456"); managed {
					for _, env := range control.Env {
						if launch.Env[env] != value {
							t.Fatalf("managed alias %s=%q, want %q", env, launch.Env[env], value)
						}
					}
				}
			}
			if !slices.Equal(launch.Args[len(launch.Args)-2:], []string{"--future", "opaque"}) {
				t.Fatal("opaque arguments changed")
			}
			request.Settings.LaunchText = "BROWSER_ACCESS=true CORS_ALIAS=false"
			if rt.EditableLaunch.Controls[0].Value == "{server.host}:{server.port}" {
				request.Settings.LaunchText = "LISTEN=127.0.0.1:23456 LISTEN_ALIAS=0.0.0.0:23456"
			}
			if result := previewSettings(t, e, request); len(result.Errors) == 0 {
				t.Fatal("contradictory source aliases accepted")
			}
		})
	}
}

func TestSavedControlsCannotBypassLaunchValidation(t *testing.T) {
	for _, command := range []bool{false, true} {
		e := settingsExecutor(t, command)
		rt := settingsState(t, e).plat.Runtime
		request := settingsRequest(t, e)
		launch, err := launchForState(settingsState(t, e), request.Settings.ServerPort)
		if err != nil {
			t.Fatal(err)
		}
		if command {
			launch.Args = append(launch.Args, "-p0")
		} else {
			launch.Env["OLLAMA_ORIGINS"] = `"*"`
		}
		if err := validateEffectiveLaunch(rt, launch, "127.0.0.1", fmt.Sprint(request.Settings.ServerPort)); err == nil {
			t.Fatal("unsafe saved launch accepted")
		}
		if command {
			args := []string{"-p0"}
			rt.LaunchArgs = &args
		} else {
			env := []string{`OLLAMA_ORIGINS="*"`}
			rt.LaunchEnv = &env
		}
		settingsState(t, e).plat.Runtime = rt
		if state, err := e.LaunchSettings("fake"); err != nil || !state.Editable {
			t.Fatalf("saved settings must remain available for repair: %+v %v", state, err)
		}
		err = e.Start(context.Background(), "fake")
		if err == nil || !(strings.Contains(err.Error(), "server port") || strings.Contains(err.Error(), "CORS origins")) {
			t.Fatalf("Start did not reject the unsafe saved networking control: %v", err)
		}
	}
}

func TestBundledNetworkingControls(t *testing.T) {
	reg := loadWithOverrides(t, t.TempDir())
	// Adding a bundled engine requires an explicit networking review and cases.
	wantEngines := []string{"lmstudio", "ollama"}
	names := reg.Names()
	slices.Sort(names)
	if !slices.Equal(names, wantEngines) {
		t.Fatalf("review networking controls for every bundled engine: %v", names)
	}
	for _, name := range names {
		manifest, _ := reg.Get(name)
		for platform, config := range manifest.Platforms {
			t.Run(name+"/"+platform, func(t *testing.T) {
				e := settingsExecutor(t, config.Runtime.modeOrDefault() == "command")
				settingsState(t, e).plat.Runtime = config.Runtime
				policy := config.Runtime.EditableLaunch
				if policy == nil {
					t.Fatal("missing reviewed networking controls")
				}
				var valid, invalid []string
				if name == "lmstudio" {
					if !reflect.DeepEqual(policy.Controls, []LaunchControl{{Value: "{server.port}", Flags: []string{"--port", "-p"}}, {Value: "{server.host}", Flags: []string{"--bind"}, Env: []string{"LMS_SERVER_HOST"}}, {Value: "{cors.enabled}", Implicit: implicitLaunchValue("true"), Flags: []string{"--cors"}}}) {
						t.Fatal("incomplete LM Studio controls")
					}
					valid = []string{"--port 23456", "--port=23456", "-p 23456", "-p23456", "-p=23456", `"-p" "23456"`, "--port 23456 -p23456", "-- -p23456"}
					invalid = []string{"-p0", "-p65536", "-p", "-pno", "--port 23456 -p23457", "-vp23456", "-vp=23456", "--bind 0.0.0.0", "--bind=::", "LMS_SERVER_HOST=0.0.0.0", "--cors=false", "--cors=true", "--cors=", "-- --bind 0.0.0.0"}
				} else {
					if !reflect.DeepEqual(policy.Controls, []LaunchControl{{Value: "{server.host}:{server.port}", Env: []string{"OLLAMA_HOST"}}, {Value: "{cors.origins}", Env: []string{"OLLAMA_ORIGINS"}}}) {
						t.Fatal("incomplete Ollama controls")
					}
					valid = []string{`OLLAMA_HOST="127.0.0.1:23456"`, `OLLAMA_HOST='127.0.0.1:23456'`, "OLLAMA_HOST=127.0.0.1:23456"}
					invalid = []string{"OLLAMA_HOST=0.0.0.0:23456", "OLLAMA_HOST=127.0.0.1:0", "OLLAMA_HOST=127.0.0.1:65536", `OLLAMA_ORIGINS='"*"'`, `OLLAMA_ORIGINS="'*'"`, `OLLAMA_ORIGINS='"http://*"'`, "OLLAMA_ORIGINS=http://localhost,*", "OLLAMA_HOST=127.0.0.1:23456 OLLAMA_HOST=0.0.0.0:23456"}
				}
				for _, text := range valid {
					p := settingsRequest(t, e)
					p.Settings.LaunchText, p.Resolution = text, "launch"
					preview := previewSettings(t, e, p)
					if len(preview.Errors) != 0 || preview.Conflict != nil || preview.Settings.ServerPort != 23456 {
						t.Fatalf("%q: %+v", text, preview)
					}
					p.Settings = preview.Settings
					again := previewSettings(t, e, p)
					if !reflect.DeepEqual(preview, again) {
						t.Fatalf("normalization not stable for %q", text)
					}
					if slices.Contains(preview.Args, "-p") || slices.Contains(preview.Args, "-p23456") {
						t.Fatal("managed alias escaped into user args")
					}
				}
				for _, text := range invalid {
					p := settingsRequest(t, e)
					p.Settings.LaunchText = text
					if result := previewSettings(t, e, p); len(result.Errors) == 0 {
						t.Fatalf("accepted %q: %+v", text, result)
					}
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
		if len(result.Errors) != 0 || result.Settings.ServerPort != 23456 || !slices.Equal(result.Args, []string{"--origins", "https://example.test"}) {
			t.Fatalf("%q: %+v", text, result)
		}
	}
	for _, text := range []string{"--address=0.0.0.0", "-b0.0.0.0", "--browser-origins=*", "-o*", "-vc", "-vohttps://example.test", "-ohttps://one.test --origins=https://two.test"} {
		request := settingsRequest(t, e)
		request.Settings.LaunchText = text
		if result := previewSettings(t, e, request); len(result.Errors) == 0 {
			t.Fatalf("accepted %q", text)
		}
	}
}

func TestCORSCanonicalization(t *testing.T) {
	for _, text := range []string{`"*"`, `'*'`, `" * "`, `"http://*"`, "http://*:80", "*://*", "https://example.test,*", "https://foo*", "https://*.*", "https://example.test/path", "https://user@example.test", "https://example.test?query", "https://example.test#fragment", "https://example.test:65536", `https://example.test\anything`} {
		if _, err := normalizeCORSOrigins(text); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
	for _, value := range []struct{ input, want string }{
		{`"https://Example.Test"`, "https://example.test"},
		{" https://example.test/ , http://localhost:* ", "https://example.test,http://localhost:*"},
		{"https://*.example.test", "https://*.example.test"},
		{"http://[::1]:1234", "http://[::1]:1234"},
		{"", ""},
	} {
		got, err := normalizeCORSOrigins(value.input)
		if err != nil || got != value.want {
			t.Errorf("%q: %q %v", value.input, got, err)
		}
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
			if _, err := e.PreviewLaunch(request); err == nil {
				t.Fatal("authoritative preview allowed a remote CORS change")
			}
			request.PreserveCORS = false
			if _, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			request = settingsRequest(t, e)
			if command {
				request.Settings.LaunchText = "--cors --cors"
			} else {
				request.Settings.LaunchText = `OLLAMA_ORIGINS='"https://EXAMPLE.test/"'`
			}
			request.PreserveCORS = true
			if _, err := e.PreviewLaunch(request); err != nil {
				t.Fatalf("equivalent policy rejected: %v", err)
			}
			request.Settings.LaunchText = ""
			if _, err := e.PreviewLaunch(request); err == nil {
				t.Fatal("remote removal changed policy")
			}
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
			if _, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			request = settingsRequest(t, e)
			request.PreserveCORS = true
			if _, err := e.PreviewLaunch(request); err != nil {
				t.Fatalf("unchanged policy rejected: %v", err)
			}
			request.Settings.LaunchText = ""
			if _, err := e.PreviewLaunch(request); err == nil {
				t.Fatal("remote removal of disabling override accepted")
			}
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
		if err := policy.validateControls(); err == nil {
			t.Errorf("accepted %+v", policy)
		}
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
		if err != nil || again != got {
			t.Fatalf("unstable normalization: %q -> %q -> %q", value, got, again)
		}
		if strings.ContainsAny(got, "\"'") {
			t.Fatal("literal quote survives CORS validation")
		}
	})
}

func implicitLaunchValue(value string) *string { return &value }
