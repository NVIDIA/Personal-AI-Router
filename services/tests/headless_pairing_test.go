// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Cross-process gate for pairing two machines with no keyboard: the flow an
// operator runs over SSH on a headless GPU box.
//
// Two real nvpair-tui processes come up, each serving its own control socket,
// each owning a real nvpair-cluster-manager. `nvpair-tui invite` on one prints
// a PIN, `nvpair-tui accept --pin` on the other returns paired, and `members`
// on both then lists the pair. Nothing about the pairing is simulated: it is
// the real PIN-authenticated EAP-NOOB exchange between two real managers.
//
// Two things are substituted, both for the same reason — every broker-owned
// port is a compiled-in constant, so only one broker can exist per machine and
// this test needs two nodes:
//
//   - each TUI's broker is the stubbroker fixture, which spawns a real cluster
//     manager on an ephemeral port and relays cluster:* / nodes:* verbatim;
//   - each TUI runs under `script`, because it is a full-screen program that
//     will not start without a terminal.
//
// The TUI process, its control socket, its subcommands, the cluster managers
// and the pairing are all real.
//
// Unix only: it drives nvpair-tui under script(1) and reaps it by process
// group, neither of which Windows has. SysProcAttr.Setpgid does not even
// compile there, hence the build tag rather than a runtime skip.

//go:build !windows

package tests

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// tuiNode is one headless machine: a real nvpair-tui under a pty, its control
// socket, and the cluster manager its stub broker owns.
type tuiNode struct {
	t      *testing.T
	name   string
	socket string
	port   int
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	log    *os.File
}

// shortDir gives a directory whose path leaves room for a socket name. A macOS
// t.TempDir() sits under /var/folders/... and, with a socket name appended,
// passes the 104-byte sun_path limit — a real constraint of this endpoint,
// which is why the control socket cannot simply live in t.TempDir().
func shortDir(t *testing.T, label string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "nvpair-"+label)
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// ptyCommand wraps argv so it runs with a controlling terminal. nvpair-tui is
// a Bubble Tea program: with no tty it fails at startup with "could not open a
// new TTY", so there is no way to exercise the real binary without one.
func ptyCommand(t *testing.T, argv []string) *exec.Cmd {
	t.Helper()
	if _, err := exec.LookPath("script"); err != nil {
		t.Skipf("script(1) is not on PATH, so no pty can be allocated for nvpair-tui: %v", err)
	}
	switch runtime.GOOS {
	case "darwin":
		// BSD script: script [-q] file command [args...]
		return exec.Command("script", append([]string{"-q", "/dev/null"}, argv...)...)
	default:
		// util-linux script (Linux and the other unixes that ship it) takes
		// the command as one string.
		return exec.Command("script", "-qec", strings.Join(argv, " "), "/dev/null")
	}
}

// startTUINode brings up one headless machine and returns once its control
// socket answers.
func startTUINode(t *testing.T, name string) *tuiNode {
	t.Helper()
	base := shortDir(t, name)
	node := &tuiNode{
		t:      t,
		name:   name,
		socket: filepath.Join(base, "tui.sock"),
		port:   freePort(t),
	}

	brokerArgs := strings.Join([]string{
		stubBrokerBin,
		"--cluster-manager-path", clusterMgrBin,
		"--config-dir", filepath.Join(base, "cluster"),
		"--port", strconv.Itoa(node.port),
	}, " ")
	// nvpair-tui resolves its broker next to itself unless told otherwise, and
	// --broker-path takes a single executable, so the fixture's arguments ride
	// in a one-line wrapper script.
	wrapper := filepath.Join(base, "broker.sh")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+brokerArgs+" \"$@\"\n"), 0o700); err != nil {
		t.Fatalf("write the broker wrapper: %v", err)
	}

	node.cmd = ptyCommand(t, []string{
		tuiBin,
		"--broker-path", wrapper,
		"--control-socket", node.socket,
		"--log-level", "debug",
	})
	// A fresh HOME and friends: the per-user data directory on a developer
	// machine belongs to whatever PAIR is already installed there.
	node.cmd.Env = append(os.Environ(),
		"HOME="+base, "XDG_CONFIG_HOME="+base, "XDG_RUNTIME_DIR="+base,
		"APPDATA="+base, "LOCALAPPDATA="+base,
	)
	// Its own process group, so cleanup can take the pty helper and the TUI
	// down together. Killing `script` alone leaves the TUI running, holding
	// its cluster manager and its socket into the next test.
	node.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	logFile, err := os.Create(filepath.Join(base, name+".term"))
	if err != nil {
		t.Fatalf("create the terminal capture: %v", err)
	}
	node.log = logFile
	node.cmd.Stdout = logFile
	node.cmd.Stderr = logFile

	stdin, err := node.cmd.StdinPipe()
	if err != nil {
		t.Fatalf("pty stdin: %v", err)
	}
	node.stdin = stdin
	if err := node.cmd.Start(); err != nil {
		t.Fatalf("start nvpair-tui under a pty: %v", err)
	}
	t.Cleanup(node.stop)

	node.awaitControlSocket()
	return node
}

