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
	"sync"
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

func TestConsentedInstallAddsCLIToPath(t *testing.T) {
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
			if err := ex.Install(context.Background(), engine, true); err != nil {
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
	if err := ex.Install(context.Background(), "ollama", true); err != nil {
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
	if err := ex.Install(context.Background(), "ollama", true); err != nil {
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
	if err := ex.Install(context.Background(), "ollama", true); err != nil {
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
	if err := ex.Install(context.Background(), "ollama", true); err != nil {
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
	if err := ex.Install(context.Background(), "ollama", true); err != nil {
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
	if err := ex.Install(context.Background(), "ollama", true); err != nil {
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
	if err := ex.Install(context.Background(), "ollama", true); err != nil {
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
	if err := ex.Install(context.Background(), "ollama", true); err == nil {
		t.Fatal("expected install failure")
	}
}

// runOpForTest sends one lifecycle request through the JSON-RPC front end and
// returns the status it responded with.
func runOpForTest(t *testing.T, ex *Executor, method, params string) EngineStatus {
	t.Helper()
	var out bytes.Buffer
	m := NewManager(NewCodec(&out), ex, nil)
	id := json.RawMessage("1")
	m.runOp(context.Background(), &Message{JSONRPC: "2.0", ID: &id, Method: method, Params: json.RawMessage(params)})
	var response struct {
		Result EngineStatus     `json:"result"`
		Error  *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error != nil {
		t.Fatalf("%s returned an error: %s", method, *response.Error)
	}
	return response.Result
}

func TestInstallRPCAddsCLIToPathOnlyWithConsent(t *testing.T) {
	t.Run("path true publishes the CLI directory", func(t *testing.T) {
		ex, cli := pathInstallExecutor(t, "ollama", "process")
		var added string
		ex.addToPath = func(dir string, receipt *pathReceipt, save func() error) error {
			added = dir
			receipt.ShellBlocks = []pathBlock{{Profile: "profile", Text: dir}}
			return save()
		}
		status := runOpForTest(t, ex, "engine:install", `{"engine":"ollama","path":true}`)
		if !status.Installed {
			t.Error("install reported the engine as not installed")
		}
		if added != filepath.Dir(cli) {
			t.Errorf("published %q on PATH, want the CLI directory %q", added, filepath.Dir(cli))
		}
		if !status.PathManaged {
			t.Error("status does not report the PATH entry PAIR now owns")
		}
	})
	t.Run("omitted path leaves PATH alone", func(t *testing.T) {
		ex, cli := pathInstallExecutor(t, "ollama", "process")
		ex.addToPath = func(string, *pathReceipt, func() error) error {
			t.Error("changed PATH without the user's consent")
			return nil
		}
		status := runOpForTest(t, ex, "engine:install", `{"engine":"ollama"}`)
		if !status.Installed || !fileExists(cli) {
			t.Fatal("a declined PATH entry must still install the engine")
		}
		if status.PathManaged {
			t.Error("status claims a PATH entry that was never published")
		}
	})
}

// Declining the PATH entry still records that PAIR installed the engine, so a
// later consented install of the engine already on disk can publish it even
// when the vendor owns the engine's location.
func TestDeclinedInstallRecordsOwnershipForALaterConsent(t *testing.T) {
	ex, cli := pathInstallExecutor(t, "lmstudio", "command")
	st := engineStateForTest(t, ex, "lmstudio")
	st.installDir = t.TempDir()
	if err := ex.Install(context.Background(), "lmstudio", false); err != nil {
		t.Fatal(err)
	}
	receipt, err := loadPathReceipt(ex.pathReceiptFile("lmstudio"))
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Installed || receipt.Dir != "" {
		t.Fatalf("receipt after a declined install = %+v, want only the installed flag", receipt)
	}

	var added string
	ex.addToPath = func(dir string, _ *pathReceipt, _ func() error) error { added = dir; return nil }
	if err := ex.Install(context.Background(), "lmstudio", true); err != nil {
		t.Fatal(err)
	}
	if added != filepath.Dir(cli) {
		t.Fatalf("consented reinstall published %q, want %q", added, filepath.Dir(cli))
	}
}

// Installing over an engine already on disk without consent records nothing:
// finding the engine there is no evidence that PAIR put it there.
func TestDeclinedInstallOfAPresentEngineRecordsNothing(t *testing.T) {
	ex, _ := pathInstallExecutor(t, "ollama", "process")
	st := engineStateForTest(t, ex, "ollama")
	for _, path := range st.plat.Detect {
		if err := os.WriteFile(path, []byte("vendor install"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := ex.Install(context.Background(), "ollama", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ex.pathReceiptFile("ollama")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a declined install over a present engine wrote a record: %v", err)
	}
}

func TestUninstallRPCRemovesPathOnlyWithConsent(t *testing.T) {
	install := func(t *testing.T) (*Executor, string, string) {
		ex, cli, profile := pathLifecycleExecutor(t, "ollama", "process")
		if err := ex.Install(context.Background(), "ollama", true); err != nil {
			t.Fatal(err)
		}
		return ex, filepath.Dir(cli), profile
	}
	profileHas := func(t *testing.T, profile, dir string) bool {
		t.Helper()
		data, err := os.ReadFile(profile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return strings.Contains(string(data), dir)
	}
	t.Run("path true removes the entry", func(t *testing.T) {
		ex, dir, profile := install(t)
		status := runOpForTest(t, ex, "engine:uninstall", `{"engine":"ollama","path":true}`)
		if profileHas(t, profile, dir) {
			t.Fatal("the consented uninstall left the PATH entry in the profile")
		}
		if status.PathManaged {
			t.Error("status still claims the removed PATH entry")
		}
	})
	t.Run("path false keeps the entry and gives up the claim", func(t *testing.T) {
		ex, dir, profile := install(t)
		status := runOpForTest(t, ex, "engine:uninstall", `{"engine":"ollama","path":false}`)
		if !profileHas(t, profile, dir) {
			t.Fatal("removed the PATH entry although the user kept it")
		}
		if _, err := os.Stat(ex.pathReceiptFile("ollama")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the record claiming the kept entry survived: %v", err)
		}
		if status.PathManaged {
			t.Error("status still claims an entry that now belongs to the user")
		}
	})
	// A client that never asked the user must not hand the entries over: that
	// would leave them with no record the application uninstaller can act on.
	t.Run("omitted path keeps the entry and the claim", func(t *testing.T) {
		ex, dir, profile := install(t)
		status := runOpForTest(t, ex, "engine:uninstall", `{"engine":"ollama"}`)
		if !profileHas(t, profile, dir) {
			t.Fatal("removed the PATH entry without the user's consent")
		}
		receipt, err := loadPathReceipt(ex.pathReceiptFile("ollama"))
		if err != nil {
			t.Fatal(err)
		}
		if !receipt.ownsEntries() || receipt.Installed {
			t.Fatalf("receipt after an unasked uninstall = %+v, want the entries without the installed flag", receipt)
		}
		if !status.PathManaged {
			t.Error("status dropped the claim PAIR still holds")
		}
		runOpForTest(t, ex, "engine:uninstall", `{"engine":"ollama","path":true}`)
		if profileHas(t, profile, dir) {
			t.Fatal("the kept claim could not remove the entry later")
		}
	})
}

// A recorded directory is not an entry. The directory may already have been on
// PATH, or the write may have failed, and there is then nothing to remove.
func TestPathManagedRequiresAnEntryPAIRWrote(t *testing.T) {
	ex, _ := pathInstallExecutor(t, "ollama", "process")
	file := ex.pathReceiptFile("ollama")
	if err := savePathReceipt(file, &pathReceipt{Dir: t.TempDir(), Installed: true}); err != nil {
		t.Fatal(err)
	}
	if ex.pathManaged("ollama") {
		t.Error("a directory PAIR did not write an entry for counts as managed")
	}
	if err := savePathReceipt(file, &pathReceipt{Dir: "d", WindowsEntry: "d"}); err != nil {
		t.Fatal(err)
	}
	if !ex.pathManaged("ollama") {
		t.Error("an entry PAIR wrote does not count as managed")
	}
}

// The state a client renders its uninstall prompt from has to be the one after
// the PATH step, not before it.
func TestLifecycleStateCarriesTheFinalPathManaged(t *testing.T) {
	// The ops below run synchronously, so every emit has landed before a read.
	captureStates := func(ex *Executor) *[]EngineStatus {
		var mu sync.Mutex
		var states []EngineStatus
		ex.emit = func(method string, params any) {
			if status, ok := params.(EngineStatus); ok && method == "engine:state-changed" {
				mu.Lock()
				states = append(states, status)
				mu.Unlock()
			}
		}
		return &states
	}
	t.Run("uninstall", func(t *testing.T) {
		ex, _, _ := pathLifecycleExecutor(t, "ollama", "process")
		if err := ex.Install(context.Background(), "ollama", true); err != nil {
			t.Fatal(err)
		}
		states := captureStates(ex)
		if err := ex.Uninstall(context.Background(), "ollama", true); err != nil {
			t.Fatal(err)
		}
		if len(*states) == 0 {
			t.Fatal("uninstall emitted no state")
		}
		if last := (*states)[len(*states)-1]; last.PathManaged {
			t.Error("the last state after uninstall still claims the removed entry")
		}
	})
	t.Run("consented install of an engine already present", func(t *testing.T) {
		ex, _, _ := pathLifecycleExecutor(t, "ollama", "process")
		if err := ex.Install(context.Background(), "ollama", false); err != nil {
			t.Fatal(err)
		}
		states := captureStates(ex)
		if err := ex.Install(context.Background(), "ollama", true); err != nil {
			t.Fatal(err)
		}
		if len(*states) == 0 || !(*states)[len(*states)-1].PathManaged {
			t.Errorf("states after adopting the engine onto PATH = %+v, want one reporting the entry", *states)
		}
	})
}

// A consented uninstall removes the whole record, including a declined
// install's installed flag, so a later vendor install is not mistaken for PAIR's.
func TestConsentedUninstallDropsAnInstalledOnlyRecord(t *testing.T) {
	ex, _, _ := pathLifecycleExecutor(t, "ollama", "process")
	if err := ex.Install(context.Background(), "ollama", false); err != nil {
		t.Fatal(err)
	}
	if err := ex.Uninstall(context.Background(), "ollama", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ex.pathReceiptFile("ollama")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the installed flag outlived the engine: %v", err)
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
	if last.Status.PathManaged {
		t.Error("the result frame reported this user's PATH state to the peer")
	}
	// Only the installed flag, so a later consented local install can adopt it.
	receipt, err := loadPathReceipt(ex.pathReceiptFile("lmstudio"))
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.Installed || receipt.Dir != "" {
		t.Fatalf("receipt after a remote install = %+v, want only the installed flag", receipt)
	}
}

func TestPeerEngineListHidesPathManaged(t *testing.T) {
	ex, _ := pathInstallExecutor(t, "ollama", "process")
	if err := savePathReceipt(ex.pathReceiptFile("ollama"), &pathReceipt{Dir: "d", WindowsEntry: "d"}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	(&controlServer{exec: ex}).handleEngines(rec, httptest.NewRequest(http.MethodGet, controlEnginesPath, nil))
	var body struct {
		Engines []EngineStatus `json:"engines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Engines) == 0 {
		t.Fatal("no engines served")
	}
	for _, e := range body.Engines {
		if e.PathManaged {
			t.Errorf("%s: served this user's PATH state to a peer", e.Engine)
		}
	}
}
