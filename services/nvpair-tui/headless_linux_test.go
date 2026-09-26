// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A re-executed test binary becomes the live owner that
// TestHeadlessNativeAbruptOwnerDeathRecoversOnlyMatchingSocket kills.
func init() {
	if os.Getenv("NVPAIR_TUI_HEADLESS_TEST_SERVER") != "1" {
		return
	}
	listener, _, err := listenHeadless()
	if err != nil {
		os.Exit(2)
	}
	_, _ = os.Stdout.WriteString("private-owner-ready\n")
	for {
		conn, err := listener.Accept()
		if err != nil {
			os.Exit(0)
		}
		_ = conn.Close()
	}
}

func headlessTestRuntime(t *testing.T) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pair-h-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", dir)
}

func TestHeadlessNativeLifetimeNeverSilentlyFallsBack(t *testing.T) {
	if _, err := headlessLifetime("persistent", false); err == nil {
		t.Fatal("persistent request silently fell back")
	}
	if got, err := headlessLifetime("session", false); err != nil || got != "session" {
		t.Fatalf("explicit session=%s %v", got, err)
	}
	if got, err := headlessLifetime("session", true); err != nil || got != "persistent" {
		t.Fatalf("existing linger was hidden=%s %v", got, err)
	}
	if _, err := headlessLifetime("automatic", true); err == nil {
		t.Fatal("unknown lifetime accepted")
	}
}

func TestHeadlessNativeSocketOwnershipConflictAndExactCleanup(t *testing.T) {
	headlessTestRuntime(t)
	listener, cleanup, err := listenHeadless()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	path, _ := headlessSocketPath()
	if _, _, err := listenHeadless(); err == nil {
		t.Fatal("existing socket was replaced")
	}
	client, err := dialHeadless(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	peer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if !headlessSameUser(peer) {
		t.Fatal("same-account Unix peer not admitted")
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("socket mode=%v %v", st, err)
	}
	cleanup()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("owned socket was not cleaned")
	}
}

func TestHeadlessNativeCleanupPreservesReplacementFile(t *testing.T) {
	headlessTestRuntime(t)
	listener, cleanup, err := listenHeadless()
	if err != nil {
		t.Fatal(err)
	}
	path, _ := headlessSocketPath()
	_ = listener.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("foreign-replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	cleanup()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "foreign-replacement" {
		t.Fatal("cleanup removed a replacement endpoint")
	}
}

func TestHeadlessNativeRuntimeDirectoryMustBePrivate(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_RUNTIME_DIR", dir)
	if _, err := headlessSocketPath(); err == nil {
		t.Fatal("public runtime directory accepted")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "nvidia-pair")); err != nil {
		t.Fatal(err)
	}
	if _, err := headlessSocketPath(); err == nil {
		t.Fatal("symlinked runtime directory accepted")
	}
}

func TestHeadlessNativeControlExchangeAndCancellation(t *testing.T) {
	headlessTestRuntime(t)
	listener, cleanup, err := listenHeadless()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var ready atomic.Bool
	ready.Store(true)
	client := &headlessFakeCaller{result: json.RawMessage(`{"nodeUuid":"real-fixture-identity"}`)}
	go func() {
		peer, err := listener.Accept()
		if err == nil {
			serveHeadlessConnection(ctx, peer, client, &ready)
		}
	}()
	var output strings.Builder
	if err := runHeadlessControl(ctx, strings.NewReader(`{"method":"cluster:get-node-id"}`), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "real-fixture-identity") {
		t.Fatalf("control response=%s", output.String())
	}
	peerReady := make(chan struct{})
	done := make(chan struct{})
	go func() {
		peer, err := listener.Accept()
		if err != nil {
			close(done)
			return
		}
		close(peerReady)
		serveHeadlessConnection(ctx, peer, client, &ready)
		close(done)
	}()
	idle, err := net.Dial("unix", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	<-peerReady
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation left an idle control read blocked")
	}
}

