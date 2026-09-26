// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// pathInstallExecutor builds an engine whose detect path and declared CLI are
// deliberately different files inside one managed install directory. Detect is a
// discovery hint that matches whatever layout a vendor ships; only runtime.cli
// names the directory PAIR is entitled to publish on PATH.
func pathInstallExecutor(t *testing.T, engine, mode string) (*Executor, string) {
	t.Helper()
	installDir := filepath.Join(t.TempDir(), "Engine Tools")
	detected := filepath.Join(installDir, engine+exeExt())
	cli := filepath.Join(installDir, "bin", engine+exeExt())
	if err := os.MkdirAll(filepath.Dir(cli), 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manifest{
		Engine: engine, DisplayName: engine, ManifestVersion: 1,
		Platforms: map[string]Platform{runtime.GOOS + "/" + runtime.GOARCH: {
			Detect:  []string{detected},
			Install: &Install{Run: []string{fakeEngineBin, "touch", detected, cli}},
			Runtime: Runtime{Mode: mode, Bin: detected, CLI: cli},
		}},
	}
	ex := newTestExecutor(t, m)
	engineStateForTest(t, ex, engine).installDir = installDir
	return ex, cli
}

func engineStateForTest(t *testing.T, ex *Executor, engine string) *engineState {
	t.Helper()
	st, err := ex.state(engine)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// pathWarning returns the reported PATH warning for an engine, or "".
func pathWarning(ex *Executor, engine string) string {
	for _, e := range ex.reporter.snapshot() {
		if e.ID == pathFailedID(engine) {
			return e.Message
		}
	}
	return ""
}

func TestInstallAutomaticallyAddsCLIToPath(t *testing.T) {
	test := func(name, engine, mode string) {
		t.Run(name, func(t *testing.T) {
			ex, cli := pathInstallExecutor(t, engine, mode)
			var added []string
			ex.addToPath = func(dir string, _ *pathReceipt, _ func() error) error {
				if !fileExists(cli) {
					t.Error("PATH changed before CLI was installed")
				}
				added = append(added, dir)
				return nil
			}
			if err := ex.Install(context.Background(), engine); err != nil {
				t.Fatal(err)
			}
			if !fileExists(cli) {
				t.Fatal("engine was not installed")
			}
			if len(added) != 1 || added[0] != filepath.Dir(cli) {
				t.Fatalf("added directories = %v", added)
			}
		})
	}
	test("Ollama CLI directory", "ollama", "process")
	test("LM Studio CLI directory", "lmstudio", "command")
}

// A PATH failure must not fail the install. Failing it would also skip the
// caller's start step, so a working engine would sit stopped behind an error.
func TestPathFailureStillCompletesInstallAndWarns(t *testing.T) {
	ex, cli := pathInstallExecutor(t, "ollama", "process")
	var announced EngineStatus
	ex.emit = func(method string, params any) {
		if method != "engine:state-changed" {
			return
		}
		data, err := json.Marshal(params)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &announced); err != nil {
			t.Fatal(err)
		}
	}
	ex.addToPath = func(string, *pathReceipt, func() error) error {
		return errors.New("profile is read only")
	}
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatalf("install error = %v, want success with a PATH warning", err)
	}
	installed, err := ex.Detect("ollama")
	if err != nil || !installed {
		t.Fatalf("installed = %v, error = %v", installed, err)
	}
	if !announced.Installed {
		t.Fatal("UI was not told the engine remains installed")
	}
	warning := pathWarning(ex, "ollama")
	if !strings.Contains(warning, "profile is read only") || !strings.Contains(warning, "full path") {
		t.Fatalf("PATH warning = %q, want the reason and the manual alternative", warning)
	}

	var added string
	ex.addToPath = func(dir string, _ *pathReceipt, _ func() error) error { added = dir; return nil }
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	if added != filepath.Dir(cli) {
		t.Fatalf("retry added %q", added)
	}
	if warning := pathWarning(ex, "ollama"); warning != "" {
		t.Fatalf("retry left the warning in place: %q", warning)
	}
}

