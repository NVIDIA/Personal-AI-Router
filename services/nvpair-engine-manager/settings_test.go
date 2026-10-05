// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	settings "nvpair-shared/enginesettings"
)

// graftPlatform points the fake engine at a bundled platform's runtime.
//
// Both fields move together: a runtime referencing {models_dir} — llama.cpp's
// LLAMA_CACHE does — needs the store that goes with it, and an unset models_dir
// is deliberately not published as a placeholder, so grafting the runtime alone
// leaves a launch that cannot resolve.
func graftPlatform(t *testing.T, e *Executor, platform Platform) {
	t.Helper()
	state := settingsState(t, e)
	state.plat.Runtime = platform.Runtime
	state.modelsDir = expandPath(platform.ModelsDir)
}

func settingsExecutor(t *testing.T, command bool) *Executor {
	t.Helper()
	m := testEngineManifest(fakeEngineBin)
	key := runtime.GOOS + "/" + runtime.GOARCH
	p := m.Platforms[key]
	port, err := freePort()
	require.NoError(t, err)
	p.Runtime.Port = port
	p.Runtime.Args = []string{"serve"}
	p.Runtime.EditableLaunch = &EditableLaunch{FixedArgs: []string{"serve"}, Controls: []LaunchControl{{Value: "{server.host}:{server.port}", Env: []string{"OLLAMA_HOST"}}, {Value: "{cors.origins}", Env: []string{"OLLAMA_ORIGINS"}}}}
	if command {
		p.Runtime.Mode = "command"
		p.Runtime.CLI = fakeEngineBin
		p.Runtime.Start = [][]string{{"{cli}", "server", "start", "--port", "{port}", "--bind", "{host}"}}
		p.Runtime.EditableLaunch = &EditableLaunch{FixedArgs: []string{"server", "start"}, Controls: []LaunchControl{{Value: "{server.port}", Flags: []string{"--port", "-p"}}, {Value: "{server.host}", Flags: []string{"--bind"}, Env: []string{"LMS_SERVER_HOST"}}, {Value: "{cors.enabled}", Implicit: implicitLaunchValue("true"), Flags: []string{"--cors"}}}}
	}
	m.Platforms[key] = p
	e := newTestExecutor(t, m)
	e.overrideDir = t.TempDir()
	{
		_, err := e.Detect("fake")
		require.NoError(t, err)
	}
	t.Cleanup(func() { _ = e.Stop("fake") })
	return e
}

func settingsRequest(t *testing.T, e *Executor) settings.Request {
	t.Helper()
	s, err := e.LaunchSettings("fake")
	require.NoError(t, err)
	return settings.Request{Engine: "fake", Settings: settings.Config{ServerPort: s.ServerPort, ProxyPort: 54301, LaunchText: s.LaunchText}}
}

// eachEngineMode runs test against both Runtime.Mode lifecycles: "process",
// where this service spawns the engine and its managed controls bind to
// environment variables, and "command", where a control CLI brings the engine
// up and the same controls bind to flags. The launch settings path must behave
// identically either way, so every mode-independent test here runs twice.
func eachEngineMode(t *testing.T, test func(*testing.T, *Executor)) {
	t.Helper()
	t.Run("process mode", func(t *testing.T) { test(t, settingsExecutor(t, false)) })
	t.Run("command mode", func(t *testing.T) { test(t, settingsExecutor(t, true)) })
}

func previewSettings(t *testing.T, e *Executor, request settings.Request) settings.Preview {
	t.Helper()
	preview, err := e.PreviewLaunch(request)
	require.NoError(t, err, "PreviewLaunch")
	return preview
}

func settingsState(t *testing.T, e *Executor) *engineState {
	t.Helper()
	state, err := e.state("fake")
	require.NoError(t, err)
	return state
}

func assertNoSettingsOverride(t *testing.T, e *Executor) {
	t.Helper()
	entries, err := os.ReadDir(e.overrideDir)
	require.NoError(t, err)
	require.Len(t, entries, 0, "rejected operation wrote configuration")
}

func TestSettingsPreviewPreservesLiteralArguments(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		request := settingsRequest(t, e)
		request.Settings.LaunchText += ` --parallel 3 "two words" "" "{port}" "$HOME"`
		preview := previewSettings(t, e, request)
		require.Len(t, preview.Errors, 0, "valid preview (%v)", preview)
		require.Nil(t, preview.Conflict, "valid preview (%v)", preview)
		want := []string{"--parallel", "3", "two words", "", "{port}", "$HOME"}
		require.Equal(t, want, preview.Args, "literal args")
		assertNoSettingsOverride(t, e)
	})
}