func TestHeadlessNativeUnitEscapesPathsAndKeepsOwnedLifetime(t *testing.T) {
	unit, err := headlessUnitText("/opt/PAIR %/$build/nvpair-tui", "/opt/PAIR %/$build/nvpair-ui-broker", "/home/operator/config %/$literal")
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{`ExecStart="/opt/PAIR %%/$$build/nvpair-tui" --headless --broker-path "/opt/PAIR %%/$$build/nvpair-ui-broker"`, "WorkingDirectory=/opt/PAIR %%/$build\n", `Environment="XDG_CONFIG_HOME=/home/operator/config %%/$literal"`, "TimeoutStopSec=145", "KillMode=mixed", "WantedBy=default.target"} {
		if !strings.Contains(unit, part) {
			t.Fatalf("missing escaped/lifetime directive %s in %s", part, unit)
		}
	}
	if _, err := headlessUnitText("/opt/PAIR\nother", "/opt/broker", "/home/config"); err == nil {
		t.Fatal("newline path accepted")
	}
}

func TestHeadlessNativeWorkingDirectoryKeepsLiteralCharacters(t *testing.T) {
	for _, value := range []string{`/opt/PAIR %/$build/back\slash`, `/opt/with"quote`, "/opt/final space ", "/opt/final-backslash\\"} {
		got, err := systemdWorkingDirectory(value)
		if err != nil {
			t.Fatal(err)
		}
		want := strings.ReplaceAll(value, "%", "%%")
		if strings.HasSuffix(value, "\\") || strings.HasSuffix(value, " ") {
			want += "/"
		}
		if got != want {
			t.Fatalf("literal path changed: got %q want %q", got, want)
		}
	}
	for _, value := range []string{"relative", "/opt/line\nbreak", "/opt/tab\tpath", "/opt/null\x00path"} {
		if _, err := systemdWorkingDirectory(value); err == nil {
			t.Fatal("unsafe working directory accepted")
		}
	}
}

func TestHeadlessNativeSystemdAnalyzeValidatesGeneratedUnit(t *testing.T) {
	analyzer, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("native systemd-analyze is required for the unit-parser gate")
	}
	root := t.TempDir()
	// Use the supported installed-bundle shape for executable names. Native
	// systemd rejected the earlier dollar/backslash executable fixture before
	// it reached WorkingDirectory; that is not evidence about this directive.
	bin := filepath.Join(root, "PAIR space %", "bin")
	config := filepath.Join(root, "config with spaces %")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(config, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"nvpair-tui", "nvpair-ui-broker"} {
		if err := os.Symlink("/bin/true", filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	unit, err := headlessUnitText(filepath.Join(bin, "nvpair-tui"), filepath.Join(bin, "nvpair-ui-broker"), config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, headlessUnit)
	verify := func(content string) (string, error) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Offline parser validation only: no service-manager install/start, no
		// generators, manual-page helpers, network listeners or rank processes.
		return boundedHeadlessCommand(ctx, analyzer, "--user", "--generators=no", "--man=no", "verify", path)
	}
	if output, err := verify(unit); err != nil {
		t.Fatalf("native systemd rejected generated unit: %v\n%s", err, output)
	}
	directory, _ := systemdWorkingDirectory(bin)
	for _, name := range []string{`cwd % $literal back\slash`, `cwd with"quote`, "cwd final space ", "cwd final-backslash\\"} {
		working := filepath.Join(root, name)
		if err := os.MkdirAll(working, 0700); err != nil {
			t.Fatal(err)
		}
		value, err := systemdWorkingDirectory(working)
		if err != nil {
			t.Fatal(err)
		}
		candidate := strings.Replace(unit, "WorkingDirectory="+directory+"\n", "WorkingDirectory="+value+"\n", 1)
		if output, err := verify(candidate); err != nil {
			t.Fatalf("native systemd rejected literal working directory %q: %v\n%s", working, err, output)
		}
	}
	quoted, _ := systemdQuoted(bin)
	broken := strings.Replace(unit, "WorkingDirectory="+directory+"\n", "WorkingDirectory="+quoted+"\n", 1)
	if output, err := verify(broken); err == nil || !strings.Contains(output, "WorkingDirectory") {
		t.Fatalf("native verifier did not reject prior quoted-path defect: %v\n%s", err, output)
	}
}