// The warning crosses the wire to a remote initiator and is push-synced to
// cluster peers, so it must not carry the user's home directory.
func TestPathWarningOmitsTheUsersHomeDirectory(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory to redact")
	}
	ex, _ := pathInstallExecutor(t, "ollama", "process")
	ex.addToPath = func(string, *pathReceipt, func() error) error {
		return errors.New("open " + filepath.Join(home, ".zshrc") + ": permission denied")
	}
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	warning := pathWarning(ex, "ollama")
	if strings.Contains(warning, home) {
		t.Fatalf("PATH warning leaked the home directory: %q", warning)
	}
	if !strings.Contains(warning, ".zshrc") {
		t.Fatalf("PATH warning dropped the profile name: %q", warning)
	}
}

// A detect entry can match a vendor layout PAIR does not own, so it is never
// enough on its own to claim a PATH directory. PAIR having just installed the
// engine is what separates this from an ordinary pre-existing installation.
func TestPathRefusesAnUndeclaredCLIOutsideTheInstallDirectory(t *testing.T) {
	ex, cli := pathInstallExecutor(t, "ollama", "process")
	st := engineStateForTest(t, ex, "ollama")
	st.plat.Runtime.CLI = ""
	st.installDir = t.TempDir()
	ex.addToPath = func(string, *pathReceipt, func() error) error {
		t.Error("claimed a PATH directory PAIR does not own")
		return nil
	}
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	if !fileExists(cli) {
		t.Fatal("engine was not installed")
	}
	if warning := pathWarning(ex, "ollama"); !strings.Contains(warning, "outside the directory PAIR installs into") {
		t.Fatalf("PATH warning = %q", warning)
	}
}