func TestSettingsPreviewPortConflict(t *testing.T) {
	test := func(name, resolution string) {
		t.Run(name, func(t *testing.T) {
			eachEngineMode(t, func(t *testing.T, e *Executor) {
				request := settingsRequest(t, e)
				launchPort := request.Settings.ServerPort
				request.Settings.ServerPort++
				request.Resolution = resolution
				preview := previewSettings(t, e, request)
				require.Len(t, preview.Errors, 0, "unexpected errors")
				if resolution == "" {
					require.NotNil(t, preview.Conflict, "missing port conflict (%v)", preview)
					require.True(t, preview.Conflict.ServerPort == request.Settings.ServerPort, "missing port conflict (%v)", preview)
					require.True(t, preview.Conflict.LaunchPort == launchPort, "missing port conflict (%v)", preview)
					return
				}
				want := request.Settings.ServerPort
				if resolution == "launch" {
					want = launchPort
				}
				require.Nil(t, preview.Conflict, "resolution (%v, %v, %v)", resolution, preview, want)
				require.True(t, preview.Settings.ServerPort == want, "resolution (%v, %v, %v)", resolution, preview, want)
			})
		})
	}
	test("requires explicit resolution", "")
	test("uses server field", "server")
	test("uses launch port", "launch")
}

func TestSettingsPreviewChecksOnlyDeclaredControls(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		for _, suffix := range []string{" | other", " && other"} {
			p := settingsRequest(t, e)
			p.Settings.LaunchText += suffix
			{
				result := previewSettings(t, e, p)
				require.NotEqual(t, "", result.Errors["launchText"], "accepted shell syntax (%v)", suffix)
			}
		}
		policy := settingsState(t, e).plat.Runtime.EditableLaunch
		if policy.Controls[0].Value == "{server.port}" {
			for _, suffix := range []string{" --bind 0.0.0.0", " --port 0", " -- --bind 0.0.0.0"} {
				p := settingsRequest(t, e)
				p.Settings.LaunchText += suffix
				{
					result := previewSettings(t, e, p)
					require.NotEqual(t, "", result.Errors["launchText"], "accepted invalid managed control (%v)", suffix)
				}
			}
		} else {
			p := settingsRequest(t, e)
			p.Settings.LaunchText = "OLLAMA_HOST=0.0.0.0:12345"
			{
				result := previewSettings(t, e, p)
				require.NotEqual(t, "", result.Errors["launchText"], "accepted invalid managed host")
			}
		}
		assertNoSettingsOverride(t, e)
	})
}

func TestSettingsDoesNotGuessNetworkOptionSemantics(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		p := settingsRequest(t, e)
		want := []string{"--bind-address", "0.0.0.0", "--set", "host=0.0.0.0", "--set=port=22", "--set=--bind=0.0.0.0", "--hostname=anything", "--listen=anything"}
		text, err := formatLaunchText(want)
		require.NoError(t, err)
		p.Settings.LaunchText += " " + text
		result := previewSettings(t, e, p)
		require.Len(t, result.Errors, 0, "opaque options changed or rejected (%v)", result)
		require.Equal(t, want, result.Args, "opaque options changed or rejected (%v)", result)
	})
}

func TestSettingsPreviewAcceptsUnrelatedOptions(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		request := settingsRequest(t, e)
		want := []string{"--transport", "grpc", "-v", "--max-tokens", "not-a-number",
			"--future-engine-option", "unknown-value", "-batch", "-ctx-size", "4096",
			"--config", "custom.json", "--config-file=custom.json", "--api-key", "test-value",
			"--token=test-value", "--password=test-value", "--secret=test-value",
			"--set=transport=grpc", "--label=host", "--set=api-key=test-value", "--set=token=test-value"}
		text, err := formatLaunchText(want)
		require.NoError(t, err)
		request.Settings.LaunchText += " " + text
		preview := previewSettings(t, e, request)
		require.Len(t, preview.Errors, 0, "safe options rejected (%v)", preview)
		require.Nil(t, preview.Conflict, "safe options rejected (%v)", preview)
		require.Equal(t, want, preview.Args, "args")
	})
}

func TestSettingsArgumentsDoNotIncludeExecutableOrSubcommand(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		p := settingsRequest(t, e)
		require.NotContains(t, p.Settings.LaunchText, fakeEngineBin, "manifest command leaked into editor")
		require.NotContains(t, p.Settings.LaunchText, "server start", "manifest command leaked into editor")
		require.NotContains(t, p.Settings.LaunchText, "serve", "manifest command leaked into editor")
		p.Settings.LaunchText = "different serve"
		result := previewSettings(t, e, p)
		require.Len(t, result.Errors, 0, "positional arguments rejected (%v)", result)
		st := settingsState(t, e)
		rt := st.plat.Runtime
		rt.LaunchArgs = &result.Args
		rt.LaunchEnv = &result.Env
		launch, err := launchForState(st, p.Settings.ServerPort)
		require.NoError(t, err)
		actual, err := applyLiteralLaunch(rt, launch, map[string]string{"host": "127.0.0.1", "port": fmt.Sprint(p.Settings.ServerPort)})
		require.NoError(t, err, "arguments changed the executable/subcommand (%v, %v)", actual, err)
		require.True(t, actual.Bin == fakeEngineBin, "arguments changed the executable/subcommand (%v, %v)", actual, err)
		assert.Equal(t, rt.EditableLaunch.FixedArgs, actual.Args[:len(rt.EditableLaunch.FixedArgs)], "arguments changed the executable/subcommand")
	})
}