func TestHeadlessNativeUnitInstallIsAtomicAndNeverClobbers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, headlessUnit)
	if err := installHeadlessUnit(path, "complete-owned-unit"); err != nil {
		t.Fatal(err)
	}
	if err := installHeadlessUnit(path, "replacement"); err == nil {
		t.Fatal("unit was overwritten")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "complete-owned-unit" {
		t.Fatal("installed unit changed")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatal("temporary unit was not cleaned")
	}
}

func TestHeadlessNativeServiceCommandOutputIsActuallyBounded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	started := time.Now()
	output, err := boundedHeadlessCommand(ctx, "/bin/sh", "-c", "yes output-limit-fixture")
	if err == nil || len(output) > 8192 || time.Since(started) > 2*time.Second {
		t.Fatalf("unbounded service command len=%d err=%v", len(output), err)
	}
}

func TestHeadlessNativePortConflictIsPassiveAndConservative(t *testing.T) {
	if err := checkHeadlessPorts("0: 00000000:37F2 00000000:0000 0A\n"); err == nil {
		t.Fatal("existing node-info port14322 was ignored")
	}
	if err := checkHeadlessPorts("0: 00000000:0016 00000000:0000 0A\n"); err != nil {
		t.Fatalf("foreign SSH service blocked PAIR: %v", err)
	}
}

func TestHeadlessNativeAbruptOwnerDeathRecoversOnlyMatchingSocket(t *testing.T) {
	headlessTestRuntime(t)
	command := exec.Command(os.Args[0], "-test.run=^$")
	command.Env = append(os.Environ(), "NVPAIR_TUI_HEADLESS_TEST_SERVER=1")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill() })
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "private-owner-ready\n" {
			t.Fatalf("child did not acquire runtime: %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("child owner did not start")
	}
	if _, _, err := listenHeadless(); err == nil {
		t.Fatal("live child owner was replaced")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	listener, cleanup, err := listenHeadless()
	if err != nil {
		t.Fatalf("receipt-bound stale socket did not recover: %v", err)
	}
	defer cleanup()
	_ = listener.Close()
}

func TestHeadlessNativeMismatchedSocketReceiptIsNeverReclaimed(t *testing.T) {
	headlessTestRuntime(t)
	path, err := headlessSocketPath()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	_ = listener.Close()
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(headlessRuntimeOwner{PID: os.Getpid(), StartTicks: "different-start", Device: 1, Inode: 1})
	if err := os.WriteFile(headlessOwnerPath(path), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := listenHeadless(); err == nil {
		t.Fatal("mismatched socket was reclaimed")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatal("mismatched socket was removed")
	}
}

func TestHeadlessNativeMissingUnitFileNeedsConfirmedManagerAbsence(t *testing.T) {
	for _, test := range []struct {
		name, output string
		err          error
		absent       bool
	}{
		{"active", "LoadState=loaded\nActiveState=active\nMainPID=123\nFragmentPath=/missing/unit\nDropInPaths=", nil, false},
		{"not-found", "LoadState=not-found\nActiveState=inactive\nMainPID=0\nFragmentPath=\nDropInPaths=", nil, true},
		{"unavailable", "", errors.New("user bus unavailable"), false},
		{"foreign-override", "LoadState=loaded\nActiveState=inactive\nMainPID=0\nFragmentPath=/foreign/unit\nDropInPaths=/foreign/override", nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			err := confirmHeadlessUnitAbsent(context.Background(), func(_ context.Context, args ...string) (string, error) {
				calls++
				if len(args) != 7 || args[0] != "show" || args[1] != headlessUnit {
					t.Fatalf("unexpected manager command: %v", args)
				}
				return test.output, test.err
			})
			if (err == nil) != test.absent || calls != 1 {
				t.Fatalf("absence=%v expected=%v calls=%d", err == nil, test.absent, calls)
			}
		})
	}
}