// stop quits the TUI the way a user does, then makes sure nothing survives.
func (n *tuiNode) stop() {
	// `q` is the TUI's quit key; quitting shuts the broker down cleanly, which
	// takes the cluster manager with it.
	_, _ = n.stdin.Write([]byte("q"))
	done := make(chan struct{})
	go func() { _ = n.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(25 * time.Second):
	}
	if n.cmd.Process != nil {
		// The whole group: `script`, the TUI, its broker and the manager.
		_ = syscall.Kill(-n.cmd.Process.Pid, syscall.SIGKILL)
	}
	if n.t.Failed() && n.log != nil {
		if captured, err := os.ReadFile(n.log.Name()); err == nil {
			n.t.Logf("%s terminal output:\n%s", n.name, strings.ReplaceAll(string(captured), "\r", "\n"))
		}
	}
	_ = n.log.Close()
}

// awaitControlSocket blocks until the node's socket answers a ping with a
// ready broker, so nothing is asked of a TUI that is still starting.
func (n *tuiNode) awaitControlSocket() {
	n.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		out, err := n.run("pending", "--json")
		if err == nil && strings.Contains(out, "invites") {
			return
		}
		last = out
		time.Sleep(250 * time.Millisecond)
	}
	n.t.Fatalf("%s never served its control socket at %s; last answer = %q", n.name, n.socket, last)
}

// run invokes one nvpair-tui subcommand against this node and returns its
// combined output. A non-zero exit comes back as an error carrying the code.
func (n *tuiNode) run(args ...string) (string, error) {
	return n.runWithEnv(nil, args...)
}

func (n *tuiNode) runWithEnv(extraEnv []string, args ...string) (string, error) {
	cmd := exec.Command(tuiBin, append(args, "--control-socket", n.socket)...)
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// mustRun fails the test when the subcommand exits non-zero.
func (n *tuiNode) mustRun(args ...string) string {
	n.t.Helper()
	out, err := n.run(args...)
	if err != nil {
		n.t.Fatalf("%s: nvpair-tui %s failed (%v):\n%s", n.name, strings.Join(args, " "), err, out)
	}
	return out
}

// exitCode digs the process exit status out of a run error.
func exitCode(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !asExitError(err, &exitErr) {
		t.Fatalf("expected a process exit status, got %T (%v)", err, err)
	}
	return exitErr.ExitCode()
}

func asExitError(err error, target **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*target = e
		return true
	}
	return false
}