func TestSettingsMigratesFullCommandWithoutChangingArguments(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		p := settingsRequest(t, e)
		launch, err := launchForState(settingsState(t, e), p.Settings.ServerPort)
		require.NoError(t, err)
		p.Settings.LaunchText, err = launch.text()
		require.NoError(t, err)
		p.Settings.LaunchText += ` --future-option "two words"`
		p.Format = "pair-launch-v1"
		result := previewSettings(t, e, p)
		require.Len(t, result.Errors, 0, "migration changed arguments (%v)", result)
		require.Equal(t, []string{"--future-option", "two words"}, result.Args, "migration changed arguments (%v)", result)
		require.NotContains(t, result.Settings.LaunchText, fakeEngineBin, "migration changed arguments (%v)", result)
	})
}

func TestSettingsEmptyArgumentsRestoreManagedDefaults(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		p := settingsRequest(t, e)
		p.Settings.LaunchText = ""
		result := previewSettings(t, e, p)
		require.Len(t, result.Errors, 0, "empty arguments did not restore defaults (%v)", result)
		require.Len(t, result.Args, 0, "empty arguments did not restore defaults (%v)", result)
		require.Len(t, result.Env, 0, "empty arguments did not restore defaults (%v)", result)
		require.True(t, result.Settings.ServerPort == p.Settings.ServerPort, "empty arguments did not restore defaults (%v)", result)
	})
}

func readSettingsOverride(t *testing.T, e *Executor) Runtime {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(e.overrideDir, "fake.json"))
	require.NoError(t, err)
	var override struct {
		Platforms map[string]struct {
			Runtime Runtime `json:"runtime"`
		} `json:"platforms"`
	}
	require.NoError(t, json.Unmarshal(data, &override), "decode override")
	platform, ok := override.Platforms[runtime.GOOS+"/"+runtime.GOARCH]
	require.True(t, ok, "host platform missing from override")
	return platform.Runtime
}

// lifecycle is what applying settings should do to the engine process.
type lifecycle int

const (
	staysStopped lifecycle = iota
	preservesProcess
	restartsProcess
)

func TestSettingsConfigureLifecycle(t *testing.T) {
	test := func(name string, want lifecycle, edit func(*testing.T, *settings.Request)) {
		t.Run(name, func(t *testing.T) {
			e := settingsExecutor(t, false)
			st := settingsState(t, e)
			startLog := filepath.Join(t.TempDir(), "starts.jsonl")
			st.plat.Runtime.Env["FAKE_START_LOG"] = startLog
			if want != staysStopped {
				require.NoError(t, e.Start(context.Background(), "fake"))
			}
			pid := func() int {
				st.mu.Lock()
				defer st.mu.Unlock()
				if st.proc == nil {
					return 0
				}
				return st.proc.cmd.Process.Pid
			}
			before := pid()
			request := settingsRequest(t, e)
			edit(t, &request)
			preview := previewSettings(t, e, request)
			require.Len(t, preview.Errors, 0, "preview (%v)", preview)
			require.Nil(t, preview.Conflict, "preview (%v)", preview)
			rebinds := 0
			result, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: preview.Settings}, func() error { rebinds++; return nil })
			require.NoError(t, err)
			got := staysStopped
			if result.Running {
				got = preservesProcess
			}
			if pid() != before {
				got = restartsProcess
			}
			require.True(t, got == want, "result (%v, %v, %v)", result, before, rebinds)
			require.Equal(t, 1, rebinds, "result (%v, %v, %v)", result, before, rebinds)
			require.True(t, result.ServerPort == preview.Settings.ServerPort, "port")
			override := readSettingsOverride(t, e)
			require.NotNil(t, override.LaunchArgs, "persisted arguments")
			require.Equal(t, preview.Args, *override.LaunchArgs, "persisted arguments")
			if want != staysStopped {
				data, err := os.ReadFile(startLog)
				require.NoError(t, err)
				launches := 1
				if want == restartsProcess {
					launches++
				}
				{
					count := bytes.Count(data, []byte("\n"))
					require.True(t, count == launches, "launch count (%v, %v)", count, launches)
				}
			} else {
				{
					_, err := os.Stat(startLog)
					require.True(t, os.IsNotExist(err), "stopped engine launched (%v)", err)
				}
			}
		})
	}
	test("no-op preserves process", preservesProcess, func(*testing.T, *settings.Request) {})
	test("proxy port preserves process", preservesProcess, func(t *testing.T, request *settings.Request) {
		request.Settings.ProxyPort++
	})
	test("literal arguments restart process", restartsProcess, func(t *testing.T, request *settings.Request) {
		request.Settings.LaunchText += ` --parallel 4 "{port}"`
	})
	test("server port restarts process", restartsProcess, func(t *testing.T, request *settings.Request) {
		port, err := freePort()
		require.NoError(t, err)
		request.Settings.ServerPort = port
		request.Resolution = "server"
	})
	test("stopped engine stays stopped", staysStopped, func(t *testing.T, request *settings.Request) {
		request.Settings.LaunchText += " --another-option"
	})
}

// A no-op Apply still persists an empty launch_env; log capture must survive.