// A user who already had the engine before PAIR gets no PATH entry and no
// warning: the vendor's installer owns that location and its PATH.
func TestPreexistingEngineProducesNoPathWarning(t *testing.T) {
	ex, cli := pathInstallExecutor(t, "ollama", "process")
	st := engineStateForTest(t, ex, "ollama")
	st.plat.Runtime.CLI = ""
	st.installDir = t.TempDir()
	for _, path := range append([]string{cli}, st.plat.Detect...) {
		if err := os.WriteFile(path, []byte("vendor install"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ex.addToPath = func(string, *pathReceipt, func() error) error {
		t.Error("claimed a PATH directory PAIR does not own")
		return nil
	}
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	if warning := pathWarning(ex, "ollama"); warning != "" {
		t.Fatalf("warned about a PATH entry PAIR was never going to add: %q", warning)
	}
}

// A relative path is meaningless as a PATH entry: it would resolve against the
// daemon's working directory, which is not where the engine lives.
func TestPathRejectsARelativeManifestCLI(t *testing.T) {
	ex, cli := pathInstallExecutor(t, "ollama", "process")
	st := engineStateForTest(t, ex, "ollama")
	t.Chdir(filepath.Dir(cli))
	st.plat.Runtime.CLI = filepath.Base(cli)
	ex.addToPath = func(string, *pathReceipt, func() error) error {
		t.Error("published a PATH entry from a relative manifest path")
		return nil
	}
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	if warning := pathWarning(ex, "ollama"); !strings.Contains(warning, "relative path") {
		t.Fatalf("PATH warning = %q", warning)
	}
}

// The runner applies whatever a manifest declares, so a third-party engine can
// suppress its installer's own PATH edits without a code change.
func TestInstallAppliesManifestDeclaredInstallerEnvironment(t *testing.T) {
	t.Setenv("PAIR_TEST_INSTALL_ENV", "inherited")
	ex, cli := pathInstallExecutor(t, "ollama", "process")
	st := engineStateForTest(t, ex, "ollama")
	record := filepath.Join(filepath.Dir(cli), "installer-env")
	st.plat.Install.Env = map[string]string{"PAIR_TEST_INSTALL_ENV": "declared"}
	st.plat.Install.Run = []string{fakeEngineBin, "write-env", "PAIR_TEST_INSTALL_ENV", record}
	st.plat.Detect = []string{record}
	if err := ex.Install(context.Background(), "ollama"); err != nil {
		t.Fatal(err)
	}
	value, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != "declared" {
		t.Fatalf("installer saw %q, want the manifest override", value)
	}
	if os.Getenv("PAIR_TEST_INSTALL_ENV") != "inherited" {
		t.Fatal("the override escaped into the manager's own environment")
	}
}

// The bundled LM Studio manifest — not a branch in the runner — is what keeps
// the vendor installer from writing PATH entries PAIR cannot clean up.
func TestBundledLMStudioManifestSuppressesVendorPathEdits(t *testing.T) {
	reg := NewRegistry()
	if err := reg.LoadFS(bundledManifests, "manifests"); err != nil {
		t.Fatal(err)
	}
	m, ok := reg.Get("lmstudio")
	if !ok {
		t.Fatal("lmstudio manifest is not bundled")
	}
	for key, plat := range m.Platforms {
		if plat.Install == nil {
			continue
		}
		if plat.Install.Env["LMS_NO_MODIFY_PATH"] != "1" {
			t.Errorf("%s install env = %v", key, plat.Install.Env)
		}
	}
}

func TestFailedInstallDoesNotChangePath(t *testing.T) {
	ex, _ := pathInstallExecutor(t, "ollama", "process")
	st, err := ex.state("ollama")
	if err != nil {
		t.Fatal(err)
	}
	st.plat.Install.Run = []string{filepath.Join(t.TempDir(), "missing-installer")}
	ex.addToPath = func(string, *pathReceipt, func() error) error {
		t.Error("PATH changed after failed install")
		return nil
	}
	if err := ex.Install(context.Background(), "ollama"); err == nil {
		t.Fatal("expected install failure")
	}
}

func TestInstallRPCAutomaticallyAddsCLIToPath(t *testing.T) {
	ex, cli := pathInstallExecutor(t, "ollama", "process")
	var added string
	ex.addToPath = func(dir string, _ *pathReceipt, _ func() error) error { added = dir; return nil }
	var out bytes.Buffer
	m := NewManager(NewCodec(&out), ex, nil)
	id := json.RawMessage("1")
	m.runOp(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: "engine:install",
		Params: json.RawMessage(`{"engine":"ollama"}`)})
	var response struct {
		Result EngineStatus     `json:"result"`
		Error  *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error != nil {
		t.Fatalf("install returned an error: %s", *response.Error)
	}
	if !response.Result.Installed {
		t.Error("install reported the engine as not installed")
	}
	if added != filepath.Dir(cli) {
		t.Errorf("published %q on PATH, want the CLI directory %q", added, filepath.Dir(cli))
	}
}

// A pinned peer may install an engine here, but rewriting this user's login
// shell configuration is a different kind of change and stays local-only.
func TestRemoteInstallDoesNotChangeTheTargetUsersPath(t *testing.T) {
	ex, cli := pathInstallExecutor(t, "lmstudio", "command")
	ex.addToPath = func(string, *pathReceipt, func() error) error {
		t.Error("a remote install changed the local user's PATH")
		return nil
	}
	s := &controlServer{exec: ex}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, controlInstallPath, strings.NewReader(`{"opId":"path-test","engine":"lmstudio"}`))
	s.handleInstall(rec, req)
	frames := decodeFrames(t, rec.Body.String())
	if len(frames) == 0 {
		t.Fatal("missing install result")
	}
	last := frames[len(frames)-1]
	if rec.Code != http.StatusOK {
		t.Errorf("HTTP status = %d, want %d", rec.Code, http.StatusOK)
	}
	if last.Type != "result" {
		t.Fatalf("terminal frame type = %q, want %q: %+v", last.Type, "result", last)
	}
	if last.Status == nil {
		t.Fatal("the terminal result frame carried no status")
	}
	if !last.Status.Installed {
		t.Error("the remote install reported the engine as not installed")
	}
	if !fileExists(cli) {
		t.Fatal("remote install did not install the engine")
	}
	if _, err := os.Stat(ex.pathReceiptFile("lmstudio")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remote install claimed PATH ownership: %v", err)
	}
}