func TestHeadlessPairingOverTheControlSocket(t *testing.T) {
	inviter := startTUINode(t, "inviter")
	joiner := startTUINode(t, "joiner")

	t.Run("nothing is pending on a machine nobody has invited", func(t *testing.T) {
		out := inviter.mustRun("pending", "--json")
		if strings.TrimSpace(out) != `{"invites":[]}` {
			t.Errorf("pending = %q, want an empty list", out)
		}
	})

	t.Run("an unclustered machine reports no cluster", func(t *testing.T) {
		var membership struct {
			ClusterID string           `json:"clusterId"`
			Members   []map[string]any `json:"members"`
		}
		if err := json.Unmarshal([]byte(inviter.mustRun("members", "--json")), &membership); err != nil {
			t.Fatalf("decode members: %v", err)
		}
		if membership.ClusterID != "" {
			t.Errorf("clusterId = %q, want empty before any pairing", membership.ClusterID)
		}
	})

	// The invite. Its PIN is what an operator reads to the other machine.
	var invite struct {
		InviteID string `json:"inviteId"`
		State    string `json:"state"`
		Pin      string `json:"pin"`
	}
	t.Run("invite prints a PIN", func(t *testing.T) {
		raw := inviter.mustRun("invite", "127.0.0.1", "--port", strconv.Itoa(joiner.port), "--json")
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &invite); err != nil {
			t.Fatalf("decode invite (%q): %v", raw, err)
		}
		if invite.State != "pending" {
			t.Fatalf("state = %q, want pending", invite.State)
		}
		if len(invite.Pin) != 6 {
			t.Fatalf("pin = %q, want six digits", invite.Pin)
		}

		// The human form has to carry the PIN too — it is the whole point of
		// the command.
		human, err := inviter.run("invite", "127.0.0.1", "--port", strconv.Itoa(joiner.port))
		if err == nil && !strings.Contains(human, "PIN") {
			t.Errorf("the human invite line does not show a PIN:\n%s", human)
		}
	})

	t.Run("the invitation is waiting on the other machine", func(t *testing.T) {
		out := awaitPending(t, joiner)
		if !strings.Contains(out, "inviteId") {
			t.Fatalf("pending = %q, want the inbound invite", out)
		}
		if strings.Contains(out, invite.Pin) {
			t.Errorf("pair:pending disclosed the PIN:\n%s", out)
		}
		// The human listing names the inviter and the invite.
		listed := joiner.mustRun("pending")
		if !strings.Contains(listed, "ago") {
			t.Errorf("the pending listing has no age column:\n%s", listed)
		}
	})

	t.Run("a wrong PIN is refused with exit 1", func(t *testing.T) {
		out, err := joiner.runWithEnv([]string{"NVPAIR_PIN=000000"}, "accept")
		if code := exitCode(t, err); code != 1 {
			t.Fatalf("exit = %d, want 1 for a wrong PIN:\n%s", code, out)
		}
		if strings.Contains(out, "000000") {
			t.Errorf("the rejected PIN was echoed back:\n%s", out)
		}
	})

	t.Run("the right PIN pairs the two machines", func(t *testing.T) {
		// A wrong PIN terminates that invite on both sides, so the inviter
		// sends a fresh one — which is what an operator does too.
		raw := inviter.mustRun("invite", "127.0.0.1", "--port", strconv.Itoa(joiner.port), "--json")
		if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &invite); err != nil {
			t.Fatalf("decode the replacement invite: %v", err)
		}
		awaitPending(t, joiner)

		// The PIN travels in the environment, which is the form a script uses:
		// an argument would be visible in ps to every user on the machine.
		out, err := joiner.runWithEnv([]string{"NVPAIR_PIN=" + invite.Pin}, "accept")
		if err != nil {
			t.Fatalf("accept failed (%v):\n%s", err, out)
		}
		if !strings.Contains(out, "paired") {
			t.Errorf("accept said %q, want it to report the pairing", out)
		}
	})

	t.Run("both machines list the pair", func(t *testing.T) {
		inviterCluster := awaitClustered(t, inviter)
		joinerCluster := awaitClustered(t, joiner)
		if inviterCluster != joinerCluster {
			t.Errorf("the two machines joined different clusters: %q vs %q", inviterCluster, joinerCluster)
		}
		for _, node := range []*tuiNode{inviter, joiner} {
			listed := node.mustRun("members")
			if !strings.Contains(listed, inviterCluster) {
				t.Errorf("%s members does not name the cluster:\n%s", node.name, listed)
			}
			if strings.Count(strings.TrimSpace(listed), "\n") < 2 {
				t.Errorf("%s members lists fewer than two machines:\n%s", node.name, listed)
			}
		}
	})

	t.Run("the answered invite is no longer pending", func(t *testing.T) {
		out := joiner.mustRun("pending", "--json")
		if strings.TrimSpace(out) != `{"invites":[]}` {
			t.Errorf("pending = %q, want the answered invite gone", out)
		}
	})

	t.Run("accepting with nothing pending exits 2", func(t *testing.T) {
		out, err := joiner.runWithEnv([]string{"NVPAIR_PIN=123456"}, "accept")
		if code := exitCode(t, err); code != 2 {
			t.Fatalf("exit = %d, want 2:\n%s", code, out)
		}
		if !strings.Contains(out, "no invite is pending") {
			t.Errorf("output = %q, want it to say nothing is waiting", out)
		}
	})

	t.Run("a subcommand with no running instance exits 2", func(t *testing.T) {
		cmd := exec.Command(tuiBin, "members", "--control-socket", filepath.Join(shortDir(t, "empty"), "tui.sock"))
		out, err := cmd.CombinedOutput()
		if code := exitCode(t, err); code != 2 {
			t.Fatalf("exit = %d, want 2:\n%s", code, out)
		}
		if !strings.Contains(string(out), "no nvpair-tui is listening") {
			t.Errorf("output = %q, want it to say nothing is listening", out)
		}
	})
}

func TestDeclineFromTheControlSocket(t *testing.T) {
	inviter := startTUINode(t, "decl-inviter")
	joiner := startTUINode(t, "decl-joiner")

	inviter.mustRun("invite", "127.0.0.1", "--port", strconv.Itoa(joiner.port), "--json")
	awaitPending(t, joiner)

	out := joiner.mustRun("decline")
	if !strings.Contains(out, "declined") {
		t.Errorf("decline said %q, want it to report the refusal", out)
	}
	if pending := joiner.mustRun("pending", "--json"); strings.TrimSpace(pending) != `{"invites":[]}` {
		t.Errorf("pending = %q, want the declined invite gone", pending)
	}
}

// awaitPending blocks until an invitation is waiting on the node, and returns
// the raw pair:pending answer.
func awaitPending(t *testing.T, node *tuiNode) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = node.mustRun("pending", "--json")
		if !strings.Contains(last, `"invites":[]`) {
			return last
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s never saw an inbound invitation; last pending = %q", node.name, last)
	return ""
}

// awaitClustered blocks until the node reports a cluster id and returns it.
// Membership is durable the moment the pairing completes, but each side
// records it on its own schedule.
func awaitClustered(t *testing.T, node *tuiNode) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		last = node.mustRun("members", "--json")
		var membership struct {
			ClusterID string `json:"clusterId"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(last)), &membership); err == nil && membership.ClusterID != "" {
			return membership.ClusterID
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s never reported a cluster; last members = %q", node.name, last)
	return ""
}