func TestSettingsNoOpApplyKeepsEngineLogCapture(t *testing.T) {
	e := settingsExecutor(t, false)
	st := settingsState(t, e)
	require.NoError(t, e.Start(context.Background(), "fake"))
	require.NotEqual(t, 0, len(st.logs.snapshot()), "no engine output captured before Apply")
	request := settingsRequest(t, e)
	preview, err := e.PreviewLaunch(request)
	require.NoError(t, err, "preview (%v, %v)", preview, err)
	require.LessOrEqual(t, len(preview.Errors), 0, "preview (%v, %v)", preview, err)
	{
		_, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: preview.Settings}, func() error { return nil })
		require.NoError(t, err)
	}
	st.mu.Lock()
	persisted := st.plat.Runtime.LaunchEnv
	st.mu.Unlock()
	require.NotNil(t, persisted, "expected a no-op Apply to leave an empty launch_env")
	require.Len(t, *persisted, 0, "expected a no-op Apply to leave an empty launch_env (%v)", persisted)
	require.NoError(t, e.Stop("fake"))
	before := len(st.logs.snapshot())
	require.NoError(t, e.Start(context.Background(), "fake"))
	require.True(t, len(st.logs.snapshot()) != before, "engine log capture stopped after a no-op Apply persisted an empty launch_env")
}

func TestSettingsEarlyLaunchFailureRetainsDesiredOptions(t *testing.T) {
	e := settingsExecutor(t, false)
	require.NoError(t, e.Start(context.Background(), "fake"))
	p := settingsRequest(t, e)
	p.Settings.LaunchText += " --fail-launch private-value"
	started := time.Now()
	result, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: p.Settings}, func() error { return nil })
	require.Error(t, err, "early failure misreported (%v, %v)", result, err)
	require.False(t, result.Running, "early failure misreported (%v, %v)", result, err)
	require.Contains(t, err.Error(), "exited before readiness", "early failure misreported (%v, %v)", result, err)
	require.LessOrEqual(t, time.Since(started), 5*time.Second, "early exit waited through readiness timeout")
	require.Contains(t, result.LaunchText, "--fail-launch", "failed desired arguments lost")
	require.Contains(t, err.Error(), "invalid launch", "vendor echo escaped into error")
	require.NotContains(t, err.Error(), "private-value", "vendor echo escaped into error")
	{
		errors := e.Errors()
		require.False(t, hasErr(errors, startFailedID("fake")), "settings failure was also reported through the global dialog (%v)", errors)
		require.False(t, hasErr(errors, exitedID("fake")), "settings failure was also reported through the global dialog (%v)", errors)
	}
}

func TestEarlyExitHasOneStartError(t *testing.T) {
	e := settingsExecutor(t, false)
	p := settingsRequest(t, e)
	p.Settings.LaunchText += " --fail-launch"
	{
		_, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: p.Settings}, func() error { return nil })
		require.NoError(t, err)
	}
	require.Error(t, e.Start(context.Background(), "fake"), "expected failed start")
	{
		errors := e.Errors()
		require.Len(t, errors, 1, "expected one start failure")
		require.True(t, hasErr(errors, startFailedID("fake")), "expected one start failure (%v)", errors)
		require.False(t, hasErr(errors, exitedID("fake")), "expected one start failure (%v)", errors)
	}
}

func TestSettingsCORSOriginsValidationAndNormalization(t *testing.T) {
	test := func(name, origins, wantError string) {
		t.Run(name, func(t *testing.T) {
			e := settingsExecutor(t, false)
			request := settingsRequest(t, e)
			request.Settings.LaunchText = `OLLAMA_ORIGINS="` + origins + `" ` + request.Settings.LaunchText
			result := previewSettings(t, e, request)
			if wantError != "" {
				require.Contains(t, result.Errors["launchText"], wantError, "expected (%v, %v)", wantError, result)
			} else {
				require.Len(t, result.Errors, 0, "valid origins rejected (%v)", result)
				env, err := literalEnvironment(result.Env)
				require.NoError(t, err)
				{
					value, ok := env["OLLAMA_ORIGINS"]
					require.True(t, ok, "origins (%v, %v, %v)", value, ok, origins)
					require.True(t, value == origins, "origins (%v, %v, %v)", value, ok, origins)
				}
			}
			assertNoSettingsOverride(t, e)
		})
	}
	test("rejects origin without scheme", "localhost", `"http://localhost"`)
	test("rejects mixed valid and invalid origins", "http://localhost,invalid", `"http://localhost"`)
	test("rejects bare wildcard", "*", "every origin")
	test("rejects wildcard host", "http://*", "every origin")
	test("rejects wildcard scheme and host", "*://*", "every origin")
	test("rejects wildcard in origin list", "http://localhost,*", "every origin")
	test("accepts localhost URL", "http://localhost", "")
	test("accepts explicit origin list", "https://example.test,http://localhost", "")
	test("accepts wildcard subdomain", "https://*.example.test", "")
	test("accepts empty origins", "", "")
}

func TestSettingsCORSIsOptionalAndIndependentOfEngine(t *testing.T) {
	test := func(name, corsEnv, corsFlag, prefix, suffix string, wantError bool) {
		t.Run(name, func(t *testing.T) {
			e := settingsExecutor(t, false)
			rt := &settingsState(t, e).plat.Runtime
			rt.Env = map[string]string{"FUTURE_BIND": "{host}:{port}"}
			rt.EditableLaunch = &EditableLaunch{FixedArgs: []string{"serve"}, Controls: []LaunchControl{{Value: "{server.host}:{server.port}", Env: []string{"FUTURE_BIND"}}}}
			if corsEnv != "" {
				rt.EditableLaunch.Controls = append(rt.EditableLaunch.Controls, LaunchControl{Value: "{cors.origins}", Env: []string{corsEnv}})
			}
			if corsFlag != "" {
				rt.EditableLaunch.Controls = append(rt.EditableLaunch.Controls, LaunchControl{Value: "{cors.origins}", Flags: []string{corsFlag}})
			}

			request := settingsRequest(t, e)
			request.Settings.LaunchText = prefix + request.Settings.LaunchText + suffix
			preview := previewSettings(t, e, request)
			require.True(t, (len(preview.Errors) != 0) == wantError, "preview errors (%v)", wantError)
			if !wantError {
				{
					_, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func() error { return nil })
					require.NoError(t, err)
				}
			}
		})
	}
	test("no CORS option required", "", "", "", " --future-option arbitrary", false)
	test("undeclared CORS configuration stays opaque", "", "", "FUTURE_ORIGINS=* ", " --cors", false)
	test("declared environment accepts origins", "FUTURE_ORIGINS", "", "FUTURE_ORIGINS=https://example.test ", "", false)
	test("declared environment rejects wildcard", "FUTURE_ORIGINS", "", "FUTURE_ORIGINS=* ", "", true)
	test("declared flag accepts origins", "", "--browser-origins", "", " --browser-origins https://example.test", false)
	test("declared flag accepts equals form", "", "--browser-origins", "", " --browser-origins=https://example.test", false)
	test("declared flag rejects wildcard", "", "--browser-origins", "", " --browser-origins=*", true)
	test("declared flag rejects missing value", "", "--browser-origins", "", " --browser-origins", true)
}

func TestSettingsPassesUnknownEnvironmentNamesAndValues(t *testing.T) {
	eachEngineMode(t, func(t *testing.T, e *Executor) {
		request := settingsRequest(t, e)
		want := []string{"FUTURE_ENGINE_SETTING=unknown-value", "OLLAMA_CONTEXT_LENGTH=not-a-number",
			"OLLAMA_HOSTNAME=example.test", "PATH=/custom", "LD_LIBRARY_PATH=/custom/lib"}
		text, err := formatLaunchText(want)
		require.NoError(t, err)
		request.Settings.LaunchText = text + " " + request.Settings.LaunchText
		preview := previewSettings(t, e, request)
		require.Len(t, preview.Errors, 0, "opaque environment rejected (%v)", preview)
		require.Nil(t, preview.Conflict, "opaque environment rejected (%v)", preview)
		require.Equal(t, want, preview.Env, "environment")
		assertNoSettingsOverride(t, e)
	})
}

func manifestSettingsRequest(t *testing.T, platform string) (*Executor, settings.Request, []string) {
	t.Helper()
	reg := loadWithOverrides(t, t.TempDir())
	manifest, ok := reg.Get("ollama")
	require.True(t, ok, "Ollama manifest missing")
	config, ok := manifest.Platforms[platform]
	require.True(t, ok, "platform (%v)", platform)
	e := settingsExecutor(t, false)
	graftPlatform(t, e, config)
	request := settingsRequest(t, e)
	tokens, err := parseLaunchText(request.Settings.LaunchText)
	require.NoError(t, err)
	require.NotEqual(t, 0, len(tokens), "missing manifest library path (%v)", tokens)
	require.True(t, strings.HasPrefix(tokens[0], "LD_LIBRARY_PATH="), "missing manifest library path (%v)", tokens)
	return e, request, tokens
}

func TestSettingsManifestEnvironmentCanBeOverridden(t *testing.T) {
	test := func(name, platform, replacement string) {
		t.Run(name, func(t *testing.T) {
			e, request, tokens := manifestSettingsRequest(t, platform)
			tokens[0] = "LD_LIBRARY_PATH=" + replacement
			var err error
			request.Settings.LaunchText, err = formatLaunchText(tokens)
			require.NoError(t, err)
			{
				preview := previewSettings(t, e, request)
				require.Len(t, preview.Errors, 0, "rejected replacement library path (%v, %v)", replacement, preview)
			}
			{
				_, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func() error { return nil })
				require.NoError(t, err)
			}
			override := readSettingsOverride(t, e)
			env, err := literalEnvironment(*override.LaunchEnv)
			require.NoError(t, err, "saved environment (%v, %v)", env, err)
			require.True(t, env["LD_LIBRARY_PATH"] == replacement, "saved environment (%v, %v)", env, err)
		})
	}
	test("amd64 custom directory", "linux/amd64", "/custom/libraries")
	test("amd64 empty directory", "linux/amd64", "")
	test("amd64 literal template", "linux/amd64", "{install_dir}/other")
	test("arm64 custom directory", "linux/arm64", "/custom/libraries")
	test("arm64 empty directory", "linux/arm64", "")
	test("arm64 literal template", "linux/arm64", "{install_dir}/other")
}

func TestSettingsSafeEditPreservesManifestEnvironment(t *testing.T) {
	test := func(name, platform string, omitFixed bool) {
		t.Run(name, func(t *testing.T) {
			e, request, tokens := manifestSettingsRequest(t, platform)
			if omitFixed {
				var err error
				request.Settings.LaunchText, err = formatLaunchText(tokens[1:])
				require.NoError(t, err)
			}
			request.Settings.LaunchText = "OLLAMA_NUM_PARALLEL=3 " + request.Settings.LaunchText
			preview := previewSettings(t, e, request)
			require.Len(t, preview.Errors, 0, "safe edit rejected (%v)", preview)
			actual, err := parseLaunchText(preview.Settings.LaunchText)
			require.NoError(t, err)
			require.NotEqual(t, 0, len(actual), "safe edit lost fixed library path (%v)", actual)
			require.True(t, actual[0] == tokens[0], "safe edit lost fixed library path (%v)", actual)
			{
				_, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func() error { return nil })
				require.NoError(t, err)
			}
			state := settingsState(t, e)
			state.installDir = "/updated-install"
			launch, err := launchForState(state, request.Settings.ServerPort)
			require.NoError(t, err, "default environment stopped following the manifest")
			require.Equal(t, "/updated-install/lib/ollama", launch.Env["LD_LIBRARY_PATH"], "default environment stopped following the manifest (%v)", err)
		})
	}
	test("amd64 retains fixed assignment", "linux/amd64", false)
	test("amd64 restores omitted assignment", "linux/amd64", true)
	test("arm64 retains fixed assignment", "linux/arm64", false)
	test("arm64 restores omitted assignment", "linux/arm64", true)
}

func TestSettingsBudgetCoversBundledReadiness(t *testing.T) {
	reg := loadWithOverrides(t, t.TempDir())
	for _, name := range reg.Names() {
		manifest, ok := reg.Get(name)
		require.True(t, ok, "manifest (%v)", name)
		for platform, config := range manifest.Platforms {
			rt := config.Runtime
			if rt.EditableLaunch == nil || rt.Ready == nil {
				continue
			}
			required := time.Duration(rt.Ready.TimeoutS) * time.Second
			if rt.Stop != nil {
				required += time.Duration(rt.Stop.GraceS) * time.Second
			}
			assert.Greater(t, settings.ConfigureBudget, required, " (%v, %v, %v)", name, platform, required)
		}
	}
	require.Greater(t, settings.OperationBudget, settings.ConfigureBudget, "settings callers must leave headroom above the operation they await")
	require.Greater(t, settings.CallBudget, settings.OperationBudget, "settings callers must leave headroom above the operation they await")
	require.Greater(t, settings.RelayBudget, settings.CallBudget, "settings callers must leave headroom above the operation they await")
}

func TestSavedLaunchOverridesManifestEnvironmentLiterally(t *testing.T) {
	rt := Runtime{
		EditableLaunch: &EditableLaunch{FixedArgs: []string{"serve"}},
		Env:            map[string]string{"LD_LIBRARY_PATH": "{install_dir}/lib"},
	}
	args := []string{}
	rt.LaunchArgs = &args
	for _, value := range []string{"/custom", "", "{install_dir}/other"} {
		env := []string{"LD_LIBRARY_PATH=" + value}
		rt.LaunchEnv = &env
		launch, err := resolveProcessLaunch(rt, "engine", map[string]string{"install_dir": "/default", "host": "127.0.0.1", "port": "12345"})
		assert.NoError(t, err, "saved launch environment (%v, %v)", err, value)
		assert.True(t, launch.Env["LD_LIBRARY_PATH"] == value, "saved launch environment (%v, %v)", err, value)
	}
}

func TestSettingsSwapsLiveServerAndProxyPorts(t *testing.T) {
	e := settingsExecutor(t, false)
	require.NoError(t, e.Start(context.Background(), "fake"))
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = proxy.Close() })
	p := settingsRequest(t, e)
	oldServer := p.Settings.ServerPort
	p.Settings.ServerPort = proxy.Addr().(*net.TCPAddr).Port
	p.Settings.ProxyPort = oldServer
	p.Resolution = "server"
	preview, err := e.PreviewLaunch(p)
	require.NoError(t, err, "swap preview (%v, %v)", preview, err)
	require.Len(t, preview.Errors, 0, "swap preview (%v, %v)", preview, err)
	require.Nil(t, preview.Conflict, "swap preview (%v, %v)", preview, err)
	rebinds := 0
	result, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: preview.Settings}, func() error {
		rebinds++
		// The old engine must release its port before the proxy moves, and
		// the new engine must wait until the old proxy listener is closed.
		next, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", oldServer))
		if err != nil {
			return err
		}
		_ = proxy.Close()
		proxy = next
		return nil
	})
	require.NoError(t, err, "live swap (%v, %v, %v)", result, rebinds, err)
	require.True(t, result.Running, "live swap (%v, %v, %v)", result, rebinds, err)
	require.True(t, result.EffectivePort == p.Settings.ServerPort, "live swap (%v, %v, %v)", result, rebinds, err)
	require.Equal(t, 1, rebinds, "live swap (%v, %v, %v)", result, rebinds, err)
}

func TestSettingsCommandLaunchPreservesLiteralsAndCleansFailedStart(t *testing.T) {
	e := settingsExecutor(t, true)
	st := settingsState(t, e)
	capture := filepath.Join(t.TempDir(), "argv.json")
	stopped := filepath.Join(t.TempDir(), "stopped")
	rt := &st.plat.Runtime
	rt.Start = [][]string{{fakeEngineBin, "captureargs", capture, "--port", "{port}", "--bind", "{host}"}}
	rt.EditableLaunch.FixedArgs = []string{"captureargs", capture}
	rt.Ready = nil
	rt.Health = nil
	rt.Stop = &StopSpec{Cmd: []string{fakeEngineBin, "touch", stopped}}
	p := settingsRequest(t, e)
	p.Settings.LaunchText += ` "{port}" "$HOME" "two words" ""`
	{
		_, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: p.Settings}, func() error { return nil })
		require.NoError(t, err)
	}
	require.NoError(t, e.Start(context.Background(), "fake"))
	data, err := os.ReadFile(capture)
	var args []string
	require.NoError(t, err, "command mode expanded literal options (%v, %v)", data, err)
	require.Nil(t, json.Unmarshal(data, &args), "command mode expanded literal options (%v, %v)", data, err)
	require.GreaterOrEqual(t, len(args), 4, "command mode expanded literal options (%v, %v)", data, err)
	require.Equal(t, []string{"{port}", "$HOME", "two words", ""}, args[len(args)-4:], "command mode expanded literal options (%v, %v)", data, err)
	require.NoError(t, e.Stop("fake"))
	require.NoError(t, os.Remove(stopped))
	p = settingsRequest(t, e)
	p.Settings.LaunchText += " --fail-launch private-value"
	{
		_, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: p.Settings}, func() error { return nil })
		require.NoError(t, err)
	}
	err = e.Start(context.Background(), "fake")
	require.Error(t, err, "failed command output was not classified safely")
	require.Contains(t, err.Error(), "invalid launch", "failed command output was not classified safely (%v)", err)
	require.NotContains(t, err.Error(), "private-value", "failed command output was not classified safely (%v)", err)
	{
		_, err := os.Stat(stopped)
		require.NoError(t, err, "failed first command did not run official cleanup")
	}
}

func TestSettingsEnvironmentReachesEngineAndCanBeEditedAndRemoved(t *testing.T) {
	for _, command := range []bool{false, true} {
		t.Run(fmt.Sprint(command), func(t *testing.T) {
			e := settingsExecutor(t, command)
			st := settingsState(t, e)
			if command {
				capture := filepath.Join(t.TempDir(), "args.json")
				rt := &st.plat.Runtime
				rt.Start = [][]string{{fakeEngineBin, "captureargs", capture, "--port", "{port}", "--bind", "{host}"}}
				rt.EditableLaunch.FixedArgs = []string{"captureargs", capture}
				rt.Ready, rt.Health = nil, nil
				rt.Stop = &StopSpec{Cmd: []string{fakeEngineBin, "touch", filepath.Join(t.TempDir(), "stopped")}}
			}
			captureEnv := filepath.Join(t.TempDir(), "env.json")
			prefix, err := formatLaunchText([]string{"PAIR_TEST_ENV_FILE=" + captureEnv, "OLLAMA_ORIGINS=http://localhost", `PAIR_TEST_LITERAL=$HOME {port} C:\new\tools`, "PAIR_TEST_EMPTY="})
			require.NoError(t, err)
			p := settingsRequest(t, e)
			p.Settings.LaunchText = prefix + " " + p.Settings.LaunchText
			configure := func() settings.LaunchState {
				result, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: p.Settings}, func() error { return nil })
				require.NoError(t, err)
				return result
			}
			require.False(t, configure().Running, "environment edit started stopped engine")
			require.NoError(t, e.Start(context.Background(), "fake"))
			readEnv := func() map[string]string {
				data, err := os.ReadFile(captureEnv)
				require.NoError(t, err)
				var env map[string]string
				require.NoError(t, json.Unmarshal(data, &env))
				return env
			}
			{
				env := readEnv()
				require.Equal(t, "http://localhost", env["OLLAMA_ORIGINS"], "environment not passed literally (%v)", env)
				require.Equal(t, `$HOME {port} C:\new\tools`, env["PAIR_TEST_LITERAL"], "environment not passed literally (%v)", env)
				require.Equal(t, "", env["PAIR_TEST_EMPTY"], "environment not passed literally (%v)", env)
			}
			p = settingsRequest(t, e)
			p.Settings.LaunchText = strings.Replace(p.Settings.LaunchText, `OLLAMA_ORIGINS="http://localhost"`, `OLLAMA_ORIGINS="http://example.test"`, 1)
			require.True(t, configure().Running, "environment-only edit did not restart with new value")
			require.Equal(t, "http://example.test", readEnv()["OLLAMA_ORIGINS"], "environment-only edit did not restart with new value")
			p = settingsRequest(t, e)
			p.Settings.LaunchText = strings.Replace(p.Settings.LaunchText, `OLLAMA_ORIGINS="http://example.test" `, "", 1)
			configure()
			require.Equal(t, "", readEnv()["OLLAMA_ORIGINS"], "removed environment variable survived")
			override := readSettingsOverride(t, e)
			require.NotNil(t, override.LaunchEnv, "launch environment missing from override")
			env, err := literalEnvironment(*override.LaunchEnv)
			require.NoError(t, err)
			{
				_, exists := env["OLLAMA_ORIGINS"]
				require.False(t, exists, "removed environment variable remains in override")
			}
			require.True(t, env["PAIR_TEST_ENV_FILE"] == captureEnv, "unrelated environment values lost (%v)", env)
			require.Equal(t, `$HOME {port} C:\new\tools`, env["PAIR_TEST_LITERAL"], "unrelated environment values lost (%v)", env)
		})
	}
}

func TestSettingsEnvironmentValidation(t *testing.T) {
	e := settingsExecutor(t, false)
	for _, prefix := range []string{"9BAD=value", "BAD-NAME=value", "PAIR_TEST=x PAIR_TEST=y"} {
		p := settingsRequest(t, e)
		p.Settings.LaunchText = prefix + " " + p.Settings.LaunchText
		result, err := e.PreviewLaunch(p)
		require.NoError(t, err, "invalid assignment accepted (%v, %v)", result, err)
		require.NotEqual(t, 0, len(result.Errors), "invalid assignment accepted (%v, %v)", result, err)
	}
}

// A stop that fails has to leave the saved configuration alone. Persisting
// first would put the new launch on disk while the live process kept the old
// one, and the next start would adopt settings the user was told had failed.
func TestSettingsFailedStopLeavesSavedConfigurationIntact(t *testing.T) {
	e := settingsExecutor(t, true)
	st := settingsState(t, e)
	capture := filepath.Join(t.TempDir(), "argv.json")
	rt := &st.plat.Runtime
	rt.Start = [][]string{{fakeEngineBin, "captureargs", capture, "--port", "{port}", "--bind", "{host}"}}
	rt.EditableLaunch.FixedArgs = []string{"captureargs", capture}
	rt.Ready, rt.Health = nil, nil
	rt.Stop = &StopSpec{Cmd: []string{fakeEngineBin, "failmark", filepath.Join(t.TempDir(), "stop-attempts")}}
	require.NoError(t, e.Start(context.Background(), "fake"))
	override := filepath.Join(e.overrideDir, "fake.json")
	before, err := os.ReadFile(override)
	require.False(t, err != nil && !os.IsNotExist(err))
	request := settingsRequest(t, e)
	request.Settings.LaunchText += " --parallel 7"
	result, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func() error {
		assert.Fail(t, "test expectation failed", "rebound an engine that could not be stopped")
		return nil
	})
	require.Error(t, err, "failed stop reported success (%v)", result)
	after, readErr := os.ReadFile(override)
	require.False(t, readErr != nil && !os.IsNotExist(readErr))
	assert.Equal(t, before, after, "stop failure still rewrote saved configuration")
	live := e.launchStateLocked("fake", st)
	require.NotContains(t, live.LaunchText, "--parallel 7", "in-memory launch adopted the rejected change")
}

func TestSettingsRejectBeforeMutationAndRetainAcceptedFailure(t *testing.T) {
	e := settingsExecutor(t, false)
	request := settingsRequest(t, e)
	st := settingsState(t, e)
	st.mu.Lock()
	st.running = true
	st.adopted = true
	st.mu.Unlock()
	request.Settings.LaunchText += " --new"
	{
		_, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func() error { assert.Fail(t, "rebound adopted engine"); return nil })
		require.Error(t, err, "adopted process accepted")
	}
	assertNoSettingsOverride(t, e)
	st.mu.Lock()
	st.running = false
	st.adopted = false
	st.mu.Unlock()
	result, err := e.ConfigureLaunch(context.Background(), settings.Configure{Engine: "fake", Settings: request.Settings}, func() error { return fmt.Errorf("bind failed") })
	require.Error(t, err, "accepted desired config not retained (%v, %v)", result, err)
	require.False(t, result.Running, "accepted desired config not retained (%v, %v)", result, err)
	require.Contains(t, result.LaunchText, "--new", "accepted desired config not retained (%v, %v)", result, err)
}

func TestSettingsSupportsEngineWithoutStartupSubcommand(t *testing.T) {
	e := settingsExecutor(t, false)
	st := settingsState(t, e)
	st.plat.Runtime.Args = nil
	st.plat.Runtime.EditableLaunch.FixedArgs = nil
	require.NoError(t, st.plat.validate(runtime.GOOS+"/"+runtime.GOARCH))
	p := settingsRequest(t, e)
	p.Settings.LaunchText += " --future-option=opaque"
	preview := previewSettings(t, e, p)
	require.Len(t, preview.Errors, 0, "engine without subcommand rejected (%v)", preview)
	require.Equal(t, []string{"--future-option=opaque"}, preview.Args, "engine without subcommand rejected (%v)", preview)
}
